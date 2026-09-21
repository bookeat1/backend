package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/restaurants"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testBookingRulesDefaults are the platform defaults guestpush tests resolve
// the booking-rules footer against — arbitrary but distinct values, so a test
// asserting on the rendered text cannot pass by coincidence with a zero value.
var testBookingRulesDefaults = restaurants.BookingRulesDefaults{
	HoldMinutes:                    15,
	LateArrivalText:                "Опаздываете — позвоните в заведение.",
	DefaultFreeCancelWindowMinutes: 120,
}

// guestHarness wires a GuestPushNotifier over in-memory fakes.
type guestHarness struct {
	n       *GuestPushNotifier
	tokens  *fakeDeviceTokens
	prefs   *fakeGuestPrefs
	sender  *recordingMobileSender
	deliv   *fakeDeliveries
	tickets *fakePushTickets
}

func newGuestHarness(t *testing.T, tokens ...domain.DevicePushToken) *guestHarness {
	t.Helper()
	h := &guestHarness{
		tokens:  newFakeDeviceTokens(tokens...),
		prefs:   newFakeGuestPrefs(),
		sender:  newRecordingMobileSender(),
		deliv:   newFakeDeliveries(),
		tickets: newFakePushTickets(),
	}
	h.n = NewGuestPushNotifier(h.tokens, h.deliv, h.tickets,
		NewGuestNotificationGate(h.prefs), fakeVenues{name: "Ocean Basket"},
		fakeVenues{name: "Ocean Basket"}, testBookingRulesDefaults,
		h.sender.send, true, discardLog())
	return h
}

// guestEvent builds a decoded Event for a guest's own booking.
func guestEvent(userID uuid.UUID, t domain.BookingEventType) Event {
	return Event{
		OutboxEventID: uuid.New(),
		BookingID:     uuid.New(),
		RestaurantID:  uuid.New(),
		Type:          t,
		GuestName:     "Дамир",
		Guests:        4,
		StartsAt:      time.Date(2026, 8, 1, 19, 30, 0, 0, time.UTC),
		GuestUserID:   &userID,
	}
}

func guestToken(userID uuid.UUID) domain.DevicePushToken {
	return domain.DevicePushToken{
		ID: uuid.New(), UserID: userID, Token: "ExponentPushToken[" + uuid.NewString() + "]",
		Platform: domain.PlatformIOS, IsActive: true,
	}
}

// THE GATE. A guest who switched notifications off gets nothing, and the event
// is still consumed (nil error) so an opt-out can never jam the outbox.
//
// This test fails if the gate.Allows call is removed from Notify.
func TestGuestPushRespectsOptOut(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	h.prefs.set(domain.NotificationPreference{UserID: uid, NotificationsEnabled: false})

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := h.sender.count(); got != 0 {
		t.Fatalf("sent %d pushes to an opted-out guest, want 0", got)
	}
}

// The per-channel switch is honoured too: master on, push off → nothing.
func TestGuestPushRespectsPushChannelOptOut(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	h.prefs.set(domain.NotificationPreference{
		UserID: uid, NotificationsEnabled: true, PushEnabled: false, EmailEnabled: true,
	})

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := h.sender.count(); got != 0 {
		t.Fatalf("sent %d pushes with push disabled, want 0", got)
	}
}

// A guest who never touched their settings is notified (the default is all-on),
// on every device they registered.
func TestGuestPushFansOutToAllDevices(t *testing.T) {
	uid := uuid.New()
	a, b := guestToken(uid), guestToken(uid)
	other := guestToken(uuid.New()) // another guest — must never be touched
	h := newGuestHarness(t, a, b, other)

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	sent := h.sender.tokens()
	if len(sent) != 2 {
		t.Fatalf("sent to %d devices, want 2 (the guest's own)", len(sent))
	}
	for _, tok := range sent {
		if tok == other.Token {
			t.Fatal("pushed to another guest's device")
		}
	}
}

// A gate read failure must NOT be treated as "allowed": the event is returned as
// an error so the dispatcher retries, rather than notifying a guest who may have
// opted out.
func TestGuestPushRetriesWhenPreferencesUnreadable(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	h.prefs.err = errors.New("db down")

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err == nil {
		t.Fatal("want an error so the event is retried, got nil")
	}
	if got := h.sender.count(); got != 0 {
		t.Fatalf("sent %d pushes despite an unreadable preference, want 0", got)
	}
}

// A cancellation the GUEST performed themselves is not echoed back at them; a
// venue-side cancellation is.
func TestGuestPushSkipsSelfCancellationButSendsVenueCancellation(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))

	self := guestEvent(uid, domain.EventBookingCancelled)
	self.CancelledBy = domain.CancelledByGuest
	if err := h.n.Notify(context.Background(), self); err != nil {
		t.Fatalf("notify self-cancel: %v", err)
	}
	if got := h.sender.count(); got != 0 {
		t.Fatalf("echoed the guest's own cancellation (%d pushes), want 0", got)
	}

	byVenue := guestEvent(uid, domain.EventBookingCancelled)
	byVenue.CancelledBy = domain.CancelledByRestaurant
	if err := h.n.Notify(context.Background(), byVenue); err != nil {
		t.Fatalf("notify venue cancel: %v", err)
	}
	if got := h.sender.count(); got != 1 {
		t.Fatalf("venue cancellation produced %d pushes, want 1", got)
	}
}

// A booking with no account (phone / admin-entered) has nobody to notify, and
// that is not an error.
func TestGuestPushSkipsAccountlessBooking(t *testing.T) {
	h := newGuestHarness(t)
	e := guestEvent(uuid.New(), domain.EventBookingConfirmed)
	e.GuestUserID = nil

	if err := h.n.Notify(context.Background(), e); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := h.sender.count(); got != 0 {
		t.Fatalf("sent %d pushes for an account-less booking, want 0", got)
	}
}

// Redelivery (a sibling channel failed and the event stayed unpublished) must
// not double-push a device that already got it.
func TestGuestPushDedupesRedelivery(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	e := guestEvent(uid, domain.EventBookingConfirmed)

	for i := 0; i < 3; i++ {
		if err := h.n.Notify(context.Background(), e); err != nil {
			t.Fatalf("notify #%d: %v", i, err)
		}
	}
	if got := h.sender.count(); got != 1 {
		t.Fatalf("three deliveries of the same event produced %d pushes, want 1", got)
	}
}

// A token the provider reports as gone is deactivated, and the event still
// drains (a dead device is not a retryable failure).
func TestGuestPushDeactivatesGoneDevice(t *testing.T) {
	uid := uuid.New()
	tok := guestToken(uid)
	h := newGuestHarness(t, tok)
	h.sender.verdict[tok.Token] = MobilePushDeviceGone

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if h.tokens.isActive(tok.ID) {
		t.Fatal("a gone device token is still active")
	}
}

// Without a configured provider the channel is a clean no-op: nil error, no
// send, no repository read — so the dispatcher still drains the outbox.
func TestGuestPushDisabledIsNoop(t *testing.T) {
	uid := uuid.New()
	sender := newRecordingMobileSender()
	n := NewGuestPushNotifier(newFakeDeviceTokens(guestToken(uid)), newFakeDeliveries(),
		newFakePushTickets(), NewGuestNotificationGate(newFakeGuestPrefs()),
		fakeVenues{name: "Ocean Basket"}, fakeVenues{name: "Ocean Basket"}, testBookingRulesDefaults,
		sender.send, false, discardLog())

	if err := n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := sender.count(); got != 0 {
		t.Fatalf("a disabled channel sent %d pushes", got)
	}
}

// The channel reacts to exactly the three guest-relevant events, and ignores the
// staff ones (booking.created is the venue's alert, not the guest's).
func TestGuestPushInterestedEvents(t *testing.T) {
	n := &GuestPushNotifier{}
	want := map[domain.BookingEventType]bool{
		domain.EventBookingConfirmed: true,
		domain.EventBookingCancelled: true,
		domain.EventBookingReminder:  true,
		domain.EventBookingCreated:   false,
		domain.EventBookingNoShow:    false,
		domain.EventBookingCompleted: false,
		domain.EventBookingArrived:   false,
		domain.EventBookingEscalated: false,
	}
	for et, w := range want {
		if got := n.Interested(et); got != w {
			t.Errorf("Interested(%s) = %v, want %v", et, got, w)
		}
	}
}

// The rendered text carries only what the guest already knows, and never their
// phone number or the device token.
func TestGuestMessageContentIsMinimal(t *testing.T) {
	e := guestEvent(uuid.New(), domain.EventBookingReminder)
	msg, ok := buildGuestMessage(e, "Ocean Basket", "")
	if !ok {
		t.Fatal("no template for booking.reminder")
	}
	if !strings.Contains(msg.Body, "Ocean Basket") || !strings.Contains(msg.Body, "4 чел.") {
		t.Fatalf("body %q must name the venue and the party size", msg.Body)
	}
	for _, forbidden := range []string{"+7", "77071234567", "ExponentPushToken"} {
		if strings.Contains(msg.Title+msg.Body, forbidden) {
			t.Fatalf("message leaks %q: %q / %q", forbidden, msg.Title, msg.Body)
		}
	}
	if msg.Data["booking_id"] != e.BookingID.String() {
		t.Fatalf("data must deep-link to the booking, got %v", msg.Data)
	}
	// Criterion 21: a booking push explicitly names the "bookings" Android
	// channel — it must never land in "offers", the channel push campaigns use.
	if msg.ChannelID != "bookings" {
		t.Fatalf("channel id = %q, want %q", msg.ChannelID, "bookings")
	}
}

// The outbox payload contract: the producer's user_id / cancelled_by reach the
// decoded Event, which is what the guest channel routes on.
func TestToEventCarriesGuestFields(t *testing.T) {
	uid := uuid.New()
	payload, _ := json.Marshal(outboxPayload{
		RestaurantID: uuid.New(), UserID: &uid, Name: "Дамир", Guests: 2,
		StartsAt: time.Now(), CancelledBy: domain.CancelledByRestaurant,
	})
	e, err := toEvent(domain.BookingOutboxEvent{
		ID: uuid.New(), BookingID: uuid.New(),
		EventType: domain.EventBookingCancelled, Payload: payload,
	})
	if err != nil {
		t.Fatalf("toEvent: %v", err)
	}
	if e.GuestUserID == nil || *e.GuestUserID != uid {
		t.Fatalf("guest user id = %v, want %s", e.GuestUserID, uid)
	}
	if e.CancelledBy != domain.CancelledByRestaurant {
		t.Fatalf("cancelled_by = %q, want restaurant", e.CancelledBy)
	}
}

// THE RECEIPT HANDLE. Expo's `ok` ticket means "accepted", not "delivered", so
// the ticket id must be queued for receipt polling. This test fails on the old
// behaviour, where Send returned a bare verdict and the id was thrown away:
// nothing was ever enqueued and a delayed DeviceNotRegistered could not be seen.
func TestGuestPushEnqueuesTicketForReceiptPolling(t *testing.T) {
	uid := uuid.New()
	tok := guestToken(uid)
	h := newGuestHarness(t, tok)
	e := guestEvent(uid, domain.EventBookingConfirmed)

	if err := h.n.Notify(context.Background(), e); err != nil {
		t.Fatalf("notify: %v", err)
	}
	ticket := h.sender.ticketOf(tok.Token)
	if ticket == "" {
		t.Fatal("the fake provider handed out no ticket id")
	}
	queued := h.tickets.unresolvedIDs()
	if len(queued) != 1 || queued[0] != ticket {
		t.Fatalf("queued tickets = %v, want exactly the ticket the provider returned (%s)", queued, ticket)
	}
	h.tickets.mu.Lock()
	row := h.tickets.rows[ticket]
	h.tickets.mu.Unlock()
	if row.DeviceTokenID != tok.ID {
		t.Fatalf("ticket points at device %s, want %s", row.DeviceTokenID, tok.ID)
	}
	if row.OutboxEventID == nil || *row.OutboxEventID != e.OutboxEventID {
		t.Fatalf("ticket outbox_event_id = %v, want %s", row.OutboxEventID, e.OutboxEventID)
	}
}

// A verdict with no ticket id enqueues nothing: there is no receipt to ask for,
// and a row with an empty id would be a permanent no-answer in the queue.
func TestGuestPushEnqueuesNothingWithoutATicketID(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	h.sender.noTicket = true

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := h.tickets.count(); got != 0 {
		t.Fatalf("queued %d tickets without an id, want 0", got)
	}
}

// A dead device reported ALREADY IN THE TICKET has no receipt to poll: it is
// deactivated on the spot, and nothing is queued.
func TestGuestPushEnqueuesNothingForAGoneDevice(t *testing.T) {
	uid := uuid.New()
	tok := guestToken(uid)
	h := newGuestHarness(t, tok)
	h.sender.verdict[tok.Token] = MobilePushDeviceGone

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got := h.tickets.count(); got != 0 {
		t.Fatalf("queued %d tickets for a device the ticket already declared gone, want 0", got)
	}
}

// Bookkeeping must never jam the outbox: a failing ticket store is logged, not
// returned. The push has already left; failing the event would re-run every
// sibling channel for the sake of a row nobody is waiting on.
func TestGuestPushSurvivesTicketStoreFailure(t *testing.T) {
	uid := uuid.New()
	h := newGuestHarness(t, guestToken(uid))
	h.tickets.recErr = errors.New("db down")

	if err := h.n.Notify(context.Background(), guestEvent(uid, domain.EventBookingConfirmed)); err != nil {
		t.Fatalf("notify: %v, want the event to drain despite the ticket write failing", err)
	}
	if got := h.sender.count(); got != 1 {
		t.Fatalf("pushes sent = %d, want 1", got)
	}
}

// --- booking-rules footer (Trello BNjLdfSP) ---

func hourly(i int) *int { return &i }

// The confirmation footer names the hold time, the venue's own late-arrival
// note (not the platform default, since this venue set one) and the
// free-cancellation deadline derived from the MONEY-path window — not an
// independent number.
func TestBookingRulesFooterOnConfirmed(t *testing.T) {
	hold, text := 25, "Опоздали? Звоните хостес."
	n := &GuestPushNotifier{
		rules: fakeVenues{rules: domain.BookingRulesOverride{
			HoldMinutes: &hold, LateArrivalText: &text,
		}, freeCancelMinutes: hourly(180)},
		rulesDefaults: testBookingRulesDefaults,
		log:           discardLog(),
	}
	e := guestEvent(uuid.New(), domain.EventBookingConfirmed)
	footer := n.bookingRulesFooter(context.Background(), e)
	if !strings.Contains(footer, "25 мин") {
		t.Errorf("footer %q must name the venue's own hold time (25), not the platform default", footer)
	}
	if !strings.Contains(footer, text) {
		t.Errorf("footer %q must carry the venue's own late-arrival note", footer)
	}
	// starts_at (19:30 UTC) − 180 min (the money-path window) = 16:30.
	if !strings.Contains(footer, "16:30") {
		t.Errorf("footer %q must derive the free-cancel deadline (180 min = until 16:30) from the money-path window", footer)
	}
}

// The reminder footer is shorter (no free-cancel line) but still carries the
// venue's own hold time and late-arrival note.
func TestBookingRulesFooterOnReminder(t *testing.T) {
	hold := 10
	n := &GuestPushNotifier{
		rules:         fakeVenues{rules: domain.BookingRulesOverride{HoldMinutes: &hold}},
		rulesDefaults: testBookingRulesDefaults,
		log:           discardLog(),
	}
	e := guestEvent(uuid.New(), domain.EventBookingReminder)
	footer := n.bookingRulesFooter(context.Background(), e)
	if !strings.Contains(footer, "10 мин") {
		t.Errorf("footer %q must name the venue's own hold time", footer)
	}
	if !strings.Contains(footer, testBookingRulesDefaults.LateArrivalText) {
		t.Errorf("footer %q must fall back to the platform default late-arrival text (venue set none)", footer)
	}
}

// A cancellation is not a "come visit us" message — no footer.
func TestBookingRulesFooterOmittedOnCancelled(t *testing.T) {
	n := &GuestPushNotifier{rules: fakeVenues{}, rulesDefaults: testBookingRulesDefaults, log: discardLog()}
	e := guestEvent(uuid.New(), domain.EventBookingCancelled)
	if footer := n.bookingRulesFooter(context.Background(), e); footer != "" {
		t.Errorf("footer = %q, want empty for booking.cancelled", footer)
	}
}

// A lookup failure degrades to no footer, never an error — the same posture
// as the venue-name lookup right above it in Notify.
func TestBookingRulesFooterSwallowsReaderError(t *testing.T) {
	n := &GuestPushNotifier{
		rules:         fakeVenues{rulesErr: errors.New("db down")},
		rulesDefaults: testBookingRulesDefaults,
		log:           discardLog(),
	}
	e := guestEvent(uuid.New(), domain.EventBookingConfirmed)
	if footer := n.bookingRulesFooter(context.Background(), e); footer != "" {
		t.Errorf("footer = %q, want empty when the reader errors", footer)
	}
}

// No reader wired at all (rules == nil, the zero value in most existing
// tests/wiring) must not panic and must simply omit the footer.
func TestBookingRulesFooterNilReaderIsNoop(t *testing.T) {
	n := &GuestPushNotifier{log: discardLog()}
	e := guestEvent(uuid.New(), domain.EventBookingConfirmed)
	if footer := n.bookingRulesFooter(context.Background(), e); footer != "" {
		t.Errorf("footer = %q, want empty with no rules reader wired", footer)
	}
}

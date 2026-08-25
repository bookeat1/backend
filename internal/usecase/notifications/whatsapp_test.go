package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes -------------------------------------------------------------

// fakeStaff is an in-memory restaurant_managers roster: the WhatsApp recipients
// come from the staff table, so this is the fan-out source under test.
type fakeStaff struct {
	rows map[uuid.UUID][]domain.RestaurantManager
	err  error
}

func newFakeStaff() *fakeStaff {
	return &fakeStaff{rows: map[uuid.UUID][]domain.RestaurantManager{}}
}

func (f *fakeStaff) add(rid uuid.UUID, optIn bool, phone string) uuid.UUID {
	id := uuid.New()
	m := domain.RestaurantManager{ID: id, RestaurantID: rid, UserID: uuid.New(), WhatsappOptIn: optIn}
	if phone != "" {
		p := phone
		m.WhatsappPhone = &p
	}
	f.rows[rid] = append(f.rows[rid], m)
	return id
}

func (f *fakeStaff) ListByRestaurant(_ context.Context, rid uuid.UUID) ([]domain.RestaurantManager, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[rid], nil
}

// fakeZones answers with one timezone for every venue.
type fakeZones struct {
	tz  string
	err error
}

func (f fakeZones) Timezone(context.Context, uuid.UUID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.tz, nil
}

// recordingWhatsAppSender captures every (phone, params) it was asked to send
// and returns a scripted status/error per phone.
type recordingWhatsAppSender struct {
	mu     sync.Mutex
	sent   []whatsAppSend
	status map[string]int
	errFor map[string]error
}

type whatsAppSend struct {
	phone  string
	params []string
}

func newRecordingWhatsAppSender() *recordingWhatsAppSender {
	return &recordingWhatsAppSender{status: map[string]int{}, errFor: map[string]error{}}
}

func (s *recordingWhatsAppSender) send(_ context.Context, phone string, params []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, whatsAppSend{phone: phone, params: append([]string(nil), params...)})
	if err := s.errFor[phone]; err != nil {
		return s.status[phone], err
	}
	if st, ok := s.status[phone]; ok {
		if st < 200 || st > 299 {
			return st, errors.New("whatsapp: rejected")
		}
		return st, nil
	}
	return 200, nil
}

func (s *recordingWhatsAppSender) sends() []whatsAppSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]whatsAppSend, len(s.sent))
	copy(out, s.sent)
	return out
}

// waHarness wires a notifier over in-memory doubles and captures its log.
type waHarness struct {
	notifier *WhatsAppNotifier
	staff    *fakeStaff
	settings *fakeSettings
	deliv    *fakeDeliveries
	sender   *recordingWhatsAppSender
	logs     *bytes.Buffer
}

func newWAHarness(t *testing.T, enabled bool, tz string) *waHarness {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := &waHarness{
		staff:    newFakeStaff(),
		settings: newFakeSettings(),
		deliv:    newFakeDeliveries(),
		sender:   newRecordingWhatsAppSender(),
		logs:     &buf,
	}
	h.notifier = NewWhatsAppNotifier(h.staff, h.settings, h.deliv, fakeZones{tz: tz},
		h.sender.send, time.UTC, enabled, log)
	return h
}

// waEvent builds the booking.created event the venue channel reacts to.
func waEvent(rid uuid.UUID) Event {
	return Event{
		OutboxEventID: uuid.New(),
		BookingID:     uuid.New(),
		RestaurantID:  rid,
		Type:          domain.EventBookingCreated,
		GuestName:     "Дамир",
		GuestPhone:    "+77078692233",
		Guests:        4,
		// 19:30 UTC = 00:30 next day in Asia/Almaty (UTC+5).
		StartsAt: time.Date(2026, 8, 24, 19, 30, 0, 0, time.UTC),
	}
}

// --- tests -------------------------------------------------------------

// The whole point of the feature: a new booking reaches the venue's opted-in
// staff, with the four parameters the approved template expects, in order.
func TestWhatsAppSendsOnBookingCreated(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	mgr := h.staff.add(rid, true, "+77010000001")

	e := waEvent(rid)
	if err := h.notifier.Notify(context.Background(), e); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	sends := h.sender.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(sends))
	}
	if sends[0].phone != "+77010000001" {
		t.Errorf("phone = %q", sends[0].phone)
	}
	// {{1}} when — in the VENUE's zone (UTC+5), spelled the way a person reads
	// it. 19:30 UTC on 24 Aug is 00:30 on the 25th in Almaty.
	want := []string{"25 августа в 00:30", "4", "Дамир", "+77078692233"}
	if len(sends[0].params) != len(want) {
		t.Fatalf("params = %v, want %d of them", sends[0].params, len(want))
	}
	for i := range want {
		if sends[0].params[i] != want[i] {
			t.Errorf("param %d = %q, want %q", i+1, sends[0].params[i], want[i])
		}
	}
	// Delivery is recorded against the STAFF ROW (not the venue): a venue can
	// have several people on the alert, and each must be deduped separately.
	done, err := h.deliv.AlreadyDelivered(context.Background(), e.OutboxEventID, domain.ChannelWhatsApp, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("the send was not recorded against the staff row")
	}
}

// A venue with nobody opted in is a normal, expected state — it must drain the
// event, not fail it, or the outbox wedges for every other channel (and the
// booking stays invisible to web push / telegram / the guest).
func TestWhatsAppNoRecipientsIsNotAnError(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	h.staff.add(rid, false, "+77010000001") // consented: no
	h.staff.add(rid, true, "")              // consented but no number
	h.staff.add(rid, true, "+7701")         // unusable number

	if err := h.notifier.Notify(context.Background(), waEvent(rid)); err != nil {
		t.Fatalf("Notify must not fail when nobody opted in: %v", err)
	}
	if n := len(h.sender.sends()); n != 0 {
		t.Fatalf("sent %d messages, want 0", n)
	}
}

// A booking is created and answered long before this code runs (the send lives
// in the worker, off the outbox), so a WhatsApp failure can only ever decide
// whether the EVENT is retried — never whether the booking exists. This pins the
// two failure classes.
func TestWhatsAppSendFailureClassification(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		transport error
		wantErr   bool
	}{
		// No WhatsApp account on that number / bad parameters / dead token: the
		// same request can never succeed, so it is consumed, not retried forever.
		{name: "permanent 400", status: 400},
		{name: "dead token 401", status: 401},
		{name: "rate limited", status: 429, wantErr: true},
		{name: "meta is down", status: 503, wantErr: true},
		{name: "transport failure", transport: errors.New("dial tcp: timeout"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rid := uuid.New()
			h := newWAHarness(t, true, "Asia/Almaty")
			h.staff.add(rid, true, "+77010000001")
			h.sender.status["+77010000001"] = tc.status
			if tc.transport != nil {
				h.sender.errFor["+77010000001"] = tc.transport
			}

			err := h.notifier.Notify(context.Background(), waEvent(rid))
			if tc.wantErr && err == nil {
				t.Fatal("want a retryable error so the outbox event is tried again")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a permanent rejection must be consumed, got %v", err)
			}
			if len(h.sender.sends()) != 1 {
				t.Fatalf("sends = %d, want exactly 1 attempt", len(h.sender.sends()))
			}
		})
	}
}

// A failed send must NOT be recorded in the ledger — otherwise the retry would
// skip the very recipient that never got the message.
func TestWhatsAppFailedSendIsNotRecordedAsDelivered(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	mgr := h.staff.add(rid, true, "+77010000001")
	h.sender.status["+77010000001"] = 500

	e := waEvent(rid)
	if err := h.notifier.Notify(context.Background(), e); err == nil {
		t.Fatal("want an error on a 5xx")
	}
	already, err := h.deliv.AlreadyDelivered(context.Background(), e.OutboxEventID, domain.ChannelWhatsApp, mgr)
	if err != nil {
		t.Fatal(err)
	}
	if already {
		t.Fatal("a failed send was recorded as delivered — the retry would skip this recipient")
	}

	// Second tick: the send succeeds and IS recorded.
	delete(h.sender.status, "+77010000001")
	if err := h.notifier.Notify(context.Background(), e); err != nil {
		t.Fatalf("retry: %v", err)
	}
	already, _ = h.deliv.AlreadyDelivered(context.Background(), e.OutboxEventID, domain.ChannelWhatsApp, mgr)
	if !already {
		t.Fatal("a successful send was not recorded")
	}
}

// At-least-once means the same event can arrive twice (a sibling channel failed
// and left it unpublished). The venue must not get the same booking twice.
func TestWhatsAppRedeliveryDoesNotDoubleSend(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	h.staff.add(rid, true, "+77010000001")

	e := waEvent(rid)
	for i := 0; i < 3; i++ {
		if err := h.notifier.Notify(context.Background(), e); err != nil {
			t.Fatalf("Notify #%d: %v", i, err)
		}
	}
	if n := len(h.sender.sends()); n != 1 {
		t.Fatalf("sent %d times, want 1", n)
	}
}

// Two people on the roster, one number written two ways: one alert each for the
// distinct numbers, and never two messages to the same handset.
func TestWhatsAppFansOutPerStaffAndDedupesNumbers(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	h.staff.add(rid, true, "+77010000001")
	h.staff.add(rid, true, "8 701 000 00 01") // the same number, written locally
	h.staff.add(rid, true, "+77010000002")

	if err := h.notifier.Notify(context.Background(), waEvent(rid)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	sends := h.sender.sends()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want 2 distinct numbers: %+v", len(sends), sends)
	}
}

// The kill switch: WHATSAPP_NOTIFY_ENABLED=false (or absent credentials) must
// make the channel a clean no-op, not a failure.
func TestWhatsAppKillSwitchSilencesTheChannel(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, false, "Asia/Almaty")
	h.staff.add(rid, true, "+77010000001")

	if err := h.notifier.Notify(context.Background(), waEvent(rid)); err != nil {
		t.Fatalf("a disabled channel must not fail the event: %v", err)
	}
	if n := len(h.sender.sends()); n != 0 {
		t.Fatalf("a disabled channel sent %d messages", n)
	}
}

// A nil sender is the same thing as disabled — a misconfiguration must not
// panic the worker.
func TestWhatsAppNilSenderIsDisabled(t *testing.T) {
	n := NewWhatsAppNotifier(newFakeStaff(), newFakeSettings(), newFakeDeliveries(),
		fakeZones{tz: "Asia/Almaty"}, nil, time.UTC, true, slog.Default())
	if err := n.Notify(context.Background(), waEvent(uuid.New())); err != nil {
		t.Fatalf("nil sender must no-op, got %v", err)
	}
}

// The venue's own switch (restaurant_notification_settings.whatsapp_enabled).
func TestWhatsAppVenueToggleOff(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	h.staff.add(rid, true, "+77010000001")
	h.settings.waDisabled[rid] = true

	if err := h.notifier.Notify(context.Background(), waEvent(rid)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if n := len(h.sender.sends()); n != 0 {
		t.Fatalf("a venue that switched WhatsApp off got %d messages", n)
	}
}

// Only a NEW booking. The approved template's text («Подтвердите или
// отклоните») is wrong for anything else, and Meta gives no way to reuse it.
func TestWhatsAppInterestedOnlyInCreated(t *testing.T) {
	n := NewWhatsAppNotifier(newFakeStaff(), newFakeSettings(), newFakeDeliveries(),
		fakeZones{}, func(context.Context, string, []string) (int, error) { return 200, nil },
		time.UTC, true, slog.Default())
	if !n.Interested(domain.EventBookingCreated) {
		t.Error("must react to booking.created")
	}
	for _, et := range []domain.BookingEventType{
		domain.EventBookingCancelled, domain.EventBookingConfirmed, domain.EventBookingReminder,
	} {
		if n.Interested(et) {
			t.Errorf("must NOT react to %s without its own approved template", et)
		}
	}
}

// Numbers are personal data. Neither the recipient's number nor the guest's may
// appear in a log line in full — the middle is masked, everywhere, including on
// the failure paths.
func TestWhatsAppMasksPhonesInLogs(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "success", status: 200},
		{name: "permanent rejection", status: 400},
		{name: "transient failure", status: 503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rid := uuid.New()
			h := newWAHarness(t, true, "Asia/Almaty")
			h.staff.add(rid, true, "+77010000001")
			h.sender.status["+77010000001"] = tc.status

			_ = h.notifier.Notify(context.Background(), waEvent(rid))

			out := h.logs.String()
			if strings.Contains(out, "+77010000001") {
				t.Fatalf("the recipient's number reached the log in full:\n%s", out)
			}
			if strings.Contains(out, "+77078692233") {
				t.Fatalf("the guest's number reached the log in full:\n%s", out)
			}
			if !strings.Contains(out, "+7701***0001") {
				t.Fatalf("expected the masked number in the log, got:\n%s", out)
			}
			// The log must still name the venue and the booking, or it is
			// useless for support.
			if !strings.Contains(out, rid.String()) {
				t.Fatalf("the venue id is missing from the log:\n%s", out)
			}
		})
	}
}

// An unreadable stored zone must not silence a venue: this is the wording of a
// message, not a payout boundary, so it degrades to the platform zone and says
// so.
func TestWhatsAppUnusableVenueTimezoneFallsBack(t *testing.T) {
	rid := uuid.New()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	staff := newFakeStaff()
	staff.add(rid, true, "+77010000001")
	sender := newRecordingWhatsAppSender()
	n := NewWhatsAppNotifier(staff, newFakeSettings(), newFakeDeliveries(),
		fakeZones{tz: "Mars/Olympus"}, sender.send, time.UTC, true, log)

	if err := n.Notify(context.Background(), waEvent(rid)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	sends := sender.sends()
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(sends))
	}
	// UTC fallback: 19:30 UTC stays 19:30.
	if sends[0].params[0] != "24 августа в 19:30" {
		t.Errorf("when = %q, want the platform-zone rendering", sends[0].params[0])
	}
	if !strings.Contains(buf.String(), "unusable") {
		t.Errorf("the bad zone was substituted silently:\n%s", buf.String())
	}
}

// A booking with no guest name / no phone still has to reach the venue: Meta
// rejects an EMPTY template parameter, so both are spelled out instead.
func TestWhatsAppFillsMissingGuestFields(t *testing.T) {
	rid := uuid.New()
	h := newWAHarness(t, true, "Asia/Almaty")
	h.staff.add(rid, true, "+77010000001")

	e := waEvent(rid)
	e.GuestName, e.GuestPhone = "", ""
	if err := h.notifier.Notify(context.Background(), e); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	p := h.sender.sends()[0].params
	if p[2] != "Гость" || p[3] != "не указан" {
		t.Fatalf("params = %v, want a placeholder name and phone", p)
	}
}

// The booking usecase must have NO knowledge of this channel: the alert is
// produced from an outbox row that was already committed with the booking. This
// test walks the real dispatcher with a WhatsApp channel that always fails and
// asserts the two properties that keep a booking safe: the sibling channels
// still run, and the failure only costs a retry of the EVENT.
func TestWhatsAppFailureCannotAffectTheBooking(t *testing.T) {
	rid := uuid.New()
	ev := createdEvent(rid)
	// The event exists because the booking was already committed — decode it to
	// prove the notifier only ever reads a row that has outlived the request.
	var p outboxPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}

	outbox := newFakeOutbox(ev)
	staff := newFakeStaff()
	staff.add(rid, true, "+77010000001")
	sender := newRecordingWhatsAppSender()
	sender.status["+77010000001"] = 503
	deliveries := newFakeDeliveries()
	settings := newFakeSettings()
	settings.tgChat[rid] = "chat-1"

	tgSender := newRecordingTelegramSender()
	telegram := NewTelegramNotifier(settings, deliveries, tgSender.send, true, slog.Default())
	whatsApp := NewWhatsAppNotifier(staff, settings, deliveries, fakeZones{tz: "Asia/Almaty"},
		sender.send, time.UTC, true, slog.Default())

	d := NewDispatcher(outbox, noopTx{}, DispatcherConfig{}, slog.Default(), telegram, whatsApp)
	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Retry != 1 {
		t.Fatalf("retry = %d, want the event left for another tick", res.Retry)
	}
	if outbox.isPublished(ev.ID) {
		t.Fatal("an event with a failed send must stay unpublished")
	}
	// The sibling channel was not held back by WhatsApp's failure.
	if n := len(tgSender.sends()); n != 1 {
		t.Fatalf("telegram sends = %d, want 1 — a WhatsApp failure must not silence other channels", n)
	}
	// And on the next tick telegram is NOT re-sent (ledger), while WhatsApp is.
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if n := len(tgSender.sends()); n != 1 {
		t.Fatalf("telegram was re-sent on the redelivery (%d sends)", n)
	}
	if n := len(sender.sends()); n != 2 {
		t.Fatalf("whatsapp attempts = %d, want a retry", n)
	}
}

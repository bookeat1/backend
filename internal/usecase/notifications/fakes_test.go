package notifications

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// noopTx runs fn inline with no real transaction — enough for the dispatcher's
// claim/mark passes in a unit test.
type noopTx struct{}

func (noopTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (noopTx) Detach(ctx context.Context) context.Context                         { return ctx }

// fakeOutbox is an in-memory booking outbox implementing the drain surface the
// dispatcher uses (ClaimDue / MarkPublished / Reschedule / Abandon). It mirrors
// the SQL of migration 0083 deliberately closely — the due filter AND the
// fresh-events-before-retries ordering — because the fairness the dispatcher
// relies on lives in that ORDER BY, and a fake that ignored it would let a
// starvation bug pass the test suite. It also records how many times each event
// was claimed so a test can prove a re-claim (redelivery) happens.
type fakeOutbox struct {
	mu        sync.Mutex
	events    []domain.BookingOutboxEvent
	published map[uuid.UUID]bool
	claims    map[uuid.UUID]int
	// retry state, keyed by event id, exactly the three 0083 columns
	attempts  map[uuid.UUID]int
	nextAt    map[uuid.UUID]time.Time
	lastErr   map[uuid.UUID]string
	abandoned map[uuid.UUID]time.Time
}

func newFakeOutbox(evs ...domain.BookingOutboxEvent) *fakeOutbox {
	return &fakeOutbox{
		events: evs, published: map[uuid.UUID]bool{}, claims: map[uuid.UUID]int{},
		attempts: map[uuid.UUID]int{}, nextAt: map[uuid.UUID]time.Time{},
		lastErr: map[uuid.UUID]string{}, abandoned: map[uuid.UUID]time.Time{},
	}
}

func (f *fakeOutbox) Create(context.Context, *domain.BookingOutboxEvent) error { return nil }

func (f *fakeOutbox) ClaimDue(_ context.Context, limit int, now time.Time) ([]domain.BookingOutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	type candidate struct {
		ev    domain.BookingOutboxEvent
		due   time.Time // COALESCE(next_attempt_at, created_at)
		retry bool      // attempts > 0
	}
	var cands []candidate
	for _, e := range f.events {
		if f.published[e.ID] {
			continue
		}
		if _, dead := f.abandoned[e.ID]; dead {
			continue
		}
		due := e.CreatedAt
		if n, ok := f.nextAt[e.ID]; ok {
			if n.After(now) {
				continue
			}
			due = n
		}
		e.Attempts = f.attempts[e.ID]
		e.LastError = f.lastErr[e.ID]
		cands = append(cands, candidate{ev: e, due: due, retry: e.Attempts > 0})
	}
	// ORDER BY (attempts > 0), COALESCE(next_attempt_at, created_at)
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].retry != cands[j].retry {
			return !cands[i].retry
		}
		return cands[i].due.Before(cands[j].due)
	})
	var out []domain.BookingOutboxEvent
	for _, c := range cands {
		if len(out) >= limit {
			break
		}
		f.claims[c.ev.ID]++
		out = append(out, c.ev)
	}
	return out, nil
}

func (f *fakeOutbox) ExistsForBooking(context.Context, uuid.UUID, domain.BookingEventType) (bool, error) {
	return false, nil
}

func (f *fakeOutbox) MarkPublished(_ context.Context, ids []uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.published[id] = true
	}
	return nil
}

func (f *fakeOutbox) Reschedule(_ context.Context, failures []domain.BookingOutboxFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fl := range failures {
		f.attempts[fl.ID]++
		f.nextAt[fl.ID] = fl.NextAttemptAt
		f.lastErr[fl.ID] = fl.LastError
	}
	return nil
}

func (f *fakeOutbox) Abandon(_ context.Context, failures []domain.BookingOutboxFailure, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fl := range failures {
		f.attempts[fl.ID]++
		f.lastErr[fl.ID] = fl.LastError
		f.abandoned[fl.ID] = at
		delete(f.nextAt, fl.ID)
	}
	return nil
}

func (f *fakeOutbox) isPublished(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[id]
}

func (f *fakeOutbox) isAbandoned(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.abandoned[id]
	return ok
}

func (f *fakeOutbox) attemptsOf(id uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[id]
}

func (f *fakeOutbox) lastErrorOf(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErr[id]
}

func (f *fakeOutbox) dueAt(id uuid.UUID) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nextAt[id]
	return n, ok
}

// stubNotifier is a dispatcher-level channel double: it answers Interested for
// the event types it was given and either records the event or fails with a
// fixed error. It exists so the dispatcher's fan-out and retry behaviour can be
// tested without dragging a real channel's settings/ledger/consent along.
type stubNotifier struct {
	mu      sync.Mutex
	channel domain.NotificationChannel
	types   map[domain.BookingEventType]bool
	err     error
	got     []uuid.UUID
}

func newStubNotifier(ch domain.NotificationChannel, err error, types ...domain.BookingEventType) *stubNotifier {
	m := map[domain.BookingEventType]bool{}
	for _, t := range types {
		m[t] = true
	}
	return &stubNotifier{channel: ch, types: m, err: err}
}

func (s *stubNotifier) Channel() domain.NotificationChannel { return s.channel }

func (s *stubNotifier) Interested(t domain.BookingEventType) bool { return s.types[t] }

func (s *stubNotifier) Notify(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e.OutboxEventID)
	return s.err
}

func (s *stubNotifier) delivered() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uuid.UUID, len(s.got))
	copy(out, s.got)
	return out
}

// fakeSubs is an in-memory push-subscription repository.
type fakeSubs struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.PushSubscription
}

func newFakeSubs(subs ...domain.PushSubscription) *fakeSubs {
	f := &fakeSubs{rows: map[uuid.UUID]domain.PushSubscription{}}
	for _, s := range subs {
		f.rows[s.ID] = s
	}
	return f
}

func (f *fakeSubs) Upsert(_ context.Context, s *domain.PushSubscription) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	f.rows[s.ID] = *s
	return nil
}

func (f *fakeSubs) DeleteByEndpointForUser(_ context.Context, userID uuid.UUID, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, s := range f.rows {
		if s.UserID == userID && s.Endpoint == endpoint {
			delete(f.rows, id)
		}
	}
	return nil
}

func (f *fakeSubs) ListByRestaurant(_ context.Context, restaurantID uuid.UUID) ([]domain.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.PushSubscription
	for _, s := range f.rows {
		if s.RestaurantID == restaurantID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeSubs) DeleteByID(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	return nil
}

func (f *fakeSubs) has(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]
	return ok
}

// fakeDeliveries is the in-memory dedupe ledger.
type fakeDeliveries struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newFakeDeliveries() *fakeDeliveries { return &fakeDeliveries{seen: map[string]bool{}} }

func delKey(ev uuid.UUID, ch domain.NotificationChannel, sub uuid.UUID) string {
	return ev.String() + "|" + string(ch) + "|" + sub.String()
}

func (f *fakeDeliveries) AlreadyDelivered(_ context.Context, ev uuid.UUID, ch domain.NotificationChannel, sub uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[delKey(ev, ch, sub)], nil
}

func (f *fakeDeliveries) RecordDelivered(_ context.Context, ev uuid.UUID, ch domain.NotificationChannel, sub uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[delKey(ev, ch, sub)] = true
	return nil
}

// fakeSettings toggles web push and telegram per restaurant; absent = enabled
// (default on). tgChat holds the connected telegram chat id per restaurant;
// tgDisabled marks the telegram channel explicitly off.
type fakeSettings struct {
	disabled   map[uuid.UUID]bool
	tgChat     map[uuid.UUID]string
	tgDisabled map[uuid.UUID]bool
	// tgNewBotReady / tgNewBotFailed mirror the two columns of migration 0098:
	// which venues the new restaurants bot may write to, and when it was last
	// refused. Kept in the fake so a test can assert the demotion actually
	// happened, not just that the fallback fired.
	tgNewBotReady  map[uuid.UUID]*time.Time
	tgNewBotFailed map[uuid.UUID]*time.Time
	// waPhone/waDisabled — то же самое для WhatsApp: канал ведёт себя как
	// телеграм, поэтому и фейк устроен одинаково.
	waPhone    map[uuid.UUID]string
	waDisabled map[uuid.UUID]bool
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{
		disabled:   map[uuid.UUID]bool{},
		tgChat:     map[uuid.UUID]string{},
		tgDisabled: map[uuid.UUID]bool{},

		tgNewBotReady:  map[uuid.UUID]*time.Time{},
		tgNewBotFailed: map[uuid.UUID]*time.Time{},

		waPhone:    map[uuid.UUID]string{},
		waDisabled: map[uuid.UUID]bool{},
	}
}

func (f *fakeSettings) WebPushEnabled(_ context.Context, restaurantID uuid.UUID) (bool, error) {
	return !f.disabled[restaurantID], nil
}

func (f *fakeSettings) TelegramSettings(_ context.Context, restaurantID uuid.UUID) (domain.TelegramSettings, error) {
	return domain.TelegramSettings{
		ChatID:         f.tgChat[restaurantID],
		Enabled:        !f.tgDisabled[restaurantID],
		NewBotReadyAt:  f.tgNewBotReady[restaurantID],
		NewBotFailedAt: f.tgNewBotFailed[restaurantID],
	}, nil
}

// MarkTelegramNewBotReady / MarkTelegramNewBotFailed reproduce the SQL of
// migration 0098 exactly, including the part that matters: a failure CLEARS
// ready_at in the same step, which is what puts the venue back on the old bot.
func (f *fakeSettings) MarkTelegramNewBotReady(_ context.Context, restaurantID uuid.UUID) error {
	now := time.Now()
	f.tgNewBotReady[restaurantID] = &now
	return nil
}

func (f *fakeSettings) MarkTelegramNewBotFailed(_ context.Context, restaurantID uuid.UUID) error {
	now := time.Now()
	delete(f.tgNewBotReady, restaurantID)
	f.tgNewBotFailed[restaurantID] = &now
	return nil
}

func (f *fakeSettings) TelegramMigrationStatus(context.Context) ([]domain.TelegramMigrationRow, error) {
	out := make([]domain.TelegramMigrationRow, 0, len(f.tgChat))
	for rid, chat := range f.tgChat {
		out = append(out, domain.TelegramMigrationRow{
			RestaurantID:   rid,
			ChatID:         chat,
			Enabled:        !f.tgDisabled[rid],
			NewBotReadyAt:  f.tgNewBotReady[rid],
			NewBotFailedAt: f.tgNewBotFailed[rid],
		})
	}
	return out, nil
}

func (f *fakeSettings) SetTelegramChatID(_ context.Context, restaurantID uuid.UUID, chatID string) error {
	f.tgChat[restaurantID] = chatID
	f.tgDisabled[restaurantID] = false
	return nil
}

// RestaurantByTelegramChatID is the reverse lookup the inbound webhook uses.
// This fake keeps the same map the forward lookup reads, so a chat connected in
// a test resolves back to its venue.
func (f *fakeSettings) RestaurantByTelegramChatID(_ context.Context, chatID string) (uuid.UUID, error) {
	for rid, c := range f.tgChat {
		if c == chatID {
			return rid, nil
		}
	}
	return uuid.Nil, domain.ErrNotFound
}

func (f *fakeSettings) ClearTelegramChatID(_ context.Context, restaurantID uuid.UUID) error {
	delete(f.tgChat, restaurantID)
	return nil
}

func (f *fakeSettings) WhatsAppSettings(_ context.Context, restaurantID uuid.UUID) (domain.WhatsAppSettings, error) {
	return domain.WhatsAppSettings{
		Phone:   f.waPhone[restaurantID],
		Enabled: !f.waDisabled[restaurantID],
	}, nil
}

func (f *fakeSettings) SetWhatsAppPhone(_ context.Context, restaurantID uuid.UUID, phone string) error {
	f.waPhone[restaurantID] = phone
	f.waDisabled[restaurantID] = false
	return nil
}

func (f *fakeSettings) ClearWhatsAppPhone(_ context.Context, restaurantID uuid.UUID) error {
	delete(f.waPhone, restaurantID)
	return nil
}

// RestaurantByWhatsAppPhone — обратный поиск, которым авторизуется входящее
// нажатие кнопки: номер отправителя и есть единственное доказательство права.
func (f *fakeSettings) RestaurantByWhatsAppPhone(_ context.Context, phone string) (uuid.UUID, error) {
	for rid, p := range f.waPhone {
		if p == phone && !f.waDisabled[rid] {
			return rid, nil
		}
	}
	return uuid.Nil, domain.ErrNotFound
}

// recordingTelegramSender captures every (chatID, text) it was asked to send
// and returns a scripted status/error per chat id.
type recordingTelegramSender struct {
	mu     sync.Mutex
	sent   []telegramSend
	status map[string]int   // default 200 when absent
	errFor map[string]error // transport error per chat id
}

type telegramSend struct {
	chatID string
	text   string
}

func newRecordingTelegramSender() *recordingTelegramSender {
	return &recordingTelegramSender{status: map[string]int{}, errFor: map[string]error{}}
}

func (s *recordingTelegramSender) send(_ context.Context, chatID, text string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, telegramSend{chatID: chatID, text: text})
	if err := s.errFor[chatID]; err != nil {
		return 0, err
	}
	if st, ok := s.status[chatID]; ok {
		return st, nil
	}
	return 200, nil
}

func (s *recordingTelegramSender) sends() []telegramSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]telegramSend, len(s.sent))
	copy(out, s.sent)
	return out
}

// recordingSender captures every (subscription) it was asked to push to and
// returns a scripted status/error per subscription id.
type recordingSender struct {
	mu     sync.Mutex
	sent   []uuid.UUID
	status map[uuid.UUID]int   // default 201 when absent
	errFor map[uuid.UUID]error // transport error per subscription
}

func newRecordingSender() *recordingSender {
	return &recordingSender{status: map[uuid.UUID]int{}, errFor: map[uuid.UUID]error{}}
}

func (s *recordingSender) send(_ context.Context, sub domain.PushSubscription, _ []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sub.ID)
	if err := s.errFor[sub.ID]; err != nil {
		return 0, err
	}
	if st, ok := s.status[sub.ID]; ok {
		return st, nil
	}
	return 201, nil
}

func (s *recordingSender) sentIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uuid.UUID, len(s.sent))
	copy(out, s.sent)
	return out
}

// createdEvent builds a booking.created outbox row with the given restaurant.
func createdEvent(restaurantID uuid.UUID) domain.BookingOutboxEvent {
	bookingID := uuid.New()
	payload, _ := json.Marshal(outboxPayload{
		RestaurantID: restaurantID,
		Name:         "Damir",
		Guests:       4,
		StartsAt:     time.Date(2026, 8, 1, 19, 30, 0, 0, time.UTC),
	})
	return domain.BookingOutboxEvent{
		ID:        uuid.New(),
		BookingID: bookingID,
		EventType: domain.EventBookingCreated,
		Payload:   payload,
		CreatedAt: time.Now(),
	}
}

// cancelledEvent builds a booking.cancelled outbox row with the given restaurant
// and the actor that performed the cancellation (guest | restaurant | system |
// "" for unknown).
func cancelledEvent(restaurantID uuid.UUID, by domain.CancelledBy) domain.BookingOutboxEvent {
	payload, _ := json.Marshal(outboxPayload{
		RestaurantID: restaurantID,
		Name:         "Damir",
		Phone:        "+77078692233",
		Guests:       4,
		StartsAt:     time.Date(2026, 8, 1, 19, 30, 0, 0, time.UTC),
		CancelledBy:  by,
	})
	return domain.BookingOutboxEvent{
		ID:        uuid.New(),
		BookingID: uuid.New(),
		EventType: domain.EventBookingCancelled,
		Payload:   payload,
		CreatedAt: time.Now(),
	}
}

// fakeDeviceTokens is an in-memory guest device-token repository. Upsert mirrors
// the production ON CONFLICT (token) semantics: keyed on the TOKEN, so a repeat
// registration re-points the existing row instead of adding a second one.
type fakeDeviceTokens struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]*domain.DevicePushToken
	byTok  map[string]uuid.UUID
	upsert int
	// deactivateErr makes DeactivateByID fail, so a test can prove the receipt
	// worker does NOT close a ticket whose whole purpose it just failed to
	// carry out.
	deactivateErr error
}

func newFakeDeviceTokens(rows ...domain.DevicePushToken) *fakeDeviceTokens {
	f := &fakeDeviceTokens{byID: map[uuid.UUID]*domain.DevicePushToken{}, byTok: map[string]uuid.UUID{}}
	for i := range rows {
		r := rows[i]
		f.byID[r.ID] = &r
		f.byTok[r.Token] = r.ID
	}
	return f
}

func (f *fakeDeviceTokens) Upsert(_ context.Context, t *domain.DevicePushToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsert++
	if id, ok := f.byTok[t.Token]; ok {
		row := f.byID[id]
		row.UserID, row.Platform, row.IsActive = t.UserID, t.Platform, true
		*t = *row
		return nil
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	t.IsActive = true
	row := *t
	f.byID[t.ID] = &row
	f.byTok[t.Token] = t.ID
	return nil
}

func (f *fakeDeviceTokens) ListActiveByUser(_ context.Context, userID uuid.UUID) ([]domain.DevicePushToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.DevicePushToken
	for _, r := range f.byID {
		if r.UserID == userID && r.IsActive {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeDeviceTokens) DeactivateByID(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deactivateErr != nil {
		return f.deactivateErr
	}
	if r, ok := f.byID[id]; ok {
		r.IsActive = false
	}
	return nil
}

func (f *fakeDeviceTokens) DeactivateForUser(_ context.Context, userID uuid.UUID, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byTok[token]; ok {
		if r := f.byID[id]; r.UserID == userID {
			r.IsActive = false
		}
	}
	return nil
}

func (f *fakeDeviceTokens) isActive(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	return ok && r.IsActive
}

func (f *fakeDeviceTokens) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

func (f *fakeDeviceTokens) get(id uuid.UUID) domain.DevicePushToken {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.byID[id]
}

// fakeGuestPrefs is the guest notification-preference repository. An unset user
// gets the all-enabled default, exactly like the Postgres implementation.
type fakeGuestPrefs struct {
	mu   sync.Mutex
	rows map[uuid.UUID]domain.NotificationPreference
	err  error
}

func newFakeGuestPrefs() *fakeGuestPrefs {
	return &fakeGuestPrefs{rows: map[uuid.UUID]domain.NotificationPreference{}}
}

func (f *fakeGuestPrefs) set(p domain.NotificationPreference) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[p.UserID] = p
}

func (f *fakeGuestPrefs) Get(_ context.Context, userID uuid.UUID) (domain.NotificationPreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.NotificationPreference{}, f.err
	}
	if p, ok := f.rows[userID]; ok {
		return p, nil
	}
	return domain.DefaultNotificationPreference(userID), nil
}

func (f *fakeGuestPrefs) Upsert(_ context.Context, p domain.NotificationPreference) error {
	f.set(p)
	return nil
}

// fakeVenues resolves every restaurant to the same display name.
type fakeVenues struct{ name string }

func (f fakeVenues) Name(context.Context, uuid.UUID) (string, error) { return f.name, nil }

// recordingMobileSender captures every device token it was asked to push to and
// returns a scripted verdict/error per token. Every accepted send hands back a
// ticket id, like a real provider does — that id is what the receipt worker
// later polls, so a fake that dropped it would hide the very bug this seam was
// changed for.
type recordingMobileSender struct {
	mu        sync.Mutex
	sent      []string
	verdict   map[string]MobilePushVerdict // default Delivered when absent
	errFor    map[string]error
	ticketFor map[string]string // token -> ticket id; generated when absent
	noTicket  bool              // provider answered without a ticket id
}

func newRecordingMobileSender() *recordingMobileSender {
	return &recordingMobileSender{
		verdict:   map[string]MobilePushVerdict{},
		errFor:    map[string]error{},
		ticketFor: map[string]string{},
	}
}

func (s *recordingMobileSender) send(_ context.Context, token string, _ MobilePushMessage) (MobilePushResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, token)
	if err := s.errFor[token]; err != nil {
		return MobilePushResult{Verdict: MobilePushRejected}, err
	}
	if v, ok := s.verdict[token]; ok && v != MobilePushDelivered {
		// Only an accepted message gets a ticket.
		return MobilePushResult{Verdict: v}, nil
	}
	ticket := ""
	if !s.noTicket {
		ticket = s.ticketFor[token]
		if ticket == "" {
			ticket = "ticket-" + uuid.NewString()
			s.ticketFor[token] = ticket
		}
	}
	return MobilePushResult{Verdict: MobilePushDelivered, TicketID: ticket}, nil
}

// ticketOf returns the ticket id the fake handed out for a token.
func (s *recordingMobileSender) ticketOf(token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ticketFor[token]
}

// fakePushTickets is an in-memory domain.PushTicketRepository.
type fakePushTickets struct {
	mu       sync.Mutex
	rows     map[string]domain.PushTicket
	order    []string // insertion order, so ListUnresolved is deterministic
	recErr   error
	listErr  error
	resErr   error
	expErr   error
	resolved []string // ids passed to Resolve, in call order
}

func newFakePushTickets() *fakePushTickets {
	return &fakePushTickets{rows: map[string]domain.PushTicket{}}
}

func (f *fakePushTickets) Record(_ context.Context, t domain.PushTicket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recErr != nil {
		return f.recErr
	}
	if _, ok := f.rows[t.ID]; ok {
		// Idempotent, like ON CONFLICT DO NOTHING.
		return nil
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	f.rows[t.ID] = t
	f.order = append(f.order, t.ID)
	return nil
}

func (f *fakePushTickets) ListUnresolved(_ context.Context, createdBefore time.Time, limit int) ([]domain.PushTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.PushTicket
	for _, id := range f.order {
		t := f.rows[id]
		// Repeat the SQL predicate, do not simplify it: a fake that returned
		// everything would pass a test the real query fails.
		if t.ResolvedAt != nil || t.CreatedAt.After(createdBefore) {
			continue
		}
		out = append(out, t)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakePushTickets) Resolve(_ context.Context, ids []string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resErr != nil {
		return f.resErr
	}
	f.resolved = append(f.resolved, ids...)
	for _, id := range ids {
		t, ok := f.rows[id]
		if !ok || t.ResolvedAt != nil {
			continue
		}
		stamp := at
		t.ResolvedAt = &stamp
		f.rows[id] = t
	}
	return nil
}

func (f *fakePushTickets) ExpireOlderThan(_ context.Context, cutoff time.Time, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expErr != nil {
		return 0, f.expErr
	}
	var n int64
	for _, id := range f.order {
		t := f.rows[id]
		if t.ResolvedAt != nil || !t.CreatedAt.Before(cutoff) {
			continue
		}
		stamp := at
		t.ResolvedAt = &stamp
		f.rows[id] = t
		n++
	}
	return n, nil
}

func (f *fakePushTickets) unresolvedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, id := range f.order {
		if f.rows[id].ResolvedAt == nil {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakePushTickets) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

var _ domain.PushTicketRepository = (*fakePushTickets)(nil)

func (s *recordingMobileSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *recordingMobileSender) tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.sent))
	copy(out, s.sent)
	return out
}

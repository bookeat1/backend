package notifications

import (
	"context"
	"encoding/json"
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
// dispatcher uses (ClaimUnpublished / MarkPublished). It records how many times
// each event was claimed so a test can prove a re-claim (redelivery) happens.
type fakeOutbox struct {
	mu        sync.Mutex
	events    []domain.BookingOutboxEvent
	published map[uuid.UUID]bool
	claims    map[uuid.UUID]int
}

func newFakeOutbox(evs ...domain.BookingOutboxEvent) *fakeOutbox {
	return &fakeOutbox{events: evs, published: map[uuid.UUID]bool{}, claims: map[uuid.UUID]int{}}
}

func (f *fakeOutbox) Create(context.Context, *domain.BookingOutboxEvent) error { return nil }

func (f *fakeOutbox) ClaimUnpublished(_ context.Context, limit int) ([]domain.BookingOutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.BookingOutboxEvent
	for _, e := range f.events {
		if f.published[e.ID] {
			continue
		}
		if len(out) >= limit {
			break
		}
		f.claims[e.ID]++
		out = append(out, e)
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

func (f *fakeOutbox) isPublished(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[id]
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
		waPhone:    map[uuid.UUID]string{},
		waDisabled: map[uuid.UUID]bool{},
	}
}

func (f *fakeSettings) WebPushEnabled(_ context.Context, restaurantID uuid.UUID) (bool, error) {
	return !f.disabled[restaurantID], nil
}

func (f *fakeSettings) TelegramSettings(_ context.Context, restaurantID uuid.UUID) (domain.TelegramSettings, error) {
	return domain.TelegramSettings{
		ChatID:  f.tgChat[restaurantID],
		Enabled: !f.tgDisabled[restaurantID],
	}, nil
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
// returns a scripted verdict/error per token.
type recordingMobileSender struct {
	mu      sync.Mutex
	sent    []string
	verdict map[string]MobilePushVerdict // default Delivered when absent
	errFor  map[string]error
}

func newRecordingMobileSender() *recordingMobileSender {
	return &recordingMobileSender{verdict: map[string]MobilePushVerdict{}, errFor: map[string]error{}}
}

func (s *recordingMobileSender) send(_ context.Context, token string, _ MobilePushMessage) (MobilePushVerdict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, token)
	if err := s.errFor[token]; err != nil {
		return MobilePushRejected, err
	}
	if v, ok := s.verdict[token]; ok {
		return v, nil
	}
	return MobilePushDelivered, nil
}

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

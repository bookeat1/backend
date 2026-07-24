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

// fakeSettings toggles web push per restaurant; absent = enabled (default on).
type fakeSettings struct{ disabled map[uuid.UUID]bool }

func newFakeSettings() *fakeSettings { return &fakeSettings{disabled: map[uuid.UUID]bool{}} }

func (f *fakeSettings) WebPushEnabled(_ context.Context, restaurantID uuid.UUID) (bool, error) {
	return !f.disabled[restaurantID], nil
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

package bookings

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Hand-written fakes for the ports this package consumes (repo convention: no
// mock framework). They are intentionally dumb: state in, state out, plus a few
// error hooks so failure paths can be exercised.

type fakeBookings struct {
	byID     map[uuid.UUID]*domain.Booking
	created  []*domain.Booking
	updated  []*domain.Booking
	statuses []statusWrite
	list     []domain.Booking
	total    int
	lastFlt  domain.BookingFilter
	getErr   error
	createTx error
}

type statusWrite struct {
	ID     uuid.UUID
	Status domain.BookingStatus
	At     time.Time
}

func newFakeBookings(bs ...*domain.Booking) *fakeBookings {
	f := &fakeBookings{byID: map[uuid.UUID]*domain.Booking{}}
	for _, b := range bs {
		f.byID[b.ID] = b
	}
	return f
}

func (f *fakeBookings) Create(_ context.Context, b *domain.Booking) error {
	if f.createTx != nil {
		return f.createTx
	}
	cp := *b
	f.created = append(f.created, &cp)
	if f.byID == nil {
		f.byID = map[uuid.UUID]*domain.Booking{}
	}
	f.byID[b.ID] = b
	return nil
}

func (f *fakeBookings) Update(_ context.Context, b *domain.Booking) error {
	cp := *b
	f.updated = append(f.updated, &cp)
	return nil
}

func (f *fakeBookings) GetByID(_ context.Context, id uuid.UUID) (*domain.Booking, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeBookings) List(_ context.Context, flt domain.BookingFilter) ([]domain.Booking, int, error) {
	f.lastFlt = flt
	return f.list, f.total, nil
}

func (f *fakeBookings) UpdateStatus(_ context.Context, id uuid.UUID, s domain.BookingStatus, at time.Time) error {
	f.statuses = append(f.statuses, statusWrite{ID: id, Status: s, At: at})
	if b, ok := f.byID[id]; ok {
		b.Status = s
	}
	return nil
}

func (f *fakeBookings) ClaimDue(context.Context, []domain.BookingStatus, time.Time, int) ([]domain.Booking, error) {
	return nil, nil
}

type fakeLinks struct {
	created   []domain.BookingTable
	busy      []domain.TableBusyInterval
	createErr error
}

func (f *fakeLinks) Create(_ context.Context, links []domain.BookingTable) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, links...)
	return nil
}

func (f *fakeLinks) ReplaceForBooking(ctx context.Context, id uuid.UUID, links []domain.BookingTable) error {
	return f.Create(ctx, links)
}

func (f *fakeLinks) ListByBooking(_ context.Context, id uuid.UUID) ([]domain.BookingTable, error) {
	var out []domain.BookingTable
	for _, l := range f.created {
		if l.BookingID == id {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeLinks) ListBusy(_ context.Context, _ uuid.UUID, _, _ time.Time) ([]domain.TableBusyInterval, error) {
	return f.busy, nil
}

type fakeItems struct{ created []domain.BookingItem }

func (f *fakeItems) ListByBooking(_ context.Context, id uuid.UUID) ([]domain.BookingItem, error) {
	var out []domain.BookingItem
	for _, i := range f.created {
		if i.BookingID == id {
			out = append(out, i)
		}
	}
	return out, nil
}
func (f *fakeItems) ReplaceForBooking(ctx context.Context, id uuid.UUID, items []domain.BookingItem) error {
	return f.Create(ctx, items)
}
func (f *fakeItems) Create(_ context.Context, items []domain.BookingItem) error {
	f.created = append(f.created, items...)
	return nil
}
func (f *fakeItems) SetStatus(context.Context, uuid.UUID, domain.BookingItemStatus) error { return nil }

type fakeMessages struct {
	created    []domain.BookingMessage
	readReader domain.SenderType
	readCount  int
}

func (f *fakeMessages) ListByBooking(_ context.Context, id uuid.UUID) ([]domain.BookingMessage, error) {
	var out []domain.BookingMessage
	for _, m := range f.created {
		if m.BookingID == id {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeMessages) Create(_ context.Context, m *domain.BookingMessage) error {
	f.created = append(f.created, *m)
	return nil
}
func (f *fakeMessages) MarkRead(_ context.Context, _ uuid.UUID, reader domain.SenderType, _ time.Time) (int, error) {
	f.readReader = reader
	return f.readCount, nil
}

type fakeSurveys struct {
	created *domain.RestaurantSurvey
	stored  *domain.RestaurantSurvey
}

func (f *fakeSurveys) Create(_ context.Context, s *domain.RestaurantSurvey) error {
	f.created = s
	return nil
}
func (f *fakeSurveys) GetByBooking(context.Context, uuid.UUID) (*domain.RestaurantSurvey, error) {
	if f.stored == nil {
		return nil, domain.ErrNotFound
	}
	return f.stored, nil
}
func (f *fakeSurveys) ListByRestaurant(context.Context, uuid.UUID, int, int) ([]domain.RestaurantSurvey, error) {
	return nil, nil
}

type fakeHistory struct{ created []domain.BookingStatusChange }

func (f *fakeHistory) Create(_ context.Context, c *domain.BookingStatusChange) error {
	f.created = append(f.created, *c)
	return nil
}
func (f *fakeHistory) ListByBooking(_ context.Context, id uuid.UUID) ([]domain.BookingStatusChange, error) {
	var out []domain.BookingStatusChange
	for _, c := range f.created {
		if c.BookingID == id {
			out = append(out, c)
		}
	}
	return out, nil
}

type fakeOutbox struct{ created []domain.BookingOutboxEvent }

func (f *fakeOutbox) Create(_ context.Context, e *domain.BookingOutboxEvent) error {
	f.created = append(f.created, *e)
	return nil
}
func (f *fakeOutbox) ClaimUnpublished(context.Context, int) ([]domain.BookingOutboxEvent, error) {
	return nil, nil
}
func (f *fakeOutbox) MarkPublished(context.Context, []uuid.UUID, time.Time) error { return nil }

func (f *fakeOutbox) types() []domain.BookingEventType {
	out := make([]domain.BookingEventType, 0, len(f.created))
	for _, e := range f.created {
		out = append(out, e.EventType)
	}
	return out
}

type fakeBlacklist struct {
	match   *domain.BlacklistEntry
	lastQry domain.BlacklistQuery
}

func (f *fakeBlacklist) Match(_ context.Context, q domain.BlacklistQuery) (*domain.BlacklistEntry, error) {
	f.lastQry = q
	return f.match, nil
}
func (f *fakeBlacklist) ListByRestaurant(context.Context, uuid.UUID) ([]domain.BlacklistEntry, error) {
	return nil, nil
}
func (f *fakeBlacklist) Create(context.Context, *domain.BlacklistEntry) error { return nil }
func (f *fakeBlacklist) Deactivate(context.Context, uuid.UUID) error          { return nil }

type fakeRateLog struct {
	count   int
	entries []domain.BookingRateLogEntry
}

func (f *fakeRateLog) Create(_ context.Context, e *domain.BookingRateLogEntry) error {
	f.entries = append(f.entries, *e)
	return nil
}
func (f *fakeRateLog) CountSince(context.Context, string, domain.RateLogAction, time.Time) (int, error) {
	return f.count, nil
}

type fakeRestaurants struct {
	agg *domain.RestaurantAggregate
	err error
}

func (f *fakeRestaurants) GetByID(context.Context, uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.agg, nil
}

type fakeSchedule struct {
	hours  []domain.WorkingHours
	slots  []domain.TimeSlot
	tables []domain.RestaurantTable
}

func (f *fakeSchedule) ListWorkingHours(context.Context, uuid.UUID) ([]domain.WorkingHours, error) {
	return f.hours, nil
}
func (f *fakeSchedule) ListTimeSlots(context.Context, uuid.UUID) ([]domain.TimeSlot, error) {
	return f.slots, nil
}
func (f *fakeSchedule) ListTables(context.Context, uuid.UUID) ([]domain.RestaurantTable, error) {
	return f.tables, nil
}

// fakeManagers answers Manages from a fixed user→restaurant set.
type fakeManagers struct{ pairs map[[2]uuid.UUID]bool }

func newFakeManagers(pairs ...[2]uuid.UUID) *fakeManagers {
	m := &fakeManagers{pairs: map[[2]uuid.UUID]bool{}}
	for _, p := range pairs {
		m.pairs[p] = true
	}
	return m
}

func (f *fakeManagers) Manages(_ context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	return f.pairs[[2]uuid.UUID{userID, restaurantID}], nil
}

// fakeTx runs fn inline; it records that it was entered so tests can assert
// the mutation happened inside a transaction.
type fakeTx struct{ calls int }

func (f *fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	f.calls++
	return fn(ctx)
}

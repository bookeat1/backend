package bookings

import (
	"context"
	"fmt"
	"sort"
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
	claims   []claimCall
	claimErr error

	reconcileCalls []reconcileCall
	reconcileErr   error
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

// reconcileCall records one ListLiveForReconcile invocation. It is a slice, not
// a "last call" field, because the point of that method is that the caller reads
// the whole set in ONE statement — a test has to be able to prove there was
// exactly one call.
type reconcileCall struct {
	restaurantID uuid.UUID
	from         time.Time
	statuses     []domain.BookingStatus
	limit        int
}

// ListLiveForReconcile mirrors the real query rather than returning f.list
// wholesale: same predicates (venue, status, starts_at >= from), same total
// ascending order, same cap. A fake that ignored the filter would let a usecase
// bug through precisely where the real query is strict.
func (f *fakeBookings) ListLiveForReconcile(
	_ context.Context,
	restaurantID uuid.UUID,
	from time.Time,
	statuses []domain.BookingStatus,
	limit int,
) ([]domain.Booking, error) {
	f.reconcileCalls = append(f.reconcileCalls, reconcileCall{
		restaurantID: restaurantID, from: from, statuses: statuses, limit: limit})
	if f.reconcileErr != nil {
		return nil, f.reconcileErr
	}
	if limit <= 0 || limit > domain.MaxReconcileBookings {
		limit = domain.MaxReconcileBookings
	}
	wanted := map[domain.BookingStatus]bool{}
	for _, s := range statuses {
		wanted[s] = true
	}
	out := make([]domain.Booking, 0, len(f.list))
	for _, b := range f.list {
		if b.RestaurantID != restaurantID || !wanted[b.Status] || b.StartsAt.Before(from) {
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.Before(out[j].StartsAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBookings) UpdateStatus(_ context.Context, id uuid.UUID, s domain.BookingStatus, at time.Time) error {
	f.statuses = append(f.statuses, statusWrite{ID: id, Status: s, At: at})
	if b, ok := f.byID[id]; ok {
		b.Status = s
	}
	return nil
}

type claimCall struct {
	statuses []domain.BookingStatus
	by       domain.ClaimColumn
	before   time.Time
	limit    int
}

// ClaimDue mirrors the real query: the caller names the cutoff column, and the
// result is ordered by that same column so the oldest waiting row wins a short
// batch (the anti-starvation property the real query has to provide).
func (f *fakeBookings) ClaimDue(_ context.Context, statuses []domain.BookingStatus, by domain.ClaimColumn, before time.Time, limit int) ([]domain.Booking, error) {
	f.claims = append(f.claims, claimCall{statuses: statuses, by: by, before: before, limit: limit})
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if !by.Valid() {
		return nil, domain.ErrValidation
	}
	cutoffOf := func(b *domain.Booking) time.Time {
		if by == domain.ClaimByCreatedAt {
			return b.CreatedAt
		}
		return b.EndsAt
	}
	wanted := map[domain.BookingStatus]bool{}
	for _, s := range statuses {
		wanted[s] = true
	}
	var out []domain.Booking
	for _, b := range f.byID {
		if !wanted[b.Status] || !cutoffOf(b).Before(before) {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := cutoffOf(&out[i]), cutoffOf(&out[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

func (f *fakeOutbox) ExistsForBooking(_ context.Context, bookingID uuid.UUID, t domain.BookingEventType) (bool, error) {
	for _, e := range f.created {
		if e.BookingID == bookingID && e.EventType == t {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeOutbox) types() []domain.BookingEventType {
	out := make([]domain.BookingEventType, 0, len(f.created))
	for _, e := range f.created {
		out = append(out, e.EventType)
	}
	return out
}

type fakeBlacklist struct {
	match       *domain.BlacklistEntry
	lastQry     domain.BlacklistQuery
	list        []domain.BlacklistEntry
	created     []domain.BlacklistEntry
	deactivated []uuid.UUID
}

func (f *fakeBlacklist) Match(_ context.Context, q domain.BlacklistQuery) (*domain.BlacklistEntry, error) {
	f.lastQry = q
	return f.match, nil
}
func (f *fakeBlacklist) ListByRestaurant(context.Context, uuid.UUID) ([]domain.BlacklistEntry, error) {
	return f.list, nil
}
func (f *fakeBlacklist) Create(_ context.Context, e *domain.BlacklistEntry) error {
	f.created = append(f.created, *e)
	return nil
}
func (f *fakeBlacklist) Deactivate(_ context.Context, id uuid.UUID) error {
	f.deactivated = append(f.deactivated, id)
	return nil
}

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
	agg  *domain.RestaurantAggregate
	byID map[uuid.UUID]*domain.RestaurantAggregate
	err  error
}

func (f *fakeRestaurants) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	if f.err != nil {
		return nil, f.err
	}
	if a, ok := f.byID[id]; ok {
		return a, nil
	}
	return f.agg, nil
}

type fakeSchedule struct {
	hours     []domain.WorkingHours
	overrides []domain.ScheduleOverride
	slots     []domain.TimeSlot
	tables    []domain.RestaurantTable

	// overrideFrom/overrideTo record the window of the last
	// ListScheduleOverrides call, so a test can assert the engine asked about
	// the date it was given and not about "now".
	overrideFrom, overrideTo time.Time
}

func (f *fakeSchedule) ListWorkingHours(context.Context, uuid.UUID) ([]domain.WorkingHours, error) {
	return f.hours, nil
}

// ListScheduleOverrides HONOURS the [from, to] window instead of returning
// everything it holds. A fake that ignored the bound would make every caller's
// window look correct and let a real one that asks for the wrong dates ship
// green — the same trap as bugs/bookeat-backend-fake-ignoring-context.
func (f *fakeSchedule) ListScheduleOverrides(_ context.Context, _ uuid.UUID, from, to time.Time) ([]domain.ScheduleOverride, error) {
	f.overrideFrom, f.overrideTo = from, to
	lo, hi := dateOnly(from), dateOnly(to)
	out := make([]domain.ScheduleOverride, 0, len(f.overrides))
	for _, o := range f.overrides {
		d := dateOnly(o.Date)
		if d < lo || d > hi {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// dateOnly renders the calendar date t carries in its OWN location, matching
// what the repository binds to a `date` column.
func dateOnly(t time.Time) string { return t.Format("2006-01-02") }
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

func (f *fakeTx) Detach(ctx context.Context) context.Context { return ctx }

// fakeCapacity is an in-memory domain.BookingCapacityRepository. It reproduces
// the ONE behaviour the usecases depend on — the venue's declared capacity is
// enforced per bucket and an overflow comes back as ErrAlreadyExists — so that
// a unit test can cover the branch without a database. The real guarantee is
// the DB CHECK; see capacity_integration_test.go for the test that proves it.
type fakeCapacity struct {
	holds map[uuid.UUID][]domain.BookingCapacityHold // by booking
	// limits mirrors restaurant_capacity_buckets.seats_limit: re-stamped by
	// every accepted claim and never lowered by a release, exactly as
	// apply_capacity_delta does. Without it the fake cannot show the one thing
	// the override mechanism turns on — that a raised limit is NOT inherited by
	// the next booking.
	limits    map[time.Time]int
	overrides []domain.BookingCapacityOverride
	err       error
	locked    []uuid.UUID // venues whose capacity lock was taken, in order
}

// LockVenue records that the lock was taken. The fake cannot reproduce the real
// serialisation, but a usecase that forgets to lock is still caught: the
// counter is asserted where the ordering matters.
func (f *fakeCapacity) LockVenue(_ context.Context, restaurantID uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.locked = append(f.locked, restaurantID)
	return nil
}

func newFakeCapacity() *fakeCapacity {
	return &fakeCapacity{
		holds:  map[uuid.UUID][]domain.BookingCapacityHold{},
		limits: map[time.Time]int{},
	}
}

func (f *fakeCapacity) taken(exclude uuid.UUID) map[time.Time]int {
	out := map[time.Time]int{}
	for bookingID, hs := range f.holds {
		if bookingID == exclude {
			continue
		}
		for _, h := range hs {
			if h.Active {
				out[h.BucketStart.UTC()] += h.Seats
			}
		}
	}
	return out
}

func (f *fakeCapacity) Create(_ context.Context, holds []domain.BookingCapacityHold) error {
	if f.err != nil {
		return f.err
	}
	if len(holds) == 0 {
		return nil
	}
	taken := f.taken(holds[0].BookingID)
	for _, h := range holds {
		if taken[h.BucketStart.UTC()]+h.Seats > h.SeatsLimit {
			return fmt.Errorf("%w: capacity", domain.ErrAlreadyExists)
		}
	}
	for _, h := range holds {
		h.Active = true
		f.holds[h.BookingID] = append(f.holds[h.BookingID], h)
		f.limits[h.BucketStart.UTC()] = h.SeatsLimit
	}
	return nil
}

// RecordOverride is append-only and unique per booking, like the table.
func (f *fakeCapacity) RecordOverride(_ context.Context, o domain.BookingCapacityOverride) error {
	if f.err != nil {
		return f.err
	}
	for _, existing := range f.overrides {
		if existing.BookingID == o.BookingID {
			return fmt.Errorf("%w: capacity override", domain.ErrAlreadyExists)
		}
	}
	f.overrides = append(f.overrides, o)
	return nil
}

func (f *fakeCapacity) ListOverrides(_ context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.BookingCapacityOverride, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.BookingCapacityOverride
	for _, o := range f.overrides {
		if o.RestaurantID == restaurantID &&
			!o.PeakBucketStart.Before(from) && o.PeakBucketStart.Before(to) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeCapacity) ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, holds []domain.BookingCapacityHold) error {
	prev := f.holds[bookingID]
	delete(f.holds, bookingID)
	for i := range holds {
		holds[i].BookingID = bookingID
	}
	if err := f.Create(ctx, holds); err != nil {
		f.holds[bookingID] = prev
		return err
	}
	return nil
}

func (f *fakeCapacity) ListByBooking(_ context.Context, bookingID uuid.UUID) ([]domain.BookingCapacityHold, error) {
	return f.holds[bookingID], f.err
}

func (f *fakeCapacity) ListUsage(_ context.Context, _ uuid.UUID, from, to time.Time) ([]domain.CapacityUsage, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.CapacityUsage
	for bucket, seats := range f.taken(uuid.Nil) {
		if bucket.Before(from) || !bucket.Before(to) {
			continue
		}
		out = append(out, domain.CapacityUsage{BucketStart: bucket, SeatsTaken: seats, SeatsLimit: f.limits[bucket]})
	}
	return out, nil
}

func (f *fakeCapacity) PeakTaken(_ context.Context, _ uuid.UUID, from time.Time) (*domain.CapacityUsage, error) {
	if f.err != nil {
		return nil, f.err
	}
	var peak *domain.CapacityUsage
	for bucket, seats := range f.taken(uuid.Nil) {
		if bucket.Before(from) {
			continue
		}
		if peak == nil || seats > peak.SeatsTaken {
			peak = &domain.CapacityUsage{BucketStart: bucket, SeatsTaken: seats}
		}
	}
	return peak, nil
}

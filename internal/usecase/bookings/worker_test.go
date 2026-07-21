package bookings

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type workerHarness struct {
	w        *Worker
	bookings *fakeBookings
	history  *fakeHistory
	outbox   *fakeOutbox
	tx       *fakeTx
	rests    *fakeRestaurants
	now      time.Time
}

// newWorkerHarness wires a worker over the fakes with a frozen clock. Venues:
// autoRest has auto_confirm on (the global default), manualRest has it off.
func newWorkerHarness(t *testing.T, bs ...*domain.Booking) *workerHarness {
	t.Helper()
	h := &workerHarness{
		bookings: newFakeBookings(bs...),
		history:  &fakeHistory{},
		outbox:   &fakeOutbox{},
		tx:       &fakeTx{},
		rests:    &fakeRestaurants{byID: map[uuid.UUID]*domain.RestaurantAggregate{}},
		now:      time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	}
	h.w = NewWorker(h.bookings, h.history, h.outbox, h.rests, h.tx,
		testConfig(),
		WorkerConfig{TickInterval: time.Minute, NoShowGrace: 30 * time.Minute, BatchSize: 100},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.w.now = func() time.Time { return h.now }
	return h
}

// venue registers a restaurant with an optional auto_confirm / SLA override.
func (h *workerHarness) venue(id uuid.UUID, autoConfirm *bool, slaMinutes *int) {
	h.rests.byID[id] = &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
		ID: id, IsActive: true,
		BookingPolicy: domain.BookingPolicyOverride{
			AutoConfirm:       autoConfirm,
			ConfirmSLAMinutes: slaMinutes,
		},
	}}
}

func (h *workerHarness) statusOf(id uuid.UUID) domain.BookingStatus {
	return h.bookings.byID[id].Status
}

func ptrBool(b bool) *bool { return &b }
func ptrInt(i int) *int    { return &i }

// booking builds a booking with explicit created_at / starts_at offsets from
// the harness clock.
func (h *workerHarness) booking(rid uuid.UUID, status domain.BookingStatus, createdAgo, endedAgo time.Duration) *domain.Booking {
	end := h.now.Add(-endedAgo)
	return &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, Name: "Дамир", Phone: "+77071234567",
		PhoneNormalized: "+77071234567", Guests: 2, Status: status,
		Source:   domain.SourceApp,
		StartsAt: end.Add(-2 * time.Hour), EndsAt: end,
		CreatedAt: h.now.Add(-createdAgo), UpdatedAt: h.now.Add(-createdAgo),
	}
}

// Confirm SLA: expired + auto_confirm on → confirmed, with history and outbox
// written in one transaction.
func TestWorkerAutoConfirmsAfterSLA(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil) // default policy: SLA 120m, auto_confirm true
	b := h.booking(rid, domain.BookingPending, 3*time.Hour, -5*time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Confirmed != 1 || res.Escalated != 0 {
		t.Fatalf("got %+v, want 1 confirmed", res)
	}
	if got := h.statusOf(b.ID); got != domain.BookingConfirmed {
		t.Fatalf("status = %s, want confirmed", got)
	}
	if len(h.history.created) != 1 {
		t.Fatalf("history rows = %d, want 1", len(h.history.created))
	}
	hist := h.history.created[0]
	if hist.ActorType != domain.ActorSystem || hist.ToStatus != domain.BookingConfirmed ||
		hist.FromStatus == nil || *hist.FromStatus != domain.BookingPending {
		t.Fatalf("unexpected history row: %+v", hist)
	}
	if types := h.outbox.types(); len(types) != 1 || types[0] != domain.EventBookingConfirmed {
		t.Fatalf("outbox = %v, want [booking.confirmed]", types)
	}
	if h.tx.calls == 0 {
		t.Fatal("transition ran outside a transaction")
	}
}

// auto_confirm = false must NOT confirm: the booking stays pending and the
// venue gets exactly one escalation event, however many ticks run.
func TestWorkerEscalatesWhenAutoConfirmOff(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, ptrBool(false), nil)
	b := h.booking(rid, domain.BookingPending, 3*time.Hour, -5*time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Confirmed != 0 || res.Escalated != 1 {
		t.Fatalf("got %+v, want 0 confirmed / 1 escalated", res)
	}
	if got := h.statusOf(b.ID); got != domain.BookingPending {
		t.Fatalf("status = %s, want pending (auto_confirm off must not confirm)", got)
	}
	if len(h.history.created) != 0 {
		t.Fatalf("escalation must not write status history, got %d rows", len(h.history.created))
	}
	if types := h.outbox.types(); len(types) != 1 || types[0] != domain.EventBookingEscalated {
		t.Fatalf("outbox = %v, want [booking.confirm_sla_breached]", types)
	}

	// Second pass: no duplicate escalation.
	h.now = h.now.Add(time.Minute)
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if n := len(h.outbox.created); n != 1 {
		t.Fatalf("outbox events after two ticks = %d, want 1 (at-most-once escalation)", n)
	}
}

// A booking whose SLA has not elapsed yet is claimed but left alone.
func TestWorkerLeavesFreshPendingAlone(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.booking(rid, domain.BookingPending, 10*time.Minute, -5*time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Confirmed != 0 || res.Escalated != 0 || res.Skipped != 1 {
		t.Fatalf("got %+v, want only skipped", res)
	}
	if got := h.statusOf(b.ID); got != domain.BookingPending {
		t.Fatalf("status = %s, want pending", got)
	}
	if len(h.outbox.created) != 0 {
		t.Fatalf("outbox must stay empty, got %v", h.outbox.types())
	}
}

// The venue's own SLA override wins over the env default.
func TestWorkerHonoursVenueSLAOverride(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, ptrInt(15)) // 15 minutes instead of the global 120
	b := h.booking(rid, domain.BookingPending, 20*time.Minute, -5*time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Confirmed != 1 {
		t.Fatalf("got %+v, want 1 confirmed with a 15m SLA", res)
	}
}

// waitlist follows the same SLA path as pending.
func TestWorkerAutoConfirmsWaitlist(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.booking(rid, domain.BookingWaitlist, 3*time.Hour, -5*time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Confirmed != 1 || h.statusOf(b.ID) != domain.BookingConfirmed {
		t.Fatalf("waitlist not confirmed: %+v / %s", res, h.statusOf(b.ID))
	}
}

// Expiry pass: arrived → completed, confirmed → no_show, and nothing inside the
// grace window is touched.
func TestWorkerClosesExpiredBookings(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)

	arrived := h.booking(rid, domain.BookingArrived, 5*time.Hour, time.Hour)
	confirmed := h.booking(rid, domain.BookingConfirmed, 5*time.Hour, time.Hour)
	// Ended 10 minutes ago — inside the 30-minute grace, must not be touched.
	fresh := h.booking(rid, domain.BookingConfirmed, 5*time.Hour, 10*time.Minute)
	// Already terminal: never claimed.
	done := h.booking(rid, domain.BookingCompleted, 5*time.Hour, time.Hour)
	for _, b := range []*domain.Booking{arrived, confirmed, fresh, done} {
		h.bookings.byID[b.ID] = b
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Completed != 1 || res.NoShow != 1 {
		t.Fatalf("got %+v, want 1 completed / 1 no_show", res)
	}
	if got := h.statusOf(arrived.ID); got != domain.BookingCompleted {
		t.Fatalf("arrived → %s, want completed", got)
	}
	if got := h.statusOf(confirmed.ID); got != domain.BookingNoShow {
		t.Fatalf("confirmed → %s, want no_show", got)
	}
	if got := h.statusOf(fresh.ID); got != domain.BookingConfirmed {
		t.Fatalf("booking inside the grace window changed to %s", got)
	}
	if got := h.statusOf(done.ID); got != domain.BookingCompleted {
		t.Fatalf("terminal booking changed to %s", got)
	}
	if len(h.history.created) != 2 {
		t.Fatalf("history rows = %d, want 2", len(h.history.created))
	}
}

// Each pass must claim on the column it actually reasons about, with the right
// cutoff: the abandoned and expiry passes on ends_at + grace, the confirm-SLA
// pass on created_at up to now.
func TestWorkerClaimColumnsAndCutoffs(t *testing.T) {
	h := newWorkerHarness(t)
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.bookings.claims) != 3 {
		t.Fatalf("claims = %d, want 3 (abandoned + sla + expiry)", len(h.bookings.claims))
	}
	grace := h.now.Add(-30 * time.Minute)
	want := []struct {
		by     domain.ClaimColumn
		before time.Time
	}{
		{domain.ClaimByEndsAt, grace},    // abandoned
		{domain.ClaimByCreatedAt, h.now}, // confirm SLA
		{domain.ClaimByEndsAt, grace},    // expiry
	}
	for i, w := range want {
		got := h.bookings.claims[i]
		if got.by != w.by || !got.before.Equal(w.before) {
			t.Errorf("claim %d = (%s, %s), want (%s, %s)", i, got.by, got.before, w.by, w.before)
		}
	}
}

// A pending booking the venue never answered and whose visit window is long
// past must not stay pending forever: no_show is unreachable from pending, so
// the worker closes it as cancelled, attributed to the system.
func TestWorkerCancelsAbandonedBookings(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, ptrBool(false), nil) // auto_confirm off: nobody ever answered

	abandoned := h.booking(rid, domain.BookingPending, 30*time.Hour, time.Hour)
	waitlisted := h.booking(rid, domain.BookingWaitlist, 30*time.Hour, time.Hour)
	// Ended 10 minutes ago — still inside the grace window, hands off. Created
	// just now too, so the confirm-SLA pass has no opinion about it either.
	fresh := h.booking(rid, domain.BookingPending, 10*time.Minute, 10*time.Minute)
	for _, b := range []*domain.Booking{abandoned, waitlisted, fresh} {
		h.bookings.byID[b.ID] = b
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Abandoned != 2 {
		t.Fatalf("got %+v, want 2 abandoned", res)
	}
	for _, b := range []*domain.Booking{abandoned, waitlisted} {
		if got := h.statusOf(b.ID); got != domain.BookingCancelled {
			t.Fatalf("abandoned booking → %s, want cancelled", got)
		}
	}
	if got := h.statusOf(fresh.ID); got != domain.BookingPending {
		t.Fatalf("booking inside the grace window changed to %s", got)
	}

	// Attribution: system, with a reason a human can read.
	if n := len(h.bookings.updated); n != 2 {
		t.Fatalf("metadata writes = %d, want 2", n)
	}
	for _, u := range h.bookings.updated {
		if u.CancelledBy == nil || *u.CancelledBy != domain.CancelledBySystem {
			t.Fatalf("cancelled_by = %v, want system", u.CancelledBy)
		}
		if u.CancelledAt == nil || !u.CancelledAt.Equal(h.now) {
			t.Fatalf("cancelled_at = %v, want %s", u.CancelledAt, h.now)
		}
		if u.CancellationReason == nil || *u.CancellationReason != abandonedReason {
			t.Fatalf("cancellation_reason = %v, want %q", u.CancellationReason, abandonedReason)
		}
	}
	for _, c := range h.history.created {
		if c.ActorType != domain.ActorSystem || c.ToStatus != domain.BookingCancelled ||
			c.Reason == nil || *c.Reason != abandonedReason {
			t.Fatalf("history row = %+v", c)
		}
	}
	if types := h.outbox.types(); len(types) != 2 ||
		types[0] != domain.EventBookingCancelled || types[1] != domain.EventBookingCancelled {
		t.Fatalf("outbox = %v, want two booking.cancelled events", types)
	}

	// Idempotent: a second tick finds nothing left to do.
	before := len(h.outbox.created)
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(h.outbox.created) != before {
		t.Fatalf("second tick emitted %d extra events", len(h.outbox.created)-before)
	}
}

// An abandoned booking must be cancelled rather than auto-confirmed into a
// visit that already happened (and then blamed on the guest as a no-show), even
// at a venue with auto_confirm on. This is why the abandoned pass runs first.
func TestWorkerAbandonedBeatsAutoConfirm(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, ptrBool(true), nil)
	b := h.booking(rid, domain.BookingPending, 30*time.Hour, time.Hour)
	h.bookings.byID[b.ID] = b

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Abandoned != 1 || res.Confirmed != 0 || res.NoShow != 0 {
		t.Fatalf("got %+v, want 1 abandoned and nothing else", res)
	}
	if got := h.statusOf(b.ID); got != domain.BookingCancelled {
		t.Fatalf("status = %s, want cancelled", got)
	}
}

// Starvation guard: when the batch is smaller than the candidate set, the
// bookings that have waited LONGEST must be the ones processed. The old query
// ordered by starts_at while cutting off on created_at, so a genuinely overdue
// request could be pushed out of every batch forever by fresher ones that
// happen to start sooner.
func TestWorkerConfirmSLADoesNotStarveOldest(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	h.w.wcfg.BatchSize = 1

	// oldest was created 10h ago but is booked for the far future; newer was
	// created 3h ago for a table tonight. Ordering by starts_at would pick
	// newer every single tick.
	oldest := h.booking(rid, domain.BookingPending, 10*time.Hour, -100*time.Hour)
	newer := h.booking(rid, domain.BookingPending, 3*time.Hour, -5*time.Hour)
	h.bookings.byID[oldest.ID] = oldest
	h.bookings.byID[newer.ID] = newer

	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := h.statusOf(oldest.ID); got != domain.BookingConfirmed {
		t.Fatalf("oldest pending is %s, want confirmed first — it is being starved", got)
	}
	if got := h.statusOf(newer.ID); got != domain.BookingPending {
		t.Fatalf("newer booking = %s, want pending (batch size is 1)", got)
	}

	// It drains: the next tick takes the one that is now the oldest.
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := h.statusOf(newer.ID); got != domain.BookingConfirmed {
		t.Fatalf("newer booking = %s after the second tick, want confirmed", got)
	}
}

// A repository failure surfaces from Tick rather than being swallowed.
func TestWorkerTickPropagatesError(t *testing.T) {
	h := newWorkerHarness(t)
	h.bookings.claimErr = errors.New("db down")
	if _, err := h.w.Tick(context.Background()); err == nil {
		t.Fatal("want an error from Tick")
	}
}

// Run must return as soon as the context is cancelled (graceful shutdown).
func TestWorkerRunStopsOnContextCancel(t *testing.T) {
	h := newWorkerHarness(t)
	h.w.wcfg.TickInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

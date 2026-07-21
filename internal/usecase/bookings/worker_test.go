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

// The expiry cutoff must be ends_at + grace, not now.
func TestWorkerExpiryCutoffUsesGrace(t *testing.T) {
	h := newWorkerHarness(t)
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.bookings.claims) != 2 {
		t.Fatalf("claims = %d, want 2 (sla pass + expiry pass)", len(h.bookings.claims))
	}
	if want := h.now.Add(-30 * time.Minute); !h.bookings.claims[1].before.Equal(want) {
		t.Fatalf("expiry cutoff = %s, want %s", h.bookings.claims[1].before, want)
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

package bookings

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeReminders mirrors postgres/booking.Reminders closely enough to prove the
// pass's contract: the claim predicate (live status, has an account, visit
// inside the window, not booked inside its own window) and — the part that
// matters most — MarkReminderSent as a CONDITIONAL stamp that answers false the
// second time. If the production guard (`guest_reminder_sent_at IS NULL`) is
// dropped, this fake's `stamped` check is what the tests below detect.
type fakeReminders struct {
	rows    map[uuid.UUID]*domain.Booking
	stamped map[uuid.UUID]time.Time
	claims  int
	// claimIgnoresStamp models the ONE case the claim predicate cannot cover:
	// the marker was written by somebody else (a second worker process, a
	// retried pass) AFTER this pass read the row. The claim then hands back a
	// booking that is already reminded, and only the conditional stamp can stop
	// a duplicate.
	claimIgnoresStamp bool
}

func newFakeReminders(bs ...*domain.Booking) *fakeReminders {
	f := &fakeReminders{rows: map[uuid.UUID]*domain.Booking{}, stamped: map[uuid.UUID]time.Time{}}
	for _, b := range bs {
		f.rows[b.ID] = b
	}
	return f
}

func (f *fakeReminders) ClaimDueReminders(_ context.Context, from, to time.Time, limit int) ([]domain.Booking, error) {
	f.claims++
	var out []domain.Booking
	for _, b := range f.rows {
		if len(out) >= limit {
			break
		}
		if _, done := f.stamped[b.ID]; done && !f.claimIgnoresStamp {
			continue
		}
		if b.UserID == nil || !reminderLive(b.Status) {
			continue
		}
		if !b.StartsAt.After(from) || b.StartsAt.After(to) {
			continue
		}
		// Booked inside its own reminder window → not reminded.
		if b.CreatedAt.After(b.StartsAt.Add(-to.Sub(from))) {
			continue
		}
		out = append(out, *b)
	}
	return out, nil
}

func (f *fakeReminders) MarkReminderSent(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	b, ok := f.rows[id]
	if !ok || !reminderLive(b.Status) {
		return false, nil
	}
	if _, done := f.stamped[id]; done {
		return false, nil
	}
	f.stamped[id] = at
	return true, nil
}

func reminderLive(s domain.BookingStatus) bool {
	return s == domain.BookingPending || s == domain.BookingConfirmed || s == domain.BookingWaitlist
}

// reminderEvents counts booking.reminder rows written to the outbox.
func reminderEvents(o *fakeOutbox) []domain.BookingOutboxEvent {
	var out []domain.BookingOutboxEvent
	for _, e := range o.created {
		if e.EventType == domain.EventBookingReminder {
			out = append(out, e)
		}
	}
	return out
}

// upcoming builds a confirmed booking with an account, starting `in` from the
// harness clock and created long before its reminder window.
func (h *workerHarness) upcoming(rid uuid.UUID, in time.Duration) *domain.Booking {
	uid := uuid.New()
	return &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &uid,
		Name: "Дамир", Phone: "+77071234567", PhoneNormalized: "+77071234567",
		Guests: 2, Status: domain.BookingConfirmed, Source: domain.SourceApp,
		StartsAt: h.now.Add(in), EndsAt: h.now.Add(in + 2*time.Hour),
		CreatedAt: h.now.Add(-48 * time.Hour), UpdatedAt: h.now.Add(-48 * time.Hour),
	}
}

// withReminders rebuilds the harness worker with the reminder pass wired.
func (h *workerHarness) withReminders(r domain.BookingReminderRepository, lead time.Duration) {
	h.w.reminders = r
	h.w.wcfg.ReminderLead = lead
}

// The happy path: a visit inside the lead window produces exactly one
// booking.reminder outbox event, and the booking's status is untouched (the
// reminder pass is the one pass that changes no status).
func TestWorkerEmitsGuestReminder(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.upcoming(rid, 45*time.Minute)
	h.withReminders(newFakeReminders(b), time.Hour)

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reminded != 1 {
		t.Fatalf("reminded = %d, want 1", res.Reminded)
	}
	evs := reminderEvents(h.outbox)
	if len(evs) != 1 || evs[0].BookingID != b.ID {
		t.Fatalf("outbox reminder events = %+v, want exactly one for %s", h.outbox.types(), b.ID)
	}
	if b.Status != domain.BookingConfirmed {
		t.Fatalf("status changed to %s, the reminder pass must not touch it", b.Status)
	}
}

// IDEMPOTENCY. A second tick over the same booking must emit NOTHING: the
// marker written by the first tick is the arbiter, so the reminder survives a
// process restart without being re-sent.
//
// This test fails if MarkReminderSent stops being conditional.
func TestWorkerRemindsOnlyOncePerBooking(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.upcoming(rid, 45*time.Minute)
	h.withReminders(newFakeReminders(b), time.Hour)

	first, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	second, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if first.Reminded != 1 || second.Reminded != 0 {
		t.Fatalf("reminded first=%d second=%d, want 1 then 0", first.Reminded, second.Reminded)
	}
	if got := len(reminderEvents(h.outbox)); got != 1 {
		t.Fatalf("outbox holds %d reminder events after two ticks, want exactly 1", got)
	}
}

// IDEMPOTENCY, the racing case. The claim hands back a booking another process
// has just stamped; MarkReminderSent answers false and the pass must emit
// NOTHING. This is the test that fails if the `if !ok { continue }` guard in
// processReminders is dropped — the previous test cannot catch that, because
// the claim predicate alone already hides an already-stamped row.
func TestWorkerReminderStampIsTheArbiterUnderARace(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.upcoming(rid, 45*time.Minute)
	reminders := newFakeReminders(b)
	reminders.claimIgnoresStamp = true
	h.withReminders(reminders, time.Hour)

	// Somebody else got there first.
	if ok, err := reminders.MarkReminderSent(context.Background(), b.ID, h.now); err != nil || !ok {
		t.Fatalf("pre-stamp: ok=%v err=%v", ok, err)
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reminded != 0 || len(reminderEvents(h.outbox)) != 0 {
		t.Fatalf("an already-stamped booking was reminded again (reminded=%d, events=%d)",
			res.Reminded, len(reminderEvents(h.outbox)))
	}
}

// A cancelled booking is never reminded, even when its visit time is inside the
// reminder window: it is filtered by the claim AND rejected by the stamp.
func TestWorkerNeverRemindsCancelledBooking(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.upcoming(rid, 45*time.Minute)
	b.Status = domain.BookingCancelled
	reminders := newFakeReminders(b)
	h.withReminders(reminders, time.Hour)

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reminded != 0 || len(reminderEvents(h.outbox)) != 0 {
		t.Fatalf("cancelled booking got a reminder (reminded=%d, events=%d)",
			res.Reminded, len(reminderEvents(h.outbox)))
	}
	// Second line of defence: even if the claim ever returned it, the stamp
	// refuses a booking that is no longer live.
	ok, err := reminders.MarkReminderSent(context.Background(), b.ID, h.now)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if ok {
		t.Fatal("MarkReminderSent accepted a cancelled booking")
	}
}

// A booking outside the lead window, one without a guest account, and one made
// inside its own reminder window are all left alone.
func TestWorkerReminderClaimBoundaries(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)

	tooFar := h.upcoming(rid, 3*time.Hour)
	past := h.upcoming(rid, -10*time.Minute)
	noAccount := h.upcoming(rid, 30*time.Minute)
	noAccount.UserID = nil
	lastMinute := h.upcoming(rid, 20*time.Minute)
	lastMinute.CreatedAt = h.now.Add(-5 * time.Minute) // booked inside the window

	h.withReminders(newFakeReminders(tooFar, past, noAccount, lastMinute), time.Hour)

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reminded != 0 || len(reminderEvents(h.outbox)) != 0 {
		t.Fatalf("reminded=%d events=%d, want none of the four to be reminded",
			res.Reminded, len(reminderEvents(h.outbox)))
	}
}

// The pass is optional: a worker built without WithGuestReminders does nothing
// instead of panicking on a nil repository (deployments that have not run
// migration 0049 yet).
func TestWorkerReminderPassIsOptional(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reminded != 0 {
		t.Fatalf("reminded = %d without a reminder repository, want 0", res.Reminded)
	}
}

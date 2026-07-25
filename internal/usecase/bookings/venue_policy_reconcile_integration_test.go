package bookings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
)

// The mode-switch reconciliation used to read the venue's bookings with
// OFFSET pagination (100 per page). That is a correctness bug, not a style
// question, and these two tests are its reproduction and its guard.
//
// Why it was a bug, in the order the pieces matter:
//
//  1. sqltx.Manager begins the transaction with no TxOptions, i.e. READ
//     COMMITTED — every statement inside it gets a FRESH snapshot.
//  2. capacity.LockVenue serializes booking CREATION only. status.go never
//     takes that lock, so a guest cancelling a booking runs freely alongside a
//     switch.
//  3. so a cancel that commits between page 1 and page 2 removes a row from the
//     result set, every later row shifts one place up, and the row that sat
//     exactly on the page boundary is never returned by any page.
//  4. that booking then leaves the switch with no capacity holds (seats mode:
//     invisible to the ledger) or no booking_tables row (tables mode: invisible
//     to the GiST constraint) — and the venue sells its seats a second time.
//
// cancellingLister injects step 3 at the port boundary — it decorates the real
// repository and commits a cancel on its own connection after the first read
// returns. Nothing in production code is contorted for the test: the hook lives
// entirely in the decorator, and the decorated call is the ordinary one.
type cancellingLister struct {
	inner *bookingrepo.Repository
	pool  *pgxpool.Pool
	t     *testing.T

	// cancelAfterFirstRead is cancelled, out of band and committed, right after
	// the FIRST read of the switch returns.
	cancelAfterFirstRead uuid.UUID

	reads      int
	pagedReads int
}

func (l *cancellingLister) ListLiveForReconcile(
	ctx context.Context,
	restaurantID uuid.UUID,
	from time.Time,
	statuses []domain.BookingStatus,
	limit int,
) ([]domain.Booking, error) {
	out, err := l.inner.ListLiveForReconcile(ctx, restaurantID, from, statuses, limit)
	l.afterRead()
	return out, err
}

// List is here so the decorator still satisfies the OLD, paginated
// implementation: reverting liveBookings to it must make these tests fail, and
// that is how the reproduction was verified.
func (l *cancellingLister) List(ctx context.Context, f domain.BookingFilter) ([]domain.Booking, int, error) {
	l.pagedReads++
	out, total, err := l.inner.List(ctx, f)
	l.afterRead()
	return out, total, err
}

func (l *cancellingLister) afterRead() {
	l.reads++
	if l.reads != 1 || l.cancelAfterFirstRead == uuid.Nil {
		return
	}
	// A separate connection from the pool: this UPDATE is its own transaction
	// and commits immediately, exactly like a guest tapping "cancel" while the
	// owner is flipping the toggle. The switch's transaction is untouched by it
	// (no rows in common — nothing has written holds yet at this point).
	if _, err := l.pool.Exec(context.Background(),
		`UPDATE bookings SET status='cancelled', cancelled_at=now() WHERE id=$1`,
		l.cancelAfterFirstRead); err != nil {
		l.t.Fatalf("out-of-band cancel: %v", err)
	}
}

// reconcileHarness is the guarantee harness with the booking lister decorated.
type reconcileHarness struct {
	*guaranteeHarness
	lister *cancellingLister
	policy PolicyUseCase
}

func newReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	h := newGuaranteeHarness(t, "tables", 0)
	lister := &cancellingLister{inner: bookingrepo.New(h.pool), pool: h.pool, t: t}
	return &reconcileHarness{
		guaranteeHarness: h,
		lister:           lister,
		policy: NewPolicyUseCase(h.restRepo, h.restRepo,
			newFakeManagers([2]uuid.UUID{h.manager.UserID, h.restaurantID}),
			h.related, h.capacity, bookingrepo.NewTables(h.pool), lister, h.txm, testConfig()),
	}
}

// seedLive writes n confirmed bookings of one guest each, spread over distinct
// start times so their order is total and reproducible. One guest each keeps the
// bucket CHECK out of the way: what is under test is which bookings the switch
// SEES, not whether they fit.
func (h *reconcileHarness) seedLive(t *testing.T, n int) []domain.Booking {
	t.Helper()
	base := h.slotAt(t, 12)
	out := make([]domain.Booking, 0, n)
	for i := 0; i < n; i++ {
		b := h.seedBooking(t, base.Add(time.Duration(i)*time.Minute), 1, domain.BookingConfirmed)
		out = append(out, *b)
	}
	return out
}

// TestModeSwitchDoesNotSkipABookingWhenOneIsCancelledMidRead is the
// reproduction. Verified to FAIL against the paginated implementation it
// replaced (120 live bookings, a cancel committed between page 1 and page 2:
// two reads instead of one, and one live booking left with zero holds).
func TestModeSwitchDoesNotSkipABookingWhenOneIsCancelledMidRead(t *testing.T) {
	h := newReconcileHarness(t)
	ctx := context.Background()

	// More than one page of the old implementation (100), so that a shifted
	// OFFSET window has a row to hide.
	const n = 120
	seeded := h.seedLive(t, n)

	// The old read order was `starts_at DESC`, so its page 1 held the LATEST 100
	// bookings. Cancelling one of them shifts every later row up by one and the
	// row on the page boundary falls into the gap between the pages.
	victim := seeded[len(seeded)-1]
	h.lister.cancelAfterFirstRead = victim.ID

	if _, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(500)); err != nil {
		t.Fatalf("switch to seats: %v", err)
	}

	// One statement, one snapshot. This assertion is the invariant: the moment
	// somebody pages this read again, it breaks.
	if h.lister.reads != 1 {
		t.Errorf("the switch read the booking set %d times, want exactly 1 (paged reads under READ COMMITTED skip rows)", h.lister.reads)
	}
	if h.lister.pagedReads != 0 {
		t.Errorf("the switch used the paginated List %d times, want 0", h.lister.pagedReads)
	}

	// And the property that actually protects the venue: every booking that is
	// still live now holds seats in the ledger.
	var missing []uuid.UUID
	for _, b := range seeded {
		if b.ID == victim.ID {
			continue // legitimately cancelled mid-switch; its holds are inert
		}
		if len(h.holds(t, b.ID)) == 0 {
			missing = append(missing, b.ID)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d live bookings came out of the switch with no capacity holds (%v) — their seats will be sold twice",
			len(missing), n-1, missing)
	}
}

// TestModeSwitchRefusalNamesTheOffendingBooking pins the refusal that comes out
// of the bucket CHECK. It used to say only "the bookings already accepted for
// <time> exceed a capacity of N", which tells staff that something does not fit
// but not WHAT — with two parties in the same slot there is no way to act on it.
// The booking id and the overloaded window are both free to report (the booking
// is the one in hand, the window is its own occupancy window), so they are.
func TestModeSwitchRefusalNamesTheOffendingBooking(t *testing.T) {
	h := newReconcileHarness(t)
	ctx := context.Background()

	// Two parties of 6 in the same slot: each fits a capacity of 8 on its own,
	// together they do not, so the SECOND one is refused by the bucket CHECK
	// rather than by the per-booking guests check above it.
	start := h.slotAt(t, 12)
	first := h.seedBooking(t, start, 6, domain.BookingConfirmed)
	second := h.seedBooking(t, start, 6, domain.BookingConfirmed)

	_, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(8))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("switch that oversells the room = %v, want ErrValidation", err)
	}
	msg := err.Error()
	// liveBookings orders by starts_at then id, so which of the two trips the
	// CHECK depends on their ids — either one is a correct answer, a message
	// naming NEITHER is not.
	if !strings.Contains(msg, first.ID.String()) && !strings.Contains(msg, second.ID.String()) {
		t.Errorf("refusal %q names no booking", msg)
	}
	for _, want := range []string{"guests", "capacity of 8", "between"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
}

// TestModeSwitchRefusesAVenueBeyondTheReconcileLimit pins the other half of the
// single-statement read: it has to be bounded, and reaching the bound must be a
// loud refusal rather than a switch that runs past the 15s write timeout with
// the venue's advisory lock in hand (which would block every booking create
// meanwhile, and roll back anyway).
func TestModeSwitchRefusesAVenueBeyondTheReconcileLimit(t *testing.T) {
	h := newReconcileHarness(t)
	ctx := context.Background()

	h.seedLive(t, domain.MaxReconcileBookings)

	_, err := h.policy.Update(ctx, h.manager, h.restaurantID, seatsOverride(1000))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("switch on an oversized venue = %v, want ErrValidation", err)
	}
	// The message is what staff read, so it is part of the contract: how many
	// bookings, what the limit is, and what to do next.
	for _, want := range []string{"live bookings", "contact support"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}

	// Refused means refused: the mode is unchanged and not one hold was written.
	view, err := h.policy.Get(ctx, h.manager, h.restaurantID)
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if view.Effective.CapacityMode != domain.CapacityModeTables {
		t.Errorf("capacity mode = %q after a refused switch, want %q",
			view.Effective.CapacityMode, domain.CapacityModeTables)
	}
	var holds int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM booking_capacity_holds`).Scan(&holds); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	if holds != 0 {
		t.Errorf("a refused switch wrote %d capacity holds, want 0", holds)
	}
}

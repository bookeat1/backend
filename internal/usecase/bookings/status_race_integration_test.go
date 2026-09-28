package bookings

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// TestConfirmVersusWorkerCancelRace_CASPreventsMoneylessConfirm is the money-race
// tech-lead flagged: the venue confirms a booking through the ordinary
// StatusUseCase path at the SAME instant the confirm-SLA worker cancels the
// same booking for having run past its visit window unanswered
// (Worker.processAbandoned). Both paths read the booking's status with NO row
// lock and decide what to write before opening their own transaction — the only
// thing standing between them and a lost update is
// BookingRepository.CompareAndSwapStatus's `WHERE id=$1 AND status=$2`
// (repository.go). Without it (plain UpdateStatus, `WHERE id=$1`), whichever
// transaction's UPDATE statement is issued LAST always wins regardless of which
// one the row lock let through first, because Postgres re-evaluates a bare
// `WHERE id=$1` against nothing but the id. A worker's cancellation that already
// released the pre-order hold (real money, settled outside this test) can be
// silently overwritten back to `confirmed` a moment later, leaving a confirmed
// booking whose money is gone.
//
// The two transitions run through TWO INDEPENDENT real Postgres connections and
// TWO INDEPENDENT transactions: the worker's ClaimDue takes FOR UPDATE SKIP
// LOCKED and holds it until its own transaction commits, so the venue's
// transaction blocks on the row and then, with the CAS fix, discovers on
// re-evaluation that `status` is no longer `pending` and aborts instead of
// clobbering the commit that already happened.
//
// FALSE-GREEN CHECK (run by hand while writing this test, 2026-09-28): temporarily
// reverting statusUseCase.transition and Worker.transition to call
// bookings.UpdateStatus instead of bookings.CompareAndSwapStatus makes this test
// fail — reliably, across the whole iteration loop — with a booking left
// `status=confirmed` AND `cancelled_at`/`cancelled_by` set (the exact "confirmed
// without money" defect). Redo that revert if you ever doubt this test is
// actually exercising the guard.
func TestConfirmVersusWorkerCancelRace_CASPreventsMoneylessConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, integrationTables...)
	ctx := context.Background()

	rid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active,
			booking_capacity_mode, booking_capacity_seats)
		 VALUES ($1,'R','Алматы','₸',true,'seats',50)`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurants WHERE id=$1`, rid)
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	txm := sqltx.NewManager(pool)
	bookingsRepo := bookingrepo.New(pool)
	restaurants := restrepo.New(pool)
	history := bookingrepo.NewHistory(pool)
	outbox := bookingrepo.NewOutbox(pool)

	status := NewStatusUseCase(bookingsRepo, history, outbox, restaurants, newFakeManagers(), txm, testConfig())
	worker := NewWorker(bookingsRepo, history, outbox, restaurants, txm, testConfig(),
		WorkerConfig{NoShowGrace: time.Minute, BatchSize: 10}, log)

	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	const iterations = 25
	var venueWon, workerWon int
	for i := 0; i < iterations; i++ {
		bookingID := seedRacingPendingBooking(t, ctx, pool, rid, i)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var confirmErr error
		var tickErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, confirmErr = status.Confirm(ctx, admin, bookingID, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, tickErr = worker.Tick(ctx)
		}()
		close(start)
		wg.Wait()

		if tickErr != nil {
			t.Fatalf("iteration %d: worker tick failed: %v", i, tickErr)
		}

		var status_ string
		var confirmedAt, cancelledAt *time.Time
		var cancelledBy *string
		if err := pool.QueryRow(ctx,
			`SELECT status, confirmed_at, cancelled_at, cancelled_by FROM bookings WHERE id=$1`,
			bookingID).Scan(&status_, &confirmedAt, &cancelledAt, &cancelledBy); err != nil {
			t.Fatalf("iteration %d: read final booking: %v", i, err)
		}

		switch status_ {
		case string(domain.BookingConfirmed):
			if confirmErr != nil {
				t.Fatalf("iteration %d: booking ended confirmed but Confirm() returned %v", i, confirmErr)
			}
			if confirmedAt == nil {
				t.Fatalf("iteration %d: confirmed booking has no confirmed_at", i)
			}
			if cancelledAt != nil || cancelledBy != nil {
				t.Fatalf("iteration %d: booking is confirmed but carries cancellation metadata "+
					"(cancelled_at=%v cancelled_by=%v) — the money-race the CAS guards against", i, cancelledAt, cancelledBy)
			}
			venueWon++
		case string(domain.BookingCancelled):
			if confirmErr == nil {
				t.Fatalf("iteration %d: booking ended cancelled but Confirm() reported success", i)
			}
			if !errors.Is(confirmErr, domain.ErrInvalidStatus) {
				t.Fatalf("iteration %d: Confirm() lost the race with an unexpected error: %v", i, confirmErr)
			}
			if cancelledAt == nil || cancelledBy == nil {
				t.Fatalf("iteration %d: cancelled booking is missing cancellation metadata", i)
			}
			if confirmedAt != nil {
				t.Fatalf("iteration %d: cancelled booking also carries confirmed_at=%v — "+
					"the venue's transition partially applied", i, *confirmedAt)
			}
			workerWon++
		default:
			t.Fatalf("iteration %d: booking ended in unexpected status %q", i, status_)
		}
	}

	t.Logf("venue won %d/%d, worker won %d/%d", venueWon, iterations, workerWon, iterations)
	if venueWon == 0 || workerWon == 0 {
		t.Fatalf("race never actually interleaved both ways (venue=%d worker=%d) — "+
			"this run did not exercise the guard, widen the timing or rerun", venueWon, workerWon)
	}
}

// seedRacingPendingBooking inserts one PENDING booking whose visit window ended
// long enough ago (more than the 1-minute NoShowGrace the test's worker uses)
// for Worker.processAbandoned to claim it, so it is a legal target for BOTH the
// venue's confirm and the worker's abandon-cancel at the same time.
func seedRacingPendingBooking(t *testing.T, ctx context.Context, pool *pgxpool.Pool, restaurantID uuid.UUID, i int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now()
	startsAt := now.Add(-2 * time.Hour)
	endsAt := now.Add(-90 * time.Minute)
	phone := fmt.Sprintf("+7707%07d", (uuid.New().ID())%10000000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, name, phone, email, phone_normalized,
			guests, starts_at, ends_at, status, source)
		 VALUES ($1,$2,'Гость race',$3,'race@example.com',$3,2,$4,$5,'pending','app')`,
		id, restaurantID, phone, startsAt, endsAt); err != nil {
		t.Fatalf("seed racing booking %d: %v", i, err)
	}
	return id
}

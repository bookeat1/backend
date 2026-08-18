package booking

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/sqltx"
)

// seedOrphanBooking writes a booking with NO user_id — the shape a phone-in or
// hostess-made reservation has in production (370 of them when this shipped).
func seedOrphanBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, phoneNormalized string) uuid.UUID {
	t.Helper()
	b := newBooking(rid, time.Now().Add(24*time.Hour))
	b.PhoneNormalized = phoneNormalized
	b.Phone = phoneNormalized
	if err := New(pool).Create(context.Background(), b); err != nil {
		t.Fatalf("seed orphan booking: %v", err)
	}
	return b.ID
}

func ownerOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var owner *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT user_id FROM bookings WHERE id=$1`, id).Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	return owner
}

// TestAttachOrphanedByPhone covers the whole contract in one table-free test,
// because the guarantees only mean something together: the matching orphan must
// be taken, and in the SAME call every other row must be left exactly as it was.
func TestAttachOrphanedByPhone(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)

	const guestPhone = "+77015550001"
	const otherPhone = "+77015550002"

	guest := seedUser(t, pool)
	otherOwner := seedUser(t, pool)

	orphan := seedOrphanBooking(t, pool, rid, guestPhone)
	orphanAlso := seedOrphanBooking(t, pool, rid, guestPhone)
	strangerOrphan := seedOrphanBooking(t, pool, rid, otherPhone)
	emptyPhoneOrphan := seedOrphanBooking(t, pool, rid, "") // legacy shape

	// Already owned by somebody else, same phone: the row this rule must never
	// touch. A phone number changing hands cannot hand over a booking.
	owned := seedOrphanBooking(t, pool, rid, guestPhone)
	if _, err := pool.Exec(ctx, `UPDATE bookings SET user_id=$1 WHERE id=$2`, otherOwner, owned); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	n, err := repo.AttachOrphanedByPhone(ctx, guest, guestPhone)
	if err != nil {
		t.Fatalf("AttachOrphanedByPhone: %v", err)
	}
	if n != 2 {
		t.Fatalf("attached %d bookings, want 2", n)
	}
	for _, id := range []uuid.UUID{orphan, orphanAlso} {
		if got := ownerOf(t, pool, id); got == nil || *got != guest {
			t.Errorf("booking %s owner = %v, want %s", id, got, guest)
		}
	}
	if got := ownerOf(t, pool, owned); got == nil || *got != otherOwner {
		t.Errorf("owned booking was re-assigned: owner = %v, want %s", got, otherOwner)
	}
	if got := ownerOf(t, pool, strangerOrphan); got != nil {
		t.Errorf("booking of another phone was attached: owner = %v", *got)
	}
	if got := ownerOf(t, pool, emptyPhoneOrphan); got != nil {
		t.Errorf("empty-phone booking was attached: owner = %v", *got)
	}

	// Idempotence: the second login of the same guest has nothing left to take
	// and must not be an error.
	again, err := repo.AttachOrphanedByPhone(ctx, guest, guestPhone)
	if err != nil {
		t.Fatalf("second AttachOrphanedByPhone: %v", err)
	}
	if again != 0 {
		t.Errorf("second attach took %d bookings, want 0", again)
	}
}

// TestAttachOrphanedByPhoneEmptyPhoneMatchesNothing pins the guard separately:
// an unnormalizable number must never sweep up the legacy rows whose
// phone_normalized is ” (the column is NOT NULL, so ” is what they carry).
func TestAttachOrphanedByPhoneEmptyPhoneMatchesNothing(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)
	guest := seedUser(t, pool)

	legacy := seedOrphanBooking(t, pool, rid, "")

	n, err := repo.AttachOrphanedByPhone(ctx, guest, "")
	if err != nil {
		t.Fatalf("AttachOrphanedByPhone: %v", err)
	}
	if n != 0 {
		t.Fatalf("attached %d bookings for an empty phone, want 0", n)
	}
	if got := ownerOf(t, pool, legacy); got != nil {
		t.Errorf("legacy empty-phone booking was attached: owner = %v", *got)
	}
}

// TestAttachOrphanedByPhoneRunsInsideCallerTx proves the write joins the
// ambient transaction (sqltx.From) rather than opening its own connection — the
// property the OTP usecase depends on to commit "user created" and "user owns
// their history" together, or neither.
func TestAttachOrphanedByPhoneRunsInsideCallerTx(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)
	guest := seedUser(t, pool)

	const guestPhone = "+77015550003"
	orphan := seedOrphanBooking(t, pool, rid, guestPhone)

	errBoom := errTestRollback{}
	err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		n, err := repo.AttachOrphanedByPhone(ctx, guest, guestPhone)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("attached %d inside tx, want 1", n)
		}
		return errBoom
	})
	if err != errBoom { //nolint:errorlint // the sentinel is returned verbatim
		t.Fatalf("WithinTx err = %v, want the rollback sentinel", err)
	}
	if got := ownerOf(t, pool, orphan); got != nil {
		t.Errorf("attach survived a rolled-back transaction: owner = %v", *got)
	}
}

type errTestRollback struct{}

func (errTestRollback) Error() string { return "rollback" }

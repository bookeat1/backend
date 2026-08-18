package auth

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/domain"
)

// verifyWithSeededCode logs the phone in by writing the OTP row directly. The
// request path is skipped on purpose: RequestOTP's per-minute budget is 1 in
// this fixture, and this test needs two logins in a row.
func verifyWithSeededCode(t *testing.T, uc OTPUseCase, otpRepo domain.OTPRepository, p string) {
	t.Helper()
	const code = "424242"
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   "test",
		ExpiresAt: time.Now().Add(5 * time.Minute),
		CreatedAt: time.Now(),
	}
	if err := otpRepo.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed otp code: %v", err)
	}
	if _, err := uc.VerifyOTP(context.Background(), p, code); err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
}

// seedOrphanBooking writes a booking with no owner, the shape a phone-in or
// hostess-made reservation has. Written with raw SQL rather than the booking
// usecase on purpose: this test is about ownership, not about capacity, tables
// or notifications.
func seedOrphanBooking(t *testing.T, db *pgxpool.Pool, restaurantID uuid.UUID, phoneNormalized string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	starts := time.Now().Add(24 * time.Hour)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests,
		                       starts_at, ends_at, status, source, created_by_admin)
		 VALUES ($1,$2,'Гость',$3,$3,2,$4,$5,'confirmed','admin',true)`,
		id, restaurantID, phoneNormalized, starts, starts.Add(2*time.Hour)); err != nil {
		t.Fatalf("seed orphan booking: %v", err)
	}
	return id
}

func seedRestaurant(t *testing.T, db *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`,
		id); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func bookingOwner(t *testing.T, db *pgxpool.Pool, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var owner *uuid.UUID
	if err := db.QueryRow(context.Background(),
		`SELECT user_id FROM bookings WHERE id=$1`, id).Scan(&owner); err != nil {
		t.Fatalf("read booking owner: %v", err)
	}
	return owner
}

// TestVerifyOTPAttachesBookingsOnRealPostgres runs the whole first login against
// a live database: the user row does not exist when the transaction opens, the
// bookings do, and the FK from bookings.user_id to that brand-new user has to
// hold inside the same transaction. The in-memory noTx fake cannot make that
// claim — it has no constraints and no commit.
func TestVerifyOTPAttachesBookingsOnRealPostgres(t *testing.T) {
	uc, otpRepo, db := newRealTestOTP(t)
	ctx := context.Background()
	rid := seedRestaurant(t, db)

	const guestPhone = "+77015551111"
	mine := seedOrphanBooking(t, db, rid, guestPhone)
	someoneElse := seedOrphanBooking(t, db, rid, "+77015552222")

	verifyWithSeededCode(t, uc, otpRepo, guestPhone)

	var userID uuid.UUID
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE phone=$1`, guestPhone).Scan(&userID); err != nil {
		t.Fatalf("user was not created: %v", err)
	}
	if got := bookingOwner(t, db, mine); got == nil || *got != userID {
		t.Fatalf("booking owner = %v, want the new user %s", got, userID)
	}
	if got := bookingOwner(t, db, someoneElse); got != nil {
		t.Errorf("booking of another number was attached: owner = %v", *got)
	}

	// The second login (the account now exists) must be a no-op, not a rewrite
	// and not an error. A new orphan created in between — an admin booking the
	// same guest in tomorrow — is what must get picked up instead.
	later := seedOrphanBooking(t, db, rid, guestPhone)
	verifyWithSeededCode(t, uc, otpRepo, guestPhone)
	if got := bookingOwner(t, db, later); got == nil || *got != userID {
		t.Errorf("later admin-made booking owner = %v, want %s", got, userID)
	}
	if got := bookingOwner(t, db, mine); got == nil || *got != userID {
		t.Errorf("first booking changed hands on the second login: owner = %v", got)
	}
}

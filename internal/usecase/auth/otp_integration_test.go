package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/otp"
	"backend-core/internal/infrastructure/postgres/refreshtoken"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/postgres/usercredential"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
)

// realStubSender is a trivial OTPSender used to wire a real *Service in the
// integration test below; it never actually sends anything.
type realStubSender struct{}

func (realStubSender) Send(_ context.Context, _, _ string) (string, error) { return "test", nil }

// newRealTestService wires a *Service against the real Postgres-backed repos
// and the real sqltx.Manager, so that transaction rollback behavior is
// exercised for real (unlike the noTx fake used by the unit tests).
func newRealTestService(t *testing.T) *Service {
	t.Helper()
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "refresh_tokens", "user_credentials", "otp_codes", "users")

	iss, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}

	return NewService(Deps{
		Users:       user.New(db),
		Credentials: usercredential.New(db),
		Refresh:     refreshtoken.New(db),
		OTP:         otp.New(db),
		Tx:          sqltx.NewManager(db),
		Tokens:      iss,
		OTPSender:   realStubSender{},
		Config: Config{
			RefreshTTL: time.Hour,
			OTPTTL:     5 * time.Minute,
			OTPPerMin:  1,
			OTPPerHour: 5,
		},
	})
}

// TestVerifyOTPWrongCodePersistsAttemptAcrossRealTx is a regression test for
// the bug where VerifyOTP wrapped its entire body (including the failed-guess
// IncrementAttempts call) in a single WithinTx. Because the closure returned
// domain.ErrUnauthorized on a wrong guess, the real transaction manager rolled
// back the whole thing, discarding the attempt increment -- so attempts never
// grew and the maxOTPAttempts lockout could never fire. The in-memory noTx
// fake used by the unit tests can't catch this since it never rolls back.
//
// Before the fix: this test fails at the "attempts == 1" assertion (attempts
// stays 0, since IncrementAttempts is rolled back with the rest of the tx).
// After the fix: attempts is durably 1 because the read + attempt accounting
// happen outside WithinTx.
func TestVerifyOTPWrongCodePersistsAttemptAcrossRealTx(t *testing.T) {
	db := testdb.Connect(t)
	svc := newRealTestService(t)
	ctx := context.Background()

	const phone = "+77010000001"
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     phone,
		CodeHash:  otpcode.Hash("111111"),
		Channel:   "test",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := svc.d.OTP.Create(ctx, rec); err != nil {
		t.Fatalf("seed OTP: %v", err)
	}

	if _, err := svc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("VerifyOTP(wrong code) = %v, want ErrUnauthorized", err)
	}

	var attempts int
	if err := db.QueryRow(`SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (failed guess must be committed outside the tx that returns ErrUnauthorized)", attempts)
	}

	// Drive the lockout: keep guessing wrong until maxOTPAttempts is reached,
	// then verify a further attempt is rejected due to the lockout itself
	// (rec.Attempts >= maxOTPAttempts), not just a hash mismatch.
	for i := attempts; i < maxOTPAttempts; i++ {
		if _, err := svc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("VerifyOTP(wrong code) attempt %d = %v, want ErrUnauthorized", i, err)
		}
	}
	if err := db.QueryRow(`SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts after lockout loop: %v", err)
	}
	if attempts != maxOTPAttempts {
		t.Fatalf("attempts = %d, want %d after exhausting guesses", attempts, maxOTPAttempts)
	}

	// One more guess: now locked out purely on attempt count, even though the
	// code hash check would otherwise still run.
	if _, err := svc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("VerifyOTP after lockout = %v, want ErrUnauthorized", err)
	}
	if err := db.QueryRow(`SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts after lockout guess: %v", err)
	}
	if attempts != maxOTPAttempts {
		t.Fatalf("attempts = %d, want unchanged %d once locked out", attempts, maxOTPAttempts)
	}
}

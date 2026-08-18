package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	"backend-core/internal/infrastructure/postgres/otp"
	"backend-core/internal/infrastructure/postgres/refreshtoken"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/infrastructure/token"
	"backend-core/internal/infrastructure/token/tokentest"
)

// realStubSender is a trivial OTPSender used to wire a real OTPUseCase in the
// integration test below; it never actually sends anything.
type realStubSender struct{}

func (realStubSender) Send(_ context.Context, _, _ string, _ domain.OTPSendHint) (string, error) {
	return "test", nil
}

// newRealTestOTP wires the OTPUseCase against the real Postgres-backed repos and
// the real sqltx.Manager, so that transaction rollback behavior is exercised for
// real (unlike the noTx fake used by the unit tests). It returns the OTP repo
// (for seeding) and the db handle (for direct assertions).
func newRealTestOTP(t *testing.T) (OTPUseCase, domain.OTPRepository, *pgxpool.Pool) {
	t.Helper()
	db := testdb.Connect(t)
	testdb.Truncate(t, db, "refresh_tokens", "user_credentials", "otp_codes", "users")

	iss, err := token.NewRSAIssuer(tokentest.GenerateKeyPEM(t), "kid", 15*time.Minute)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}

	otpRepo := otp.New(db)
	uc := NewOTPUseCase(
		user.New(db),
		otpRepo,
		refreshtoken.New(db),
		bookingrepo.New(db),
		sqltx.NewManager(db),
		iss,
		realStubSender{},
		Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5},
	)
	return uc, otpRepo, db
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
	uc, otpRepo, db := newRealTestOTP(t)
	ctx := context.Background()

	const phone = "+77010000001"
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     phone,
		CodeHash:  otpcode.Hash("111111"),
		Channel:   "test",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if err := otpRepo.Create(ctx, rec); err != nil {
		t.Fatalf("seed OTP: %v", err)
	}

	if _, err := uc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("VerifyOTP(wrong code) = %v, want ErrUnauthorized", err)
	}

	var attempts int
	if err := db.QueryRow(ctx, `SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (failed guess must be committed outside the tx that returns ErrUnauthorized)", attempts)
	}

	// Drive the lockout: keep guessing wrong until maxOTPAttempts is reached,
	// then verify a further attempt is rejected due to the lockout itself
	// (rec.Attempts >= maxOTPAttempts), not just a hash mismatch.
	for i := attempts; i < maxOTPAttempts; i++ {
		if _, err := uc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("VerifyOTP(wrong code) attempt %d = %v, want ErrUnauthorized", i, err)
		}
	}
	if err := db.QueryRow(ctx, `SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts after lockout loop: %v", err)
	}
	if attempts != maxOTPAttempts {
		t.Fatalf("attempts = %d, want %d after exhausting guesses", attempts, maxOTPAttempts)
	}

	// One more guess: now locked out purely on attempt count, even though the
	// code hash check would otherwise still run.
	if _, err := uc.VerifyOTP(ctx, phone, "000000"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("VerifyOTP after lockout = %v, want ErrUnauthorized", err)
	}
	if err := db.QueryRow(ctx, `SELECT attempts FROM otp_codes WHERE id = $1`, rec.ID).Scan(&attempts); err != nil {
		t.Fatalf("query attempts after lockout guess: %v", err)
	}
	if attempts != maxOTPAttempts {
		t.Fatalf("attempts = %d, want unchanged %d once locked out", attempts, maxOTPAttempts)
	}
}

// TestOTPVerifyCodesAgainstRealPostgres pins the machine-readable codes over
// the REAL repository. The in-memory fake decides "expired" by comparing Go
// timestamps; here it is the SQL predicate on otp_codes that decides, which is
// the thing that actually runs in production — and it is the one situation
// where a merged answer could silently become a distinguishable one.
func TestOTPVerifyCodesAgainstRealPostgres(t *testing.T) {
	uc, otpRepo, _ := newRealTestOTP(t)
	ctx := context.Background()

	seed := func(t *testing.T, phone, code string, expiresIn time.Duration) {
		t.Helper()
		if err := otpRepo.Create(ctx, &domain.OTPCode{
			ID: uuid.New(), Phone: phone, CodeHash: otpcode.Hash(code),
			Channel: "test", ExpiresAt: time.Now().Add(expiresIn),
		}); err != nil {
			t.Fatalf("seed OTP: %v", err)
		}
	}

	const (
		live    = "+77010000010" // a live code, guessed wrong
		expired = "+77010000011" // the right code, submitted too late
		unknown = "+77010000012" // never requested anything
	)
	seed(t, live, "111111", 5*time.Minute)
	seed(t, expired, "222222", -time.Second)

	answers := map[string]error{}
	for name, call := range map[string]func() error{
		"wrong code": func() error {
			_, err := uc.VerifyOTP(ctx, live, "000000")
			return err
		},
		"expired code": func() error {
			_, err := uc.VerifyOTP(ctx, expired, "222222")
			return err
		},
		"no active code": func() error {
			_, err := uc.VerifyOTP(ctx, unknown, "000000")
			return err
		},
	} {
		err := call()
		if !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("%s: err = %v, want ErrUnauthorized", name, err)
		}
		code, ok := domain.CodeOf(err)
		if !ok || code != domain.CodeOTPInvalid {
			t.Errorf("%s: code = %q (present %v), want %q — the three must stay merged, "+
				"or the endpoint tells an outsider whether a live code exists",
				name, code, ok, domain.CodeOTPInvalid)
		}
		answers[name] = err
	}
	if len(answers) != 3 {
		t.Fatalf("expected all three situations to be exercised, got %d", len(answers))
	}

	// The lockout keeps its own code: retyping cannot help any more.
	const locked = "+77010000013"
	seed(t, locked, "333333", 5*time.Minute)
	var lastErr error
	for i := 0; i < maxOTPAttempts; i++ {
		_, lastErr = uc.VerifyOTP(ctx, locked, "000000")
	}
	code, ok := domain.CodeOf(lastErr)
	if !ok || code != domain.CodeOTPTooManyAttempts {
		t.Errorf("after %d wrong guesses: code = %q (present %v), want %q",
			maxOTPAttempts, code, ok, domain.CodeOTPTooManyAttempts)
	}
	// Even the correct code is refused now, with the same code.
	_, err := uc.VerifyOTP(ctx, locked, "333333")
	code, ok = domain.CodeOf(err)
	if !ok || code != domain.CodeOTPTooManyAttempts {
		t.Errorf("correct code after lockout: code = %q (present %v), want %q", code, ok, domain.CodeOTPTooManyAttempts)
	}
}

// The per-phone rate limits must be distinguishable AND must say how long to
// wait, over the real counter (which counts otp_codes rows, not memory).
func TestOTPRequestRateLimitCodesAgainstRealPostgres(t *testing.T) {
	uc, _, _ := newRealTestOTP(t) // OTPPerMin = 1, OTPPerHour = 5
	ctx := context.Background()

	const phone = "+77010000020"
	if _, err := uc.RequestOTP(ctx, phone); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, err := uc.RequestOTP(ctx, phone)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("second request: err = %v, want ErrValidation (422 unchanged)", err)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeOTPRateLimitedMinute {
		t.Errorf("code = %q (present %v), want %q", code, ok, domain.CodeOTPRateLimitedMinute)
	}
	after, ok := domain.RetryAfterOf(err)
	if !ok || after != time.Minute {
		t.Errorf("retry-after = %v (present %v), want 1m", after, ok)
	}
}

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// The login endpoint used to answer one 401 to four different situations and
// one 422 to three, so the app could only list possibilities to the guest
// ("код неверный, устарел, или попыток слишком много"). This file pins the
// machine-readable code of every one of those situations — AND pins the three
// that must stay merged, because separating them would leak whether a live code
// exists for a phone number to anyone who asks (see domain.CodeOTPInvalid).

// newOTPWith wires the usecase over the fakes with an explicit config, so a
// test can make a code expire on arrival or make the hourly limit reachable.
func newOTPWith(t *testing.T, cfg Config) (OTPUseCase, *fakeOTP) {
	t.Helper()
	otp := newFakeOTP()
	uc := NewOTPUseCase(newFakeUsers(), otp, newFakeRefresh(), noTx{},
		testIssuer(t), &stubSender{}, cfg)
	return uc, otp
}

func defaultOTPConfig() Config {
	return Config{
		RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute,
		OTPPerMin: 1, OTPPerHour: 5, OTPDevExpose: true,
	}
}

// assertCoded checks the sentinel (which fixes the HTTP status) and the
// machine-readable code together: a right code on the wrong sentinel would
// change the status the client sees.
func assertCoded(t *testing.T, err error, wantSentinel error, wantCode domain.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error carrying %s, got nil", wantCode)
	}
	if !errors.Is(err, wantSentinel) {
		t.Errorf("sentinel = %v, want %v (the HTTP status would change)", err, wantSentinel)
	}
	code, ok := domain.CodeOf(err)
	if !ok {
		t.Fatalf("no code attached, want %s", wantCode)
	}
	if code != wantCode {
		t.Errorf("code = %q, want %q", code, wantCode)
	}
}

func TestOTPErrorCodes(t *testing.T) {
	t.Run("request: unusable phone", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		_, err := uc.RequestOTP(context.Background(), "не телефон")
		assertCoded(t, err, domain.ErrValidation, domain.CodeOTPInvalidPhone)
		if _, ok := domain.RetryAfterOf(err); ok {
			t.Error("a malformed phone is not a rate limit; it must carry no Retry-After")
		}
	})

	t.Run("request: per-minute limit", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig()) // OTPPerMin = 1
		ctx := context.Background()
		if _, err := uc.RequestOTP(ctx, "+77070000001"); err != nil {
			t.Fatalf("first request: %v", err)
		}
		_, err := uc.RequestOTP(ctx, "+77070000001")
		assertCoded(t, err, domain.ErrValidation, domain.CodeOTPRateLimitedMinute)
		after, ok := domain.RetryAfterOf(err)
		if !ok || after != time.Minute {
			t.Errorf("retry-after = %v (present %v), want 1m", after, ok)
		}
	})

	t.Run("request: hourly limit", func(t *testing.T) {
		cfg := defaultOTPConfig()
		cfg.OTPPerMin, cfg.OTPPerHour = 10, 2 // per-minute must not fire first
		uc, _ := newOTPWith(t, cfg)
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if _, err := uc.RequestOTP(ctx, "+77070000002"); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		_, err := uc.RequestOTP(ctx, "+77070000002")
		assertCoded(t, err, domain.ErrValidation, domain.CodeOTPRateLimitedHour)
		after, ok := domain.RetryAfterOf(err)
		if !ok || after != time.Hour {
			t.Errorf("retry-after = %v (present %v), want 1h", after, ok)
		}
	})

	t.Run("verify: unusable phone", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		_, err := uc.VerifyOTP(context.Background(), "", "123456")
		assertCoded(t, err, domain.ErrValidation, domain.CodeOTPInvalidPhone)
	})

	t.Run("verify: no code submitted", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		_, err := uc.VerifyOTP(context.Background(), "+77070000003", "")
		assertCoded(t, err, domain.ErrValidation, domain.CodeOTPCodeRequired)
	})

	t.Run("verify: wrong code", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		ctx := context.Background()
		if _, err := uc.RequestOTP(ctx, "+77070000004"); err != nil {
			t.Fatalf("request: %v", err)
		}
		_, err := uc.VerifyOTP(ctx, "+77070000004", "000000")
		assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPInvalid)
	})

	t.Run("verify: expired code", func(t *testing.T) {
		cfg := defaultOTPConfig()
		cfg.OTPTTL = -time.Second // born expired
		uc, _ := newOTPWith(t, cfg)
		ctx := context.Background()
		code, err := uc.RequestOTP(ctx, "+77070000005")
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		// The RIGHT code, submitted too late.
		_, err = uc.VerifyOTP(ctx, "+77070000005", code)
		assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPInvalid)
	})

	t.Run("verify: no active code at all", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		_, err := uc.VerifyOTP(context.Background(), "+77070000006", "123456")
		assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPInvalid)
	})

	t.Run("verify: locked after too many wrong guesses", func(t *testing.T) {
		uc, _ := newOTPWith(t, defaultOTPConfig())
		ctx := context.Background()
		code, err := uc.RequestOTP(ctx, "+77070000007")
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		// The first four guesses are ordinary refusals; the fifth exhausts the
		// budget and must say so rather than invite a sixth.
		for i := 0; i < maxOTPAttempts-1; i++ {
			_, err := uc.VerifyOTP(ctx, "+77070000007", "000000")
			assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPInvalid)
		}
		_, err = uc.VerifyOTP(ctx, "+77070000007", "000000")
		assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPTooManyAttempts)

		// And the correct code no longer helps — the lockout is real, not a
		// message. This is the guarantee that makes the separate code honest.
		_, err = uc.VerifyOTP(ctx, "+77070000007", code)
		assertCoded(t, err, domain.ErrUnauthorized, domain.CodeOTPTooManyAttempts)
	})
}

// The anti-oracle guarantee. An outsider who knows a phone number must not be
// able to learn from /auth/otp/verify whether that number has a live code —
// that bit is the timing signal an OTP phishing call needs, and it is free to
// poll for. So the three "not accepted" situations must be byte-identical.
func TestOTPVerifyDoesNotRevealWhetherACodeExists(t *testing.T) {
	ctx := context.Background()

	// (a) a number with a LIVE code, guessed wrong
	live, _ := newOTPWith(t, defaultOTPConfig())
	if _, err := live.RequestOTP(ctx, "+77070001111"); err != nil {
		t.Fatalf("request: %v", err)
	}
	_, wrongErr := live.VerifyOTP(ctx, "+77070001111", "000000")

	// (b) a number that never requested anything
	none, _ := newOTPWith(t, defaultOTPConfig())
	_, noneErr := none.VerifyOTP(ctx, "+77070002222", "000000")

	// (c) a number whose code has expired
	expiredCfg := defaultOTPConfig()
	expiredCfg.OTPTTL = -time.Second
	expired, _ := newOTPWith(t, expiredCfg)
	if _, err := expired.RequestOTP(ctx, "+77070003333"); err != nil {
		t.Fatalf("request: %v", err)
	}
	_, expiredErr := expired.VerifyOTP(ctx, "+77070003333", "000000")

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"wrong code (a live code exists)", wrongErr},
		{"no code was ever requested", noneErr},
		{"the code expired", expiredErr},
	} {
		assertCoded(t, tc.err, domain.ErrUnauthorized, domain.CodeOTPInvalid)
		if tc.err.Error() != wrongErr.Error() {
			t.Errorf("%s: message %q differs from %q — the two answers must be indistinguishable",
				tc.name, tc.err.Error(), wrongErr.Error())
		}
	}
}

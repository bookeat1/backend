package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// newTestOTPWith wires the OTPUseCase over in-memory fakes, exposing the sender
// and the OTP repository so tests can drive delivery and inspect the memory the
// repository holds (there is no separate preferences store: a USED otp_codes row
// IS the memory).
func newTestOTPWith(t *testing.T, sender *stubSender) (OTPUseCase, *fakeOTP) {
	t.Helper()
	otpRepo := newFakeOTP()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 3, OTPPerHour: 10, OTPDevExpose: true}
	uc := NewOTPUseCase(newFakeUsers(), otpRepo, newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), sender, cfg)
	return uc, otpRepo
}

// A verified login teaches the next one: the channel that carried the code the
// guest typed back must come first the next time. That is the entire payoff of
// the memory, and it costs no new table.
func TestRequestOTPPrefersTheChannelOfTheLastVerifiedCode(t *testing.T) {
	sender := &stubSender{channel: domain.OTPChannelSMS}
	uc, _ := newTestOTPWith(t, sender)
	ctx := context.Background()

	code, err := uc.RequestOTP(ctx, "8 707 000 0000")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if sender.lastHint.Prefer != "" {
		t.Fatalf("a first-time number must carry no preference, got %q", sender.lastHint.Prefer)
	}
	if _, err := uc.VerifyOTP(ctx, "+77070000000", code); err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}

	if _, err := uc.RequestOTP(ctx, "+77070000000"); err != nil {
		t.Fatalf("second RequestOTP: %v", err)
	}
	if sender.lastHint.Prefer != domain.OTPChannelSMS {
		t.Fatalf("hint.Prefer = %q, want %q", sender.lastHint.Prefer, domain.OTPChannelSMS)
	}
}

// A code that a provider accepted but nobody verified is the WhatsApp silent-drop
// signature. The retry must be told to try that channel LAST, otherwise the guest
// hits the same silence forever.
func TestRequestOTPDeprioritizesAChannelHoldingAnUnverifiedCode(t *testing.T) {
	sender := &stubSender{channel: domain.OTPChannelWhatsApp}
	uc, _ := newTestOTPWith(t, sender)
	ctx := context.Background()

	if _, err := uc.RequestOTP(ctx, "+77070000001"); err != nil {
		t.Fatalf("first RequestOTP: %v", err)
	}
	if len(sender.lastHint.Deprioritize) != 0 {
		t.Fatalf("first attempt must carry no deprioritized channel, got %v", sender.lastHint.Deprioritize)
	}

	// Second attempt while the first code is still active and unused.
	if _, err := uc.RequestOTP(ctx, "+77070000001"); err != nil {
		t.Fatalf("second RequestOTP: %v", err)
	}
	if len(sender.lastHint.Deprioritize) != 1 || sender.lastHint.Deprioritize[0] != domain.OTPChannelWhatsApp {
		t.Fatalf("hint.Deprioritize = %v, want [%s]", sender.lastHint.Deprioritize, domain.OTPChannelWhatsApp)
	}
}

// The memory is written by verification and by nothing else: a code a provider
// merely ACCEPTED must not become a preference, because acceptance is not
// delivery (Meta answers 200 for a number that has no WhatsApp).
func TestChannelIsRememberedOnlyAfterVerification(t *testing.T) {
	sender := &stubSender{channel: domain.OTPChannelWhatsApp}
	uc, repo := newTestOTPWith(t, sender)
	ctx := context.Background()

	code, err := uc.RequestOTP(ctx, "+77070000002")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if last, _ := repo.LastUsedChannelByPhone(ctx, "+77070000002"); last != "" {
		t.Fatalf("remembered %q on send — acceptance is not delivery", last)
	}

	if _, err := uc.VerifyOTP(ctx, "+77070000002", "000000"); err == nil {
		t.Fatal("expected a wrong code to fail")
	}
	if last, _ := repo.LastUsedChannelByPhone(ctx, "+77070000002"); last != "" {
		t.Fatalf("a failed verification must remember nothing, got %q", last)
	}

	if _, err := uc.VerifyOTP(ctx, "+77070000002", code); err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	if last, _ := repo.LastUsedChannelByPhone(ctx, "+77070000002"); last != domain.OTPChannelWhatsApp {
		t.Fatalf("remembered = %q, want %q", last, domain.OTPChannelWhatsApp)
	}
}

// "stub" and "undelivered" are not routes a guest can be reached on; preferring
// either would put a dead channel at the head of the next waterfall.
func TestNonDeliveryChannelsAreNeverPreferred(t *testing.T) {
	for _, channel := range []string{domain.OTPChannelStub, domain.OTPChannelUndelivered, "test", ""} {
		t.Run("channel="+channel, func(t *testing.T) {
			sender := &stubSender{channel: channel}
			uc, _ := newTestOTPWith(t, sender)
			ctx := context.Background()

			code, err := uc.RequestOTP(ctx, "+77070000003")
			if err != nil {
				t.Fatalf("RequestOTP: %v", err)
			}
			if _, err := uc.VerifyOTP(ctx, "+77070000003", code); err != nil {
				t.Fatalf("VerifyOTP: %v", err)
			}
			if _, err := uc.RequestOTP(ctx, "+77070000003"); err != nil {
				t.Fatalf("second RequestOTP: %v", err)
			}
			if sender.lastHint.Prefer != "" {
				t.Fatalf("channel %q must not be preferred, got hint %q", channel, sender.lastHint.Prefer)
			}
		})
	}
}

// The memory is an optimization. A failing read must slow a login down, never
// break it.
func TestChannelMemoryFailureNeverBreaksLogin(t *testing.T) {
	sender := &stubSender{channel: domain.OTPChannelSMS}
	uc, repo := newTestOTPWith(t, sender)
	repo.lastUsedErr = errors.New("otp_codes is on fire")
	ctx := context.Background()

	code, err := uc.RequestOTP(ctx, "+77070000004")
	if err != nil {
		t.Fatalf("RequestOTP must survive a broken memory read: %v", err)
	}
	if sender.lastHint.Prefer != "" {
		t.Fatalf("a failed read must produce no preference, got %q", sender.lastHint.Prefer)
	}
	if _, err := uc.VerifyOTP(ctx, "+77070000004", code); err != nil {
		t.Fatalf("VerifyOTP must not depend on the memory: %v", err)
	}
}

// When no channel takes the code the caller gets an error — and the attempt must
// STILL be counted, or the rate limits (which count otp_codes rows) would apply
// only to successful sends and an attacker could burn our WhatsApp/SMS budget
// without limit.
func TestRequestOTPFailedDeliveryStillConsumesTheRateLimit(t *testing.T) {
	sender := &stubSender{err: errors.New("delivery failed on every configured channel")}
	otpRepo := newFakeOTP()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5}
	uc := NewOTPUseCase(newFakeUsers(), otpRepo, newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), sender, cfg)
	ctx := context.Background()

	if _, err := uc.RequestOTP(ctx, "+77070000006"); err == nil {
		t.Fatal("expected the delivery failure to surface")
	}
	// The second call inside the same minute must be rejected by the rate limit
	// (OTPPerMin=1) and must NOT reach the sender again.
	if _, err := uc.RequestOTP(ctx, "+77070000006"); err == nil {
		t.Fatal("expected the rate limit to reject the second attempt")
	}
	if sender.calls != 1 {
		t.Fatalf("sender was called %d times, want 1 — a failed delivery must still consume the budget", sender.calls)
	}

	// The recorded attempt can never be used to log in.
	n, err := otpRepo.CountSince(ctx, "+77070000006", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("CountSince: %v", err)
	}
	if n != 1 {
		t.Fatalf("recorded attempts = %d, want 1", n)
	}
	if _, err := otpRepo.LatestActiveByPhone(ctx, "+77070000006"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an undelivered code must not be active, got %v", err)
	}
	// An undelivered attempt must not teach the router anything either.
	if last, _ := otpRepo.LastUsedChannelByPhone(ctx, "+77070000006"); last != "" {
		t.Fatalf("an undelivered attempt was remembered as %q", last)
	}
}

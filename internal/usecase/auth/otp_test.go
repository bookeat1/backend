package auth

import (
	"context"
	"testing"
	"time"
)

// newTestOTP wires the OTPUseCase over in-memory fakes with OTPDevExpose on, so
// tests can read back the generated code. It returns the shared fakeUsers so
// tests can assert on find-or-create behavior.
func newTestOTP(t *testing.T) (OTPUseCase, *fakeUsers, *stubSender) {
	t.Helper()
	users := newFakeUsers()
	sender := &stubSender{}
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5, OTPDevExpose: true}
	uc := NewOTPUseCase(users, newFakeOTP(), newFakeRefresh(), noTx{}, testIssuer(t), sender, cfg)
	return uc, users, sender
}

func TestRequestOTPRateLimit(t *testing.T) {
	uc, _, sender := newTestOTP(t)
	ctx := context.Background()

	if _, err := uc.RequestOTP(ctx, "8 707 000 0000"); err != nil {
		t.Fatalf("first RequestOTP: %v", err)
	}
	if sender.lastCode == "" {
		t.Fatal("sender should have received a code")
	}
	// Second within the same minute exceeds OTPPerMin=1.
	if _, err := uc.RequestOTP(ctx, "8 707 000 0000"); err == nil {
		t.Error("expected rate-limit error on immediate second request")
	}
}

func TestVerifyOTPCreatesUserAndIssuesPair(t *testing.T) {
	uc, users, _ := newTestOTP(t)
	ctx := context.Background()

	code, err := uc.RequestOTP(ctx, "+7 701 111 2222")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if code == "" {
		t.Fatal("dev expose should return the code")
	}
	pair, err := uc.VerifyOTP(ctx, "8 701 111 2222", code) // different formatting, same number
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("expected token pair")
	}
	// A brand-new user now exists for that normalized phone.
	if _, err := users.GetByPhone(ctx, "+77011112222"); err != nil {
		t.Errorf("user should exist for phone: %v", err)
	}
}

func TestVerifyOTPWrongCode(t *testing.T) {
	uc, _, _ := newTestOTP(t)
	ctx := context.Background()
	if _, err := uc.RequestOTP(ctx, "+7 705 000 0000"); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := uc.VerifyOTP(ctx, "+7 705 000 0000", "000000"); err == nil {
		t.Error("expected error on wrong code")
	}
}

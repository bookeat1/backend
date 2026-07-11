package auth

import (
	"context"
	"testing"
	"time"
)

func TestRequestOTPRateLimit(t *testing.T) {
	svc, sender := newTestService(t)
	ctx := context.Background()

	if _, err := svc.RequestOTP(ctx, "8 707 000 0000"); err != nil {
		t.Fatalf("first RequestOTP: %v", err)
	}
	if sender.lastCode == "" {
		t.Fatal("sender should have received a code")
	}
	// Second within the same minute exceeds OTPPerMin=1.
	if _, err := svc.RequestOTP(ctx, "8 707 000 0000"); err == nil {
		t.Error("expected rate-limit error on immediate second request")
	}
}

func TestVerifyOTPCreatesUserAndIssuesPair(t *testing.T) {
	svc, _ := newTestService(t)
	svc.d.Config.OTPDevExpose = true
	ctx := context.Background()

	code, err := svc.RequestOTP(ctx, "+7 701 111 2222")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if code == "" {
		t.Fatal("dev expose should return the code")
	}
	pair, err := svc.VerifyOTP(ctx, "8 701 111 2222", code) // different formatting, same number
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("expected token pair")
	}
	// A brand-new user now exists for that normalized phone.
	if _, err := svc.d.Users.GetByPhone(ctx, "+77011112222"); err != nil {
		t.Errorf("user should exist for phone: %v", err)
	}
}

func TestVerifyOTPWrongCode(t *testing.T) {
	svc, _ := newTestService(t)
	svc.d.Config.OTPDevExpose = true
	ctx := context.Background()
	if _, err := svc.RequestOTP(ctx, "+7 705 000 0000"); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := svc.VerifyOTP(ctx, "+7 705 000 0000", "000000"); err == nil {
		t.Error("expected error on wrong code")
	}
	_ = time.Now
}

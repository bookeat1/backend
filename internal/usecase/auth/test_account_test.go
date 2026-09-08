package auth

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// The number and the code the owner approved for App Review. Written here in
// the human format the env var will carry, on purpose: the test then also
// proves the normalization step (spaces and all) that the reviewer's typing
// relies on.
const (
	reviewPhoneRaw  = "+7 777 000 00 00"
	reviewPhoneE164 = "+77770000000"
	reviewCode      = "123456"
)

// newReviewOTP wires the usecase with the test account ENABLED, and returns a
// buffer holding every log line it writes.
func newReviewOTP(t *testing.T) (OTPUseCase, *fakeUsers, *stubSender, *fakeOTP, *bytes.Buffer) {
	t.Helper()
	users := newFakeUsers()
	sender := &stubSender{}
	otpRepo := newFakeOTP()
	cfg := Config{
		RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute,
		OTPPerMin: 1, OTPPerHour: 5,
		TestAccountPhone: reviewPhoneRaw,
		TestAccountCode:  reviewCode,
	}
	uc := NewOTPUseCase(users, otpRepo, newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), sender, cfg)
	var buf bytes.Buffer
	return uc, users, sender, otpRepo, &buf
}

// logCtx returns a context whose logger writes into buf, so a test can assert
// on the WARN lines the test account is required to leave.
func logCtx(buf *bytes.Buffer) context.Context {
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return logging.WithLogger(context.Background(), slog.New(h))
}

// The whole point of the feature: the reviewer asks for a code, nothing is
// sent anywhere, and the fixed code logs them in.
func TestTestAccountLogsInWithFixedCode(t *testing.T) {
	uc, users, sender, otpRepo, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	code, err := uc.RequestOTP(ctx, reviewPhoneRaw)
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if code != "" {
		t.Errorf("without OTPDevExpose the response must carry no code, got %q", code)
	}
	// The number does not exist on any network: a delivery attempt would fail
	// and take the login down with it.
	if sender.lastCode != "" {
		t.Errorf("sender must not be called for the test account, got code %q", sender.lastCode)
	}
	// And nothing is written to otp_codes — there is no code to store.
	if len(otpRepo.codes) != 0 {
		t.Errorf("no otp_codes row must be written, got %d", len(otpRepo.codes))
	}

	pair, err := uc.VerifyOTP(ctx, "8 777 000 00 00", reviewCode) // any formatting of the same number
	if err != nil {
		t.Fatalf("VerifyOTP with the fixed code: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("expected a full token pair")
	}
	// It is an ordinary guest account afterwards, created on first use.
	u, err := users.GetByPhone(ctx, reviewPhoneE164)
	if err != nil {
		t.Fatalf("account should exist for the normalized number: %v", err)
	}
	if u.Role != domain.RoleUser {
		t.Errorf("test account must be a plain guest, got role %q", u.Role)
	}
	if u.PhoneVerifiedAt == nil {
		t.Error("phone_verified_at must be set, like any other OTP login")
	}
}

// Only the configured code opens the account. Every other code — including a
// well-formed six-digit one — gets the same answer any wrong code gets.
func TestTestAccountRejectsEveryOtherCode(t *testing.T) {
	uc, users, _, _, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	for _, wrong := range []string{"000000", "12345", "1234567", "123457", "", "  123456  "} {
		if _, err := uc.VerifyOTP(ctx, reviewPhoneRaw, wrong); err == nil {
			t.Fatalf("code %q must NOT be accepted", wrong)
		}
	}
	if _, err := users.GetByPhone(ctx, reviewPhoneE164); err == nil {
		t.Error("a rejected attempt must not create the account")
	}
}

// Every touch of this number is visible in the log: that is the only way to
// notice somebody hammering it, since it writes no otp_codes rows at all.
func TestTestAccountAttemptsAreLogged(t *testing.T) {
	uc, _, _, _, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	if _, err := uc.RequestOTP(ctx, reviewPhoneRaw); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := uc.VerifyOTP(ctx, reviewPhoneRaw, "999999"); err == nil {
		t.Fatal("wrong code must fail")
	}
	if _, err := uc.VerifyOTP(ctx, reviewPhoneRaw, reviewCode); err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		logging.EventTestAccountOTPRequested,
		logging.EventTestAccountLoginAttempt,
		"accepted=false",
		"accepted=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log must contain %q; got:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "level=WARN"); n != 3 {
		t.Errorf("all three lines must be WARN, got %d:\n%s", n, out)
	}
	// The full number never reaches the log.
	if strings.Contains(out, reviewPhoneE164) {
		t.Errorf("the phone must be masked in the log:\n%s", out)
	}
}

// The reviewer taps "resend" as often as they like: the per-phone budget
// (1/min) does not apply to a number nothing is ever sent to. Cost of a
// request here is zero, and a lockout would read as a broken app.
func TestTestAccountIsNotPerPhoneRateLimited(t *testing.T) {
	uc, _, _, _, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	for i := range 5 {
		if _, err := uc.RequestOTP(ctx, reviewPhoneRaw); err != nil {
			t.Fatalf("RequestOTP #%d: %v", i+1, err)
		}
	}
}

// The ordinary numbers must not notice this feature exists: same rate limit,
// same delivery, same attempt counter.
func TestOtherPhonesUnaffectedWhileTestAccountEnabled(t *testing.T) {
	uc, _, sender, _, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	code, err := uc.RequestOTP(ctx, "+7 701 222 3344")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if sender.lastCode == "" {
		t.Fatal("a real number must still get its code sent")
	}
	if code != "" {
		t.Error("dev expose is off, the response must carry no code")
	}
	// Per-phone budget still applies to everybody else (OTPPerMin=1).
	if _, err := uc.RequestOTP(ctx, "+7 701 222 3344"); err == nil {
		t.Error("the ordinary per-phone rate limit must still fire")
	}
	// The review code is not a master key.
	if _, err := uc.VerifyOTP(ctx, "+7 701 222 3344", reviewCode); err == nil && sender.lastCode != reviewCode {
		t.Error("the review code must not open an ordinary number")
	}
	// The number's real code still works.
	if _, err := uc.VerifyOTP(ctx, "+7 701 222 3344", sender.lastCode); err != nil {
		t.Errorf("the real code must still log the guest in: %v", err)
	}
}

// With the env vars empty the feature does not exist: the approved number is
// just a number, its code is really sent, and a made-up "123456" is refused.
func TestTestAccountDisabledByDefault(t *testing.T) {
	uc, _, sender := newTestOTP(t) // no TestAccount* in its Config
	var buf bytes.Buffer
	ctx := logCtx(&buf)

	sent, err := uc.RequestOTP(ctx, reviewPhoneRaw)
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if sender.lastCode == "" {
		t.Fatal("with the feature off the code must really be sent")
	}
	if sent != sender.lastCode {
		t.Fatalf("dev expose should echo the generated code, got %q want %q", sent, sender.lastCode)
	}
	if sender.lastCode != reviewCode { // a random 6-digit code, astronomically unlikely to collide
		if _, err := uc.VerifyOTP(ctx, reviewPhoneRaw, reviewCode); err == nil {
			t.Error("with the feature off the fixed code must NOT be accepted")
		}
	}
	if strings.Contains(buf.String(), "test_account") {
		t.Errorf("the disabled feature must log nothing:\n%s", buf.String())
	}
}

// Half a configuration is no configuration: a phone without a code (or the
// other way round) must never half-enable anything. bootstrap.NewConfig
// refuses that pair outright, and this is the second line of defence.
func TestTestAccountHalfConfiguredStaysOff(t *testing.T) {
	for _, tc := range []struct{ name, phone, code string }{
		{"phone without code", reviewPhoneRaw, ""},
		{"code without phone", "", reviewCode},
		{"phone with no digits", "not-a-number", reviewCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := newTestAccount(Config{TestAccountPhone: tc.phone, TestAccountCode: tc.code})
			if acc.enabled() {
				t.Fatal("must stay disabled")
			}
			if acc.matches(reviewPhoneE164) {
				t.Error("a disabled account must match no phone")
			}
			if acc.codeAccepted(reviewCode) {
				t.Error("a disabled account must accept no code")
			}
		})
	}
}

// The review number is reserved: a signed-in account may not move onto it in
// either step of the phone-change flow. Without this, anybody with a session
// could take over the reviewer's account.
func TestTestAccountCannotBeTakenOverByPhoneChange(t *testing.T) {
	uc, users, sender, _, buf := newReviewOTP(t)
	ctx := logCtx(buf)

	other := "+77015556677"
	u := &domain.User{ID: uuid.New(), Phone: &other, Role: domain.RoleUser, IsActive: true}
	if err := users.Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := uc.RequestPhoneChangeOTP(ctx, u.ID, reviewPhoneRaw); err == nil {
		t.Error("requesting a phone change onto the review number must be refused")
	}
	if sender.lastCode != "" {
		t.Error("nothing must be sent for a refused phone change")
	}
	if _, err := uc.VerifyPhoneChange(ctx, u.ID, reviewPhoneRaw, reviewCode); err == nil {
		t.Fatal("verifying a phone change onto the review number must be refused")
	}
	// The seeded user still owns its own number.
	got, err := users.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Phone == nil || *got.Phone != other {
		t.Errorf("the user must not have been moved, phone = %v", got.Phone)
	}
	if !strings.Contains(buf.String(), logging.EventTestAccountPhoneChangeRefused) {
		t.Errorf("both refusals must be logged:\n%s", buf.String())
	}
}

// With OTPDevExpose on (dev/test contours only) the fixed code is echoed, the
// same way a generated one is — so a dev client's autofill keeps working.
func TestTestAccountDevExposeEchoesFixedCode(t *testing.T) {
	users := newFakeUsers()
	cfg := Config{
		RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 1, OTPPerHour: 5,
		OTPDevExpose:     true,
		TestAccountPhone: reviewPhoneRaw,
		TestAccountCode:  reviewCode,
	}
	uc := NewOTPUseCase(users, newFakeOTP(), newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), &stubSender{}, cfg)

	got, err := uc.RequestOTP(context.Background(), reviewPhoneRaw)
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if got != reviewCode {
		t.Errorf("got %q, want the configured code %q", got, reviewCode)
	}
}

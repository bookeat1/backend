package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

// newTestOTPWithUsers wires the OTPUseCase over the shared in-memory fakes and
// returns the fakes tests need to seed users and assert on OTP state.
func newTestOTPWithUsers(t *testing.T) (OTPUseCase, *fakeUsers, *fakeOTP) {
	t.Helper()
	users := newFakeUsers()
	otp := newFakeOTP()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 5, OTPPerHour: 20, OTPDevExpose: true}
	uc := NewOTPUseCase(users, otp, newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), &stubSender{}, cfg)
	return uc, users, otp
}

// seedUser inserts an active user with the given (already-normalized) phone.
func seedUser(t *testing.T, users *fakeUsers, rawPhone string) *domain.User {
	t.Helper()
	p := phone.Normalize(rawPhone)
	u := &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru"}
	if err := users.Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

func TestVerifyPhoneChangeHappyPath(t *testing.T) {
	uc, users, otp := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+7 701 111 0000")
	newPhone := "+7 707 222 3333"

	code, err := uc.RequestPhoneChangeOTP(ctx, u.ID, newPhone)
	if err != nil {
		t.Fatalf("RequestPhoneChangeOTP: %v", err)
	}
	if code == "" {
		t.Fatal("dev expose should return the code")
	}

	updated, err := uc.VerifyPhoneChange(ctx, u.ID, "8 707 222 3333", code) // different formatting, same number
	if err != nil {
		t.Fatalf("VerifyPhoneChange: %v", err)
	}
	if updated.Phone == nil || *updated.Phone != phone.Normalize(newPhone) {
		t.Fatalf("phone = %v, want %q", updated.Phone, phone.Normalize(newPhone))
	}
	if updated.PhoneVerifiedAt == nil {
		t.Error("phone_verified_at must be set")
	}
	// Persisted, and reachable under the new number.
	if _, err := users.GetByPhone(ctx, phone.Normalize(newPhone)); err != nil {
		t.Errorf("user should be reachable by new phone: %v", err)
	}
	// The used code can no longer verify anything.
	if _, err := otp.LatestActiveByPhone(ctx, phone.Normalize(newPhone)); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("no active code should remain for the new number, got %v", err)
	}
}

func TestVerifyPhoneChangeWrongCodeRejected(t *testing.T) {
	uc, users, otp := newTestOTPWithUsers(t)
	ctx := context.Background()
	oldPhone := "+77011110000"
	u := seedUser(t, users, oldPhone)
	newPhone := "+77072223333"

	if _, err := uc.RequestPhoneChangeOTP(ctx, u.ID, newPhone); err != nil {
		t.Fatalf("RequestPhoneChangeOTP: %v", err)
	}

	_, err := uc.VerifyPhoneChange(ctx, u.ID, newPhone, "000000")
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong code err = %v, want ErrUnauthorized", err)
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeOTPInvalid {
		t.Errorf("code = %q, want otp_invalid", code)
	}
	// No phone change happened.
	after, _ := users.GetByID(ctx, u.ID)
	if after.Phone == nil || *after.Phone != oldPhone {
		t.Errorf("phone changed on wrong code: %v", after.Phone)
	}
	if after.PhoneVerifiedAt != nil {
		t.Error("phone_verified_at must not be set on a rejected code")
	}
	// The code is still active (a wrong guess must not consume it).
	if _, err := otp.LatestActiveByPhone(ctx, newPhone); err != nil {
		t.Errorf("code should still be active after one wrong guess: %v", err)
	}
}

func TestVerifyPhoneChangeExpiredCodeRejected(t *testing.T) {
	uc, users, otp := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")
	newPhone := "+77072223333"

	code, err := uc.RequestPhoneChangeOTP(ctx, u.ID, newPhone)
	if err != nil {
		t.Fatalf("RequestPhoneChangeOTP: %v", err)
	}
	// Expire every stored code for the new number.
	for _, c := range otp.codes {
		if c.Phone == newPhone {
			c.ExpiresAt = time.Now().Add(-time.Minute)
		}
	}

	_, err = uc.VerifyPhoneChange(ctx, u.ID, newPhone, code)
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired code err = %v, want ErrUnauthorized", err)
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeOTPInvalid {
		t.Errorf("code = %q, want otp_invalid", code)
	}
	after, _ := users.GetByID(ctx, u.ID)
	if after.PhoneVerifiedAt != nil {
		t.Error("phone must stay unverified on an expired code")
	}
}

func TestVerifyPhoneChangeUniquenessConflict(t *testing.T) {
	uc, users, otp := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")
	taken := "+77072223333"
	other := seedUser(t, users, taken) // another live account already owns it

	// Force an active code for the taken number so we reach the write step,
	// even though the request step would normally 409 first.
	code, err := uc.RequestOTP(ctx, taken)
	if err != nil {
		t.Fatalf("seed code: %v", err)
	}

	_, err = uc.VerifyPhoneChange(ctx, u.ID, taken, code)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("conflict err = %v, want ErrAlreadyExists", err)
	}
	if c, _ := domain.CodeOf(err); c != domain.CodePhoneInUse {
		t.Errorf("code = %q, want phone_in_use", c)
	}
	// The caller keeps their old number; the other account is untouched.
	caller, _ := users.GetByID(ctx, u.ID)
	if caller.Phone == nil || *caller.Phone != "+77011110000" {
		t.Errorf("caller phone changed: %v", caller.Phone)
	}
	stillOther, _ := users.GetByPhone(ctx, taken)
	if stillOther.ID != other.ID {
		t.Errorf("taken number no longer belongs to the other user")
	}
	_ = otp
}

// updateConflictUsers wraps fakeUsers to simulate the users.phone UNIQUE
// constraint firing at write time: GetByPhone still reports the number free
// (the advisory pre-check passes), but Update rejects with ErrAlreadyExists —
// the exact race the DB-level wrap must tag with CodePhoneInUse.
type updateConflictUsers struct{ *fakeUsers }

func (u updateConflictUsers) Update(context.Context, *domain.User) error {
	return domain.ErrAlreadyExists
}

func TestVerifyPhoneChangeDBRaceMappedToPhoneInUse(t *testing.T) {
	users := newFakeUsers()
	otp := newFakeOTP()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 5, OTPPerHour: 20, OTPDevExpose: true}
	uc := NewOTPUseCase(updateConflictUsers{users}, otp, newFakeRefresh(), &fakeBookingLinker{}, noTx{}, testIssuer(t), &stubSender{}, cfg)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")
	newPhone := "+77072223333"

	code, err := uc.RequestPhoneChangeOTP(ctx, u.ID, newPhone)
	if err != nil {
		t.Fatalf("RequestPhoneChangeOTP: %v", err)
	}
	_, err = uc.VerifyPhoneChange(ctx, u.ID, newPhone, code)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("DB-race err = %v, want ErrAlreadyExists", err)
	}
	if c, _ := domain.CodeOf(err); c != domain.CodePhoneInUse {
		t.Errorf("code = %q, want phone_in_use (DB-race path must match the pre-check)", c)
	}
}

func TestRequestPhoneChangeSameNumberRejected(t *testing.T) {
	uc, users, _ := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")

	_, err := uc.RequestPhoneChangeOTP(ctx, u.ID, "8 701 111 0000") // same number, different formatting
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("same-number err = %v, want ErrValidation", err)
	}
	if c, _ := domain.CodeOf(err); c != domain.CodePhoneUnchanged {
		t.Errorf("code = %q, want phone_unchanged", c)
	}
}

func TestVerifyPhoneChangeSameNumberRejected(t *testing.T) {
	uc, users, _ := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")

	_, err := uc.VerifyPhoneChange(ctx, u.ID, "+77011110000", "123456")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("same-number err = %v, want ErrValidation", err)
	}
	if c, _ := domain.CodeOf(err); c != domain.CodePhoneUnchanged {
		t.Errorf("code = %q, want phone_unchanged", c)
	}
}

func TestRequestPhoneChangeInUseRejected(t *testing.T) {
	uc, users, _ := newTestOTPWithUsers(t)
	ctx := context.Background()
	u := seedUser(t, users, "+77011110000")
	seedUser(t, users, "+77072223333") // taken by another live account

	_, err := uc.RequestPhoneChangeOTP(ctx, u.ID, "+77072223333")
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("in-use err = %v, want ErrAlreadyExists", err)
	}
	if c, _ := domain.CodeOf(err); c != domain.CodePhoneInUse {
		t.Errorf("code = %q, want phone_in_use", c)
	}
}

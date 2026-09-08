package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
)

// newTestOTPWithLinker wires the usecase over the in-memory fakes with a
// caller-supplied booking linker, so a test can seed the bookings that were made
// for a number before it had an account.
func newTestOTPWithLinker(t *testing.T, linker *fakeBookingLinker) (OTPUseCase, *fakeUsers) {
	t.Helper()
	users := newFakeUsers()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 5, OTPPerHour: 20, OTPDevExpose: true}
	uc := NewOTPUseCase(users, newFakeOTP(), newFakeRefresh(), linker, noTx{}, testIssuer(t), &stubSender{}, cfg)
	return uc, users
}

// login runs the full request+verify pair the app runs, and fails the test if
// either half does.
func login(t *testing.T, uc OTPUseCase, rawPhone string) *TokenPair {
	t.Helper()
	ctx := context.Background()
	code, err := uc.RequestOTP(ctx, rawPhone)
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	pair, err := uc.VerifyOTP(ctx, rawPhone, code)
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	return pair
}

// TestVerifyOTPAttachesBookingsOfThatPhone is the rule itself: a guest whose
// reservation was taken over the phone (no account yet) finds it waiting the
// first time they sign in — and takes nothing else with it.
func TestVerifyOTPAttachesBookingsOfThatPhone(t *testing.T) {
	stranger := uuid.New()
	linker := &fakeBookingLinker{rows: []*fakeBooking{
		{phone: "+77015550001"},                   // theirs: made by phone, nobody owns it
		{phone: "+77015550001", owner: &stranger}, // same number, ALREADY owned
		{phone: "+77015550002"},                   // somebody else's number
	}}
	uc, users := newTestOTPWithLinker(t, linker)

	login(t, uc, "8 701 555 0001")

	u, err := users.GetByPhone(context.Background(), "+77015550001")
	if err != nil {
		t.Fatalf("user was not created: %v", err)
	}
	if got := linker.rows[0].owner; got == nil || *got != u.ID {
		t.Errorf("matching booking owner = %v, want the new user %s", got, u.ID)
	}
	if got := linker.rows[1].owner; got == nil || *got != stranger {
		t.Errorf("already-owned booking was stolen: owner = %v, want %s", got, stranger)
	}
	if linker.rows[2].owner != nil {
		t.Errorf("booking of another phone was attached: owner = %v", *linker.rows[2].owner)
	}
}

// TestVerifyOTPAttachesForAnExistingAccount covers the other half of the rule:
// the account already exists (nothing is created), a booking is made for that
// number by a venue admin, and the guest's NEXT login picks it up. This is what
// makes the rule need no backfill job.
func TestVerifyOTPAttachesForAnExistingAccount(t *testing.T) {
	linker := &fakeBookingLinker{}
	uc, users := newTestOTPWithLinker(t, linker)

	login(t, uc, "+7 701 555 0010")
	u, err := users.GetByPhone(context.Background(), "+77015550010")
	if err != nil {
		t.Fatalf("user was not created: %v", err)
	}

	// A hostess books this guest in, by phone, after the account exists.
	linker.rows = append(linker.rows, &fakeBooking{phone: "+77015550010"})

	login(t, uc, "+7 701 555 0010")
	if got := linker.rows[0].owner; got == nil || *got != u.ID {
		t.Errorf("admin-made booking owner = %v, want the existing user %s", got, u.ID)
	}
}

// TestVerifyOTPAttachIsIdempotent: logging in again attaches nothing and is not
// an error. The claim is about the linker's own answer (0 rows), not just about
// the login succeeding.
func TestVerifyOTPAttachIsIdempotent(t *testing.T) {
	linker := &countingLinker{fakeBookingLinker: fakeBookingLinker{
		rows: []*fakeBooking{{phone: "+77015550003"}},
	}}
	users := newFakeUsers()
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 5, OTPPerHour: 20, OTPDevExpose: true}
	uc := NewOTPUseCase(users, newFakeOTP(), newFakeRefresh(), linker, noTx{}, testIssuer(t), &stubSender{}, cfg)

	login(t, uc, "+77015550003")
	login(t, uc, "+77015550003")

	if want := []int64{1, 0}; !equalInt64(linker.attached, want) {
		t.Fatalf("attached per login = %v, want %v", linker.attached, want)
	}
}

// TestVerifyOTPFailsWhenAttachFails: the attach is part of the login, not a
// side effect of it. If it cannot be written, the login must fail so that the
// transaction takes the user row with it — a guest created without their
// history is exactly the state this whole design exists to prevent.
func TestVerifyOTPFailsWhenAttachFails(t *testing.T) {
	boom := errors.New("bookings unavailable")
	uc, _ := newTestOTPWithLinker(t, &fakeBookingLinker{err: boom})
	ctx := context.Background()

	code, err := uc.RequestOTP(ctx, "+77015550004")
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	pair, err := uc.VerifyOTP(ctx, "+77015550004", code)
	if !errors.Is(err, boom) {
		t.Fatalf("VerifyOTP err = %v, want %v", err, boom)
	}
	if pair != nil {
		t.Error("no token pair may be issued when the attach failed")
	}
}

// TestVerifyPhoneChangeAttachesBookingsOfTheNewPhone — the same rule on the
// phone-change path: the caller has just proved ownership of the new number, so
// the account-less bookings made for it become theirs, while the bookings that
// came with the OLD number stay exactly where they are (they already have an
// owner — this same user).
func TestVerifyPhoneChangeAttachesBookingsOfTheNewPhone(t *testing.T) {
	users := newFakeUsers()
	otp := newFakeOTP()
	linker := &fakeBookingLinker{}
	cfg := Config{RefreshTTL: time.Hour, OTPTTL: 5 * time.Minute, OTPPerMin: 5, OTPPerHour: 20, OTPDevExpose: true}
	uc := NewOTPUseCase(users, otp, newFakeRefresh(), linker, noTx{}, testIssuer(t), &stubSender{}, cfg)
	ctx := context.Background()

	u := seedUser(t, users, "+7 701 111 0000")
	old := u.ID
	linker.rows = []*fakeBooking{
		{phone: "+77011110000", owner: &old}, // history under the old number
		{phone: "+77072223333"},              // booked by phone under the new number
		{phone: "+77072224444"},              // an unrelated number
	}

	newPhone := "+7 707 222 3333"
	code, err := uc.RequestPhoneChangeOTP(ctx, u.ID, newPhone)
	if err != nil {
		t.Fatalf("RequestPhoneChangeOTP: %v", err)
	}
	if _, err := uc.VerifyPhoneChange(ctx, u.ID, newPhone, code); err != nil {
		t.Fatalf("VerifyPhoneChange: %v", err)
	}

	if got := linker.rows[1].owner; got == nil || *got != u.ID {
		t.Errorf("booking of the new number owner = %v, want %s", got, u.ID)
	}
	if got := linker.rows[0].owner; got == nil || *got != u.ID {
		t.Errorf("history under the old number must be untouched, owner = %v", got)
	}
	if linker.rows[2].owner != nil {
		t.Errorf("unrelated booking was attached: owner = %v", *linker.rows[2].owner)
	}
	if want, got := phone.Normalize(newPhone), users.byID[u.ID].Phone; got == nil || *got != want {
		t.Errorf("phone = %v, want %s", got, want)
	}
}

// countingLinker records what each attach call answered, so idempotence can be
// asserted on the counts themselves rather than inferred from the rows.
type countingLinker struct {
	fakeBookingLinker
	attached []int64
}

func (c *countingLinker) AttachOrphanedByPhone(ctx context.Context, userID uuid.UUID, p string) (int64, error) {
	n, err := c.fakeBookingLinker.AttachOrphanedByPhone(ctx, userID, p)
	c.attached = append(c.attached, n)
	return n, err
}

func equalInt64(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

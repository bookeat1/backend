package notifications

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// RE-REGISTRATION. The same token registered twice by the same guest is ONE
// row, refreshed in place — a phone that re-opens the app every day must not
// accumulate a row per launch.
func TestDeviceTokenReRegistrationDoesNotDuplicate(t *testing.T) {
	repo := newFakeDeviceTokens()
	uc := NewDeviceTokenUseCase(repo)
	uid := uuid.New()
	in := RegisterDeviceInput{Token: "ExponentPushToken[abc]", Platform: domain.PlatformIOS}

	first, err := uc.Register(context.Background(), uid, in)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	second, err := uc.Register(context.Background(), uid, in)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if repo.rowCount() != 1 {
		t.Fatalf("%d rows after re-registering the same token, want 1", repo.rowCount())
	}
	if first.ID != second.ID {
		t.Fatalf("re-registration created a new row (%s → %s)", first.ID, second.ID)
	}
}

// RE-REGISTRATION ACROSS ACCOUNTS. A device that changed hands is re-pointed to
// the CURRENT account — the previous owner must stop receiving that guest's
// booking notifications immediately.
func TestDeviceTokenReRegistrationRepointsToCurrentAccount(t *testing.T) {
	repo := newFakeDeviceTokens()
	uc := NewDeviceTokenUseCase(repo)
	oldOwner, newOwner := uuid.New(), uuid.New()
	in := RegisterDeviceInput{Token: "ExponentPushToken[shared]", Platform: domain.PlatformAndroid}

	row, err := uc.Register(context.Background(), oldOwner, in)
	if err != nil {
		t.Fatalf("register by the old owner: %v", err)
	}
	if _, err := uc.Register(context.Background(), newOwner, in); err != nil {
		t.Fatalf("register by the new owner: %v", err)
	}

	if repo.rowCount() != 1 {
		t.Fatalf("%d rows after a device changed hands, want 1", repo.rowCount())
	}
	if got := repo.get(row.ID).UserID; got != newOwner {
		t.Fatalf("token still points at %s, want the current account %s", got, newOwner)
	}
	stale, err := repo.ListActiveByUser(context.Background(), oldOwner)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("the previous owner still has %d live device(s)", len(stale))
	}
}

// A re-registration after the token was deactivated (provider said "gone", then
// the app produced the same token again) brings the row back to life.
func TestDeviceTokenReRegistrationReactivates(t *testing.T) {
	repo := newFakeDeviceTokens()
	uc := NewDeviceTokenUseCase(repo)
	uid := uuid.New()
	in := RegisterDeviceInput{Token: "ExponentPushToken[revive]", Platform: domain.PlatformIOS}

	row, err := uc.Register(context.Background(), uid, in)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := repo.DeactivateByID(context.Background(), row.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := uc.Register(context.Background(), uid, in); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !repo.isActive(row.ID) {
		t.Fatal("re-registering a deactivated token did not reactivate it")
	}
}

// Unregister is scoped to the caller: knowing another guest's exact token is not
// enough to silence their device.
func TestDeviceTokenUnregisterIsOwnerScoped(t *testing.T) {
	repo := newFakeDeviceTokens()
	uc := NewDeviceTokenUseCase(repo)
	owner, attacker := uuid.New(), uuid.New()
	row, err := uc.Register(context.Background(), owner, RegisterDeviceInput{
		Token: "ExponentPushToken[victim]", Platform: domain.PlatformIOS,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := uc.Unregister(context.Background(), attacker, row.Token); err != nil {
		t.Fatalf("unregister by a stranger must be a silent no-op, got %v", err)
	}
	if !repo.isActive(row.ID) {
		t.Fatal("a stranger silenced someone else's device")
	}
	if err := uc.Unregister(context.Background(), owner, row.Token); err != nil {
		t.Fatalf("unregister by the owner: %v", err)
	}
	if repo.isActive(row.ID) {
		t.Fatal("the owner's own unregister did not take effect")
	}
	// Idempotent: a second unregister is still a success.
	if err := uc.Unregister(context.Background(), owner, row.Token); err != nil {
		t.Fatalf("second unregister: %v", err)
	}
}

func TestDeviceTokenRegisterValidation(t *testing.T) {
	uc := NewDeviceTokenUseCase(newFakeDeviceTokens())
	uid := uuid.New()
	cases := map[string]RegisterDeviceInput{
		"empty token":      {Token: "   ", Platform: domain.PlatformIOS},
		"oversized token":  {Token: strings.Repeat("x", maxDeviceTokenLen+1), Platform: domain.PlatformIOS},
		"unknown platform": {Token: "ExponentPushToken[x]", Platform: "symbian"},
		"missing platform": {Token: "ExponentPushToken[x]", Platform: ""},
	}
	for name, in := range cases {
		if _, err := uc.Register(context.Background(), uid, in); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

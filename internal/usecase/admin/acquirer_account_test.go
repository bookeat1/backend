package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeAcquirerAccounts is the venue↔acquirer mapping store.
type fakeAcquirerAccounts struct {
	stored map[string]*domain.RestaurantSplitAccount
	writes int
}

func newFakeAcquirerAccounts() *fakeAcquirerAccounts {
	return &fakeAcquirerAccounts{stored: map[string]*domain.RestaurantSplitAccount{}}
}

func acquirerKey(provider domain.PaymentProvider, restaurantID uuid.UUID) string {
	return string(provider) + ":" + restaurantID.String()
}

func (f *fakeAcquirerAccounts) GetActive(_ context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error) {
	a, ok := f.stored[acquirerKey(provider, restaurantID)]
	if !ok || !a.IsActive {
		// The repo answers ErrNotFound for a deactivated mapping too — the
		// checkout must treat suspended exactly like never-onboarded.
		return nil, domain.ErrNotFound
	}
	return a, nil
}

func (f *fakeAcquirerAccounts) Upsert(_ context.Context, a *domain.RestaurantSplitAccount) error {
	if err := a.Validate(); err != nil {
		return err
	}
	f.writes++
	stored := *a
	f.stored[acquirerKey(a.Provider, a.RestaurantID)] = &stored
	return nil
}

// newAcquirerHarness is newHarness with the acquirer-account store wired in.
func newAcquirerHarness(grant map[string]bool) (*harness, *fakeAcquirerAccounts) {
	h := newHarness(grant)
	accounts := newFakeAcquirerAccounts()
	WithAcquirerAccounts(accounts)(h.uc)
	return h, accounts
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

func TestSetAcquirerAccountStoresTheVenuesKaspiCompany(t *testing.T) {
	h, accounts := newAcquirerHarness(nil)
	rid := uuid.New()

	got, err := h.uc.SetAcquirerAccount(context.Background(), superadmin(), rid, AcquirerAccount{
		Provider: domain.ProviderKaspi, AccountRef: " 7 ", IsActive: true,
	})
	if err != nil {
		t.Fatalf("SetAcquirerAccount() error = %v", err)
	}
	if got.AccountRef != "7" {
		t.Fatalf("account_ref = %q, want the trimmed %q", got.AccountRef, "7")
	}
	stored := accounts.stored[acquirerKey(domain.ProviderKaspi, rid)]
	if stored == nil || stored.AccountRef != "7" || !stored.IsActive {
		t.Fatalf("stored mapping = %+v, want an active kaspi company 7", stored)
	}
}

// A venue owner may READ where their money goes but must never be able to
// change it: that string decides whose bank account a guest's money lands in.
func TestSetAcquirerAccountIsRefusedToAVenueOwner(t *testing.T) {
	owner, rid := uuid.New(), uuid.New()
	// The owner holds restaurant.manage at this very venue and STILL may not
	// move where its money lands.
	h, accounts := newAcquirerHarness(map[string]bool{key(owner, rid, domain.PermRestaurantManage): true})

	_, err := h.uc.SetAcquirerAccount(context.Background(), Actor{UserID: owner, Role: domain.RoleRestaurant}, rid,
		AcquirerAccount{Provider: domain.ProviderKaspi, AccountRef: "9", IsActive: true})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want domain.ErrForbidden", err)
	}
	if accounts.writes != 0 {
		t.Fatalf("the store was written %d times by a non-platform actor, want 0", accounts.writes)
	}
}

func TestSetAcquirerAccountRefusesAnEmptyOrUnknownTarget(t *testing.T) {
	h, accounts := newAcquirerHarness(nil)
	rid := uuid.New()
	ctx := context.Background()

	cases := map[string]AcquirerAccount{
		"no account ref":   {Provider: domain.ProviderKaspi, AccountRef: "   ", IsActive: true},
		"unknown provider": {Provider: domain.PaymentProvider("kaspiii"), AccountRef: "7", IsActive: true},
		"absurdly long":    {Provider: domain.ProviderKaspi, AccountRef: strings.Repeat("7", 200), IsActive: true},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.uc.SetAcquirerAccount(ctx, superadmin(), rid, in); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want domain.ErrValidation", err)
			}
		})
	}
	if accounts.writes != 0 {
		t.Fatalf("the store was written %d times for invalid input, want 0", accounts.writes)
	}
}

func TestGetAcquirerAccountReportsAVenueWithNoMappingAsNotConnected(t *testing.T) {
	owner, rid := uuid.New(), uuid.New()
	h, _ := newAcquirerHarness(map[string]bool{key(owner, rid, domain.PermRestaurantManage): true})

	got, err := h.uc.GetAcquirerAccount(context.Background(),
		Actor{UserID: owner, Role: domain.RoleRestaurant}, rid, domain.ProviderKaspi)
	if err != nil {
		t.Fatalf("GetAcquirerAccount() error = %v — a venue nobody onboarded is an ordinary state, not an error", err)
	}
	if got.AccountRef != "" {
		t.Fatalf("account_ref = %q, want empty", got.AccountRef)
	}
}

// A deactivated mapping must read back as "not connected": the checkout
// refuses such a venue, and the panel must not claim it is set up.
func TestGetAcquirerAccountTreatsASuspendedMappingAsMissing(t *testing.T) {
	owner, rid := uuid.New(), uuid.New()
	h, accounts := newAcquirerHarness(map[string]bool{key(owner, rid, domain.PermRestaurantManage): true})
	accounts.stored[acquirerKey(domain.ProviderKaspi, rid)] = &domain.RestaurantSplitAccount{
		RestaurantID: rid, Provider: domain.ProviderKaspi, AccountRef: "7", IsActive: false,
	}

	got, err := h.uc.GetAcquirerAccount(context.Background(),
		Actor{UserID: owner, Role: domain.RoleRestaurant}, rid, domain.ProviderKaspi)
	if err != nil {
		t.Fatalf("GetAcquirerAccount() error = %v", err)
	}
	if got.AccountRef != "" {
		t.Fatalf("account_ref = %q, want empty for a suspended mapping", got.AccountRef)
	}
}

func TestGetAcquirerAccountIsRefusedToAStranger(t *testing.T) {
	h, _ := newAcquirerHarness(nil) // no permission granted anywhere

	_, err := h.uc.GetAcquirerAccount(context.Background(),
		Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}, uuid.New(), domain.ProviderKaspi)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want domain.ErrForbidden", err)
	}
}

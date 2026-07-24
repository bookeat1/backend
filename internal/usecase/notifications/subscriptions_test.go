package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeManagers is a minimal RestaurantManagerRepository: only ListByUser is
// exercised by the subscription usecase's membership check.
type fakeManagers struct {
	byUser map[uuid.UUID][]domain.RestaurantManager
}

func (f *fakeManagers) ListByRestaurant(context.Context, uuid.UUID) ([]domain.RestaurantManager, error) {
	return nil, nil
}
func (f *fakeManagers) ListByUser(_ context.Context, uid uuid.UUID) ([]domain.RestaurantManager, error) {
	return f.byUser[uid], nil
}
func (f *fakeManagers) GetByID(context.Context, uuid.UUID) (*domain.RestaurantManager, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeManagers) Create(context.Context, *domain.RestaurantManager) error       { return nil }
func (f *fakeManagers) UpdateRole(context.Context, uuid.UUID, domain.StaffRole) error { return nil }
func (f *fakeManagers) Delete(context.Context, uuid.UUID) error                       { return nil }

func validRegister(rest uuid.UUID) RegisterInput {
	return RegisterInput{RestaurantID: rest, Endpoint: "https://push/abc", P256dh: "p256", Auth: "auth"}
}

// A staff member may register a subscription for a restaurant they work at.
func TestSubscriptions_RegisterAllowedForStaff(t *testing.T) {
	user := uuid.New()
	rest := uuid.New()
	subs := newFakeSubs()
	managers := &fakeManagers{byUser: map[uuid.UUID][]domain.RestaurantManager{
		user: {{UserID: user, RestaurantID: rest, Role: domain.StaffRoleHostess}},
	}}
	uc := NewSubscriptionUseCase(subs, managers)

	if err := uc.Register(context.Background(), user, false, validRegister(rest)); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, _ := subs.ListByRestaurant(context.Background(), rest)
	if len(got) != 1 || got[0].UserID != user {
		t.Fatalf("subscription not stored for the caller: %+v", got)
	}
}

// A caller who is NOT staff of the target restaurant is forbidden — even if
// they are staff somewhere else (no cross-tenant registration).
func TestSubscriptions_RegisterForbiddenForNonStaff(t *testing.T) {
	user := uuid.New()
	otherRest := uuid.New()
	targetRest := uuid.New()
	subs := newFakeSubs()
	managers := &fakeManagers{byUser: map[uuid.UUID][]domain.RestaurantManager{
		user: {{UserID: user, RestaurantID: otherRest, Role: domain.StaffRoleOwner}},
	}}
	uc := NewSubscriptionUseCase(subs, managers)

	err := uc.Register(context.Background(), user, false, validRegister(targetRest))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("register for a non-staff restaurant = %v, want ErrForbidden", err)
	}
	if got, _ := subs.ListByRestaurant(context.Background(), targetRest); len(got) != 0 {
		t.Fatal("a forbidden registration still stored a subscription")
	}
}

// A superadmin may register for any restaurant, bypassing the membership check.
func TestSubscriptions_RegisterSuperadminBypass(t *testing.T) {
	admin := uuid.New()
	rest := uuid.New()
	subs := newFakeSubs()
	managers := &fakeManagers{byUser: map[uuid.UUID][]domain.RestaurantManager{}}
	uc := NewSubscriptionUseCase(subs, managers)

	if err := uc.Register(context.Background(), admin, true, validRegister(rest)); err != nil {
		t.Fatalf("superadmin register: %v", err)
	}
	if got, _ := subs.ListByRestaurant(context.Background(), rest); len(got) != 1 {
		t.Fatal("superadmin registration not stored")
	}
}

func TestSubscriptions_RegisterValidatesRequiredFields(t *testing.T) {
	uc := NewSubscriptionUseCase(newFakeSubs(), &fakeManagers{})
	err := uc.Register(context.Background(), uuid.New(), true, RegisterInput{RestaurantID: uuid.New(), Endpoint: ""})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty endpoint = %v, want ErrValidation", err)
	}
}

// Unregister only removes the caller's OWN subscription, never another user's
// even with the exact endpoint.
func TestSubscriptions_UnregisterScopedToCaller(t *testing.T) {
	owner := uuid.New()
	attacker := uuid.New()
	rest := uuid.New()
	victimSub := domain.PushSubscription{ID: uuid.New(), UserID: owner, RestaurantID: rest, Endpoint: "shared-endpoint", P256dh: "p", Auth: "a"}
	subs := newFakeSubs(victimSub)
	uc := NewSubscriptionUseCase(subs, &fakeManagers{})

	// Attacker tries to remove the owner's endpoint — no-op, and the row survives.
	if err := uc.Unregister(context.Background(), attacker, "shared-endpoint"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if !subs.has(victimSub.ID) {
		t.Fatal("another user's subscription was deleted (cross-user unregister)")
	}

	// The owner removes their own — gone.
	if err := uc.Unregister(context.Background(), owner, "shared-endpoint"); err != nil {
		t.Fatalf("owner unregister: %v", err)
	}
	if subs.has(victimSub.ID) {
		t.Fatal("owner could not remove their own subscription")
	}
}

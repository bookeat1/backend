package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeMembershipReader answers ListMembershipsByUser from a per-user map, so a
// query for user X can NEVER return user Y's rows — this is what lets the
// cross-tenant-leak test below actually prove isolation at the usecase seam
// (the production guarantee is the SQL WHERE user_id=$1; the repo integration
// test proves that half).
type fakeMembershipReader struct {
	byUser map[uuid.UUID][]domain.StaffMembership
	err    error
	// gotUser records the id the usecase queried with.
	gotUser uuid.UUID
}

func (f *fakeMembershipReader) ListMembershipsByUser(_ context.Context, userID uuid.UUID) ([]domain.StaffMembership, error) {
	f.gotUser = userID
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

type fakeBriefReader struct {
	all    []domain.RestaurantBrief
	err    error
	called bool
}

func (f *fakeBriefReader) ListManageableBrief(_ context.Context) ([]domain.RestaurantBrief, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return f.all, nil
}

func TestMyRestaurants_ManagesTwo_ReturnsBothWithRoles(t *testing.T) {
	userA := uuid.New()
	restA, restB := uuid.New(), uuid.New()
	mr := &fakeMembershipReader{byUser: map[uuid.UUID][]domain.StaffMembership{
		userA: {
			{RestaurantID: restA, Name: "Alpha", Role: domain.StaffRoleOwner},
			{RestaurantID: restB, Name: "Bravo", Role: domain.StaffRoleHostess},
		},
	}}
	br := &fakeBriefReader{}
	uc := NewMyRestaurantsUseCase(mr, br)

	got, err := uc.List(context.Background(), Actor{UserID: userA, Role: domain.RoleRestaurant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if mr.gotUser != userA {
		t.Fatalf("queried memberships for %s, want caller %s", mr.gotUser, userA)
	}
	if br.called {
		t.Fatalf("non-admin caller must not trigger the platform-wide brief read")
	}
	roles := map[uuid.UUID]string{}
	for _, r := range got {
		roles[r.RestaurantID] = r.Role
	}
	if len(roles) != 2 || roles[restA] != "owner" || roles[restB] != "hostess" {
		t.Fatalf("want {A:owner, B:hostess}, got %+v", roles)
	}
}

func TestMyRestaurants_NoMembership_ReturnsEmptyNonNil(t *testing.T) {
	mr := &fakeMembershipReader{byUser: map[uuid.UUID][]domain.StaffMembership{}}
	uc := NewMyRestaurantsUseCase(mr, &fakeBriefReader{})

	got, err := uc.List(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatalf("result must be a non-nil slice so it serializes as [], not null")
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}

// A caller must never see a restaurant they have no membership in. userB is
// staff of restB; userA is staff of restA only. Listing as userA must yield
// exactly restA and never restB.
func TestMyRestaurants_NoCrossTenantLeak(t *testing.T) {
	userA, userB := uuid.New(), uuid.New()
	restA, restB := uuid.New(), uuid.New()
	mr := &fakeMembershipReader{byUser: map[uuid.UUID][]domain.StaffMembership{
		userA: {{RestaurantID: restA, Name: "Alpha", Role: domain.StaffRoleManager}},
		userB: {{RestaurantID: restB, Name: "Bravo", Role: domain.StaffRoleOwner}},
	}}
	uc := NewMyRestaurantsUseCase(mr, &fakeBriefReader{})

	got, err := uc.List(context.Background(), Actor{UserID: userA, Role: domain.RoleRestaurant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].RestaurantID != restA {
		t.Fatalf("want exactly [restA], got %+v", got)
	}
	for _, r := range got {
		if r.RestaurantID == restB {
			t.Fatalf("cross-tenant leak: userA was shown restB")
		}
	}
}

func TestMyRestaurants_Superadmin_GetsAllVenuesAsAdmin(t *testing.T) {
	restA, restB := uuid.New(), uuid.New()
	mr := &fakeMembershipReader{byUser: map[uuid.UUID][]domain.StaffMembership{}}
	br := &fakeBriefReader{all: []domain.RestaurantBrief{
		{ID: restA, Name: "Alpha"},
		{ID: restB, Name: "Bravo"},
	}}
	uc := NewMyRestaurantsUseCase(mr, br)

	got, err := uc.List(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !br.called {
		t.Fatalf("superadmin must read the platform-wide brief list")
	}
	if mr.gotUser != uuid.Nil {
		t.Fatalf("superadmin path must not query per-user memberships")
	}
	if len(got) != 2 {
		t.Fatalf("want 2 venues, got %d", len(got))
	}
	for _, r := range got {
		if r.Role != string(domain.RoleAdmin) {
			t.Fatalf("superadmin entries must carry role %q, got %q", domain.RoleAdmin, r.Role)
		}
	}
}

func TestMyRestaurants_PropagatesReaderError(t *testing.T) {
	sentinel := errors.New("boom")
	mr := &fakeMembershipReader{err: sentinel}
	uc := NewMyRestaurantsUseCase(mr, &fakeBriefReader{})

	if _, err := uc.List(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}); !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel error, got %v", err)
	}
}

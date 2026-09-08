package roles

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeRoleRepo struct {
	admins  int
	changes []domain.UserRoleChange
	err     error
}

func (f *fakeRoleRepo) SetRole(_ context.Context, c domain.UserRoleChange) error {
	if f.err != nil {
		return f.err
	}
	f.changes = append(f.changes, c)
	return nil
}
func (f *fakeRoleRepo) CountByRole(context.Context, domain.Role) (int, error) { return f.admins, nil }
func (f *fakeRoleRepo) History(context.Context, uuid.UUID, int) ([]domain.UserRoleChange, error) {
	return nil, nil
}
func (f *fakeRoleRepo) Search(context.Context, string, int) ([]domain.User, error) { return nil, nil }

type fakeUsers struct {
	byID    map[uuid.UUID]*domain.User
	byEmail map[string]*domain.User
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByEmail(_ context.Context, e string) (*domain.User, error) {
	if u, ok := f.byEmail[e]; ok {
		return u, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (f *fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) Update(context.Context, *domain.User) error { return nil }
func (f *fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

func user(role domain.Role) *domain.User {
	return &domain.User{ID: uuid.New(), Role: role}
}

func harness(admins int, users ...*domain.User) (*UseCase, *fakeRoleRepo) {
	repo := &fakeRoleRepo{admins: admins}
	fu := &fakeUsers{byID: map[uuid.UUID]*domain.User{}, byEmail: map[string]*domain.User{}}
	for _, u := range users {
		fu.byID[u.ID] = u
		if u.Email != nil {
			fu.byEmail[*u.Email] = u
		}
	}
	return NewUseCase(repo, fu), repo
}

// This endpoint hands out the rights to every other admin endpoint, so the very
// first thing it must refuse is a caller who is not an administrator.
func TestOnlyAnAdministratorMayChangeRoles(t *testing.T) {
	target := user(domain.RoleUser)
	uc, repo := harness(2, target)

	for _, role := range []domain.Role{domain.RoleUser, domain.RoleRestaurant} {
		err := uc.SetRole(context.Background(), Actor{UserID: uuid.New(), Role: role}, target.ID, domain.RoleAdmin, nil)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("role %q: want ErrForbidden, got %v", role, err)
		}
	}
	if len(repo.changes) != 0 {
		t.Fatal("a non-administrator must not produce a role change")
	}
}

// A typo must fail loudly here, not silently store a role nothing recognises
// and leave the user denied everything.
func TestUnknownRoleIsRefused(t *testing.T) {
	target := user(domain.RoleUser)
	uc, repo := harness(2, target)
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	err := uc.SetRole(context.Background(), admin, target.ID, domain.Role("superuser"), nil)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if len(repo.changes) != 0 {
		t.Fatal("an unknown role must not be written")
	}
}

func TestPromotionIsRecordedWithActorAndReason(t *testing.T) {
	target := user(domain.RoleUser)
	uc, repo := harness(1, target)
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}
	reason := "берёт на себя платформенную поддержку"

	if err := uc.SetRole(context.Background(), admin, target.ID, domain.RoleAdmin, &reason); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if len(repo.changes) != 1 {
		t.Fatalf("want one audit row, got %d", len(repo.changes))
	}
	c := repo.changes[0]
	if c.ActorID == nil || *c.ActorID != admin.UserID {
		t.Fatalf("the audit must name who did it, got %v", c.ActorID)
	}
	if c.FromRole != domain.RoleUser || c.ToRole != domain.RoleAdmin {
		t.Fatalf("wrong transition recorded: %s -> %s", c.FromRole, c.ToRole)
	}
	if c.Reason == nil || *c.Reason != reason {
		t.Fatalf("reason lost: %v", c.Reason)
	}
}

// Setting the role a user already has is not a change, and recording it would
// make the history describe something that never happened.
func TestSettingTheSameRoleWritesNothing(t *testing.T) {
	target := user(domain.RoleAdmin)
	uc, repo := harness(2, target)
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	if err := uc.SetRole(context.Background(), admin, target.ID, domain.RoleAdmin, nil); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if len(repo.changes) != 0 {
		t.Fatal("a no-op must not produce an audit row")
	}
}

// The usual way to lock yourself out is to demote the account you are signed in
// as and then have nobody left who can undo it.
func TestAnAdministratorCannotDemoteThemselves(t *testing.T) {
	me := user(domain.RoleAdmin)
	uc, repo := harness(5, me)
	admin := Actor{UserID: me.ID, Role: domain.RoleAdmin}

	err := uc.SetRole(context.Background(), admin, me.ID, domain.RoleUser, nil)

	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if len(repo.changes) != 0 {
		t.Fatal("self-demotion must not be written")
	}
}

// A platform with zero administrators can only be repaired from the database —
// the exact state this feature exists to leave behind.
func TestTheLastAdministratorCannotBeDemoted(t *testing.T) {
	last := user(domain.RoleAdmin)
	uc, repo := harness(1, last)
	someoneElse := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	err := uc.SetRole(context.Background(), someoneElse, last.ID, domain.RoleUser, nil)

	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if len(repo.changes) != 0 {
		t.Fatal("the last administrator must survive")
	}
}

func TestAnAdministratorCanBeDemotedWhileOthersRemain(t *testing.T) {
	other := user(domain.RoleAdmin)
	uc, repo := harness(3, other)
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	if err := uc.SetRole(context.Background(), admin, other.ID, domain.RoleUser, nil); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if len(repo.changes) != 1 {
		t.Fatal("a demotion with other admins around must go through")
	}
}

// The bootstrap answers "where does the first administrator come from" and must
// do nothing in every other situation.
func TestBootstrapPromotesOnlyWhenNobodyIsAdmin(t *testing.T) {
	email := "owner@example.com"
	owner := user(domain.RoleUser)
	owner.Email = &email

	t.Run("promotes on an empty platform", func(t *testing.T) {
		uc, repo := harness(0, owner)
		if err := uc.EnsureBootstrapAdmin(context.Background(), email); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if len(repo.changes) != 1 || repo.changes[0].ToRole != domain.RoleAdmin {
			t.Fatalf("want a promotion, got %+v", repo.changes)
		}
		// The platform did it, not a person: naming an actor here would be a lie.
		if repo.changes[0].ActorID != nil {
			t.Fatal("the bootstrap must not attribute itself to a user")
		}
	})

	t.Run("does nothing when an administrator exists", func(t *testing.T) {
		uc, repo := harness(1, owner)
		if err := uc.EnsureBootstrapAdmin(context.Background(), email); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if len(repo.changes) != 0 {
			t.Fatal("the bootstrap must not re-grant rights somebody removed on purpose")
		}
	})

	t.Run("does nothing without an email", func(t *testing.T) {
		uc, repo := harness(0, owner)
		if err := uc.EnsureBootstrapAdmin(context.Background(), "   "); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if len(repo.changes) != 0 {
			t.Fatal("an unset email must be a no-op")
		}
	})

	t.Run("is patient when the owner has not signed up yet", func(t *testing.T) {
		uc, repo := harness(0)
		if err := uc.EnsureBootstrapAdmin(context.Background(), "nobody@example.com"); err != nil {
			t.Fatalf("an unknown email must not fail startup: %v", err)
		}
		if len(repo.changes) != 0 {
			t.Fatal("nothing to promote")
		}
	})
}

package users

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/users"
)

// Hand-written fakes (repo convention: no mock framework), same shape as
// bookings'/payments' fakes_test.go.

// --- auth plumbing: the test router runs the real middleware.Auth, so tests
// exercise the same AuthUser path as production. The access token is simply
// the user id.

type fakeIssuer struct{}

func (fakeIssuer) IssueAccess(id uuid.UUID, role string) (string, time.Time, error) {
	return id.String(), time.Now().Add(time.Hour), nil
}

func (fakeIssuer) ParseAccess(token string) (uuid.UUID, string, error) {
	id, err := uuid.Parse(token)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("bad token")
	}
	return id, "", nil
}

// fakeUsers backs middleware.Auth's user lookup — one fixed user, always
// active, so every test id authenticates.
type fakeUsers struct{}

func (fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	return &domain.User{ID: id, Role: domain.RoleUser, IsActive: true}, nil
}
func (fakeUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (fakeUsers) Update(context.Context, *domain.User) error { return nil }
func (fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

// fakeFacade is a scriptable uc.Facade: it records the last id each method was
// called with, so handler tests can assert the handler never leaks another
// user's id into the facade call.
type fakeFacade struct {
	user       *domain.User
	cuisineIDs []uuid.UUID
	err        error

	lastMeID         uuid.UUID
	lastUpdateID     uuid.UUID
	lastUpdateIn     uc.UpdateInput
	lastDeleteID     uuid.UUID
	deleteCalled     int
	cuisineCalledFor uuid.UUID
}

func (f *fakeFacade) Me(_ context.Context, id uuid.UUID) (*domain.User, error) {
	f.lastMeID = id
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func (f *fakeFacade) UpdateMe(_ context.Context, id uuid.UUID, in uc.UpdateInput) (*domain.User, error) {
	f.lastUpdateID = id
	f.lastUpdateIn = in
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func (f *fakeFacade) CuisinePreferences(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	f.cuisineCalledFor = id
	if f.err != nil {
		return nil, f.err
	}
	return f.cuisineIDs, nil
}

func (f *fakeFacade) DeleteMe(_ context.Context, id uuid.UUID) error {
	f.lastDeleteID = id
	f.deleteCalled++
	return f.err
}

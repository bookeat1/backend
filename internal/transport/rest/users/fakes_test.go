package users

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	authuc "backend-core/internal/usecase/auth"
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
	lastAvatarID     uuid.UUID
	lastAvatarURL    string
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

func (f *fakeFacade) SetAvatarURL(_ context.Context, id uuid.UUID, url string) error {
	f.lastAvatarID, f.lastAvatarURL = id, url
	return f.err
}

// fakeOTP is a scriptable authuc.OTPUseCase. Only the two phone-change methods
// carry behavior; the login methods are unused by the users handler and panic
// if ever reached, so a wiring mistake fails loudly instead of silently.
type fakeOTP struct {
	code         string
	user         *domain.User
	requestErr   error
	verifyErr    error
	lastReqID    uuid.UUID
	lastReqPhone string
	lastVerID    uuid.UUID
	lastVerPhone string
	lastVerCode  string
}

func (f *fakeOTP) RequestOTP(context.Context, string) (string, error) {
	panic("unused by users handler")
}
func (f *fakeOTP) VerifyOTP(context.Context, string, string) (*authuc.TokenPair, error) {
	panic("unused by users handler")
}
func (f *fakeOTP) RequestPhoneChangeOTP(_ context.Context, id uuid.UUID, newPhone string) (string, error) {
	f.lastReqID, f.lastReqPhone = id, newPhone
	if f.requestErr != nil {
		return "", f.requestErr
	}
	return f.code, nil
}
func (f *fakeOTP) VerifyPhoneChange(_ context.Context, id uuid.UUID, newPhone, code string) (*domain.User, error) {
	f.lastVerID, f.lastVerPhone, f.lastVerCode = id, newPhone, code
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	return f.user, nil
}

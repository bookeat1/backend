package bookings

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/bookings"
)

// Hand-written fakes for the usecases and the two infrastructure ports the
// router needs (repo convention: no mock framework).

// --- auth plumbing: the test router runs the real middleware.Auth, so the
// tests exercise the same AuthUser → Actor path as production. The access
// token is simply the user id.

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

type fakeUsers struct {
	role     domain.Role
	inactive bool
}

func (f fakeUsers) Create(context.Context, *domain.User) error { return nil }
func (f fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	return &domain.User{ID: id, Role: f.role, IsActive: !f.inactive}, nil
}
func (f fakeUsers) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (f fakeUsers) Update(context.Context, *domain.User) error { return nil }

type fakeManagers struct{ manages bool }

func (f fakeManagers) Manages(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return f.manages, nil
}

// --- transaction manager: pass-through. The handler tests do not exercise
// rollback; the usecase and integration suites do.

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// --- idempotency store: an in-memory map with the same uniqueness rule as the
// (user_id, endpoint, idempotency_key) constraint.

type fakeKeys struct {
	rows map[string]domain.IdempotencyRecord
}

func newFakeKeys() *fakeKeys { return &fakeKeys{rows: map[string]domain.IdempotencyRecord{}} }

func (f *fakeKeys) key(userID uuid.UUID, endpoint, k string) string {
	return userID.String() + "|" + endpoint + "|" + k
}

func (f *fakeKeys) Get(_ context.Context, userID uuid.UUID, endpoint, k string) (*domain.IdempotencyRecord, error) {
	rec, ok := f.rows[f.key(userID, endpoint, k)]
	if !ok {
		return nil, fmt.Errorf("%w: idempotency key", domain.ErrNotFound)
	}
	return &rec, nil
}

func (f *fakeKeys) Insert(_ context.Context, r *domain.IdempotencyRecord) error {
	k := f.key(r.UserID, r.Endpoint, r.Key)
	if _, exists := f.rows[k]; exists {
		return fmt.Errorf("%w: idempotency key", domain.ErrAlreadyExists)
	}
	f.rows[k] = *r
	return nil
}

// --- usecases

type fakeCreate struct {
	calls int
	err   error
}

func (f *fakeCreate) Create(_ context.Context, _ uc.Actor, in uc.CreateInput) (*uc.BookingDetails, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls++
	guests := in.Guests
	return &uc.BookingDetails{Booking: domain.Booking{
		ID: uuid.New(), RestaurantID: in.RestaurantID, UserID: in.UserID,
		Name: in.Name, Guests: guests, StartsAt: in.StartsAt,
		Status: domain.BookingPending, Source: in.Source,
	}}, nil
}

type fakeFacade struct {
	details *uc.BookingDetails
	err     error
}

func (f *fakeFacade) Get(context.Context, uc.Actor, uuid.UUID) (*uc.BookingDetails, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.details, nil
}

func (f *fakeFacade) ListMine(context.Context, uc.Actor, domain.BookingFilter) ([]domain.Booking, int, error) {
	return nil, 0, f.err
}

func (f *fakeFacade) ListByRestaurant(context.Context, uc.Actor, uuid.UUID, domain.BookingFilter) ([]domain.Booking, int, error) {
	return nil, 0, f.err
}

func (f *fakeFacade) History(context.Context, uc.Actor, uuid.UUID) ([]domain.BookingStatusChange, error) {
	return nil, f.err
}

func (f *fakeFacade) Messages(context.Context, uc.Actor, uuid.UUID) ([]domain.BookingMessage, error) {
	return nil, f.err
}

func (f *fakeFacade) PostMessage(context.Context, uc.Actor, uuid.UUID, string) (*domain.BookingMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.BookingMessage{ID: uuid.New()}, nil
}

func (f *fakeFacade) MarkMessagesRead(context.Context, uc.Actor, uuid.UUID) (int, error) {
	return 0, f.err
}

func (f *fakeFacade) Survey(context.Context, uc.Actor, uuid.UUID) (*domain.RestaurantSurvey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.RestaurantSurvey{ID: uuid.New()}, nil
}

func (f *fakeFacade) SubmitSurvey(context.Context, uc.Actor, uuid.UUID, uc.SurveyInput) (*domain.RestaurantSurvey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.RestaurantSurvey{ID: uuid.New()}, nil
}

type fakeStatus struct{ err error }

func (f *fakeStatus) result() (*domain.Booking, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.Booking{ID: uuid.New(), Status: domain.BookingConfirmed}, nil
}

func (f *fakeStatus) Confirm(context.Context, uc.Actor, uuid.UUID, *string) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) Reject(context.Context, uc.Actor, uuid.UUID, *string) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) Arrive(context.Context, uc.Actor, uuid.UUID) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) Complete(context.Context, uc.Actor, uuid.UUID) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) NoShow(context.Context, uc.Actor, uuid.UUID, *string) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) Cancel(context.Context, uc.Actor, uuid.UUID, uc.CancelInput) (*domain.Booking, error) {
	return f.result()
}
func (f *fakeStatus) Waitlist(context.Context, uc.Actor, uuid.UUID, *string) (*domain.Booking, error) {
	return f.result()
}

type fakeUpdate struct{ err error }

func (f *fakeUpdate) Update(context.Context, uc.Actor, uuid.UUID, uc.UpdateInput) (*uc.BookingDetails, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &uc.BookingDetails{Booking: domain.Booking{ID: uuid.New()}}, nil
}

type fakeAvail struct{ err error }

func (f *fakeAvail) Day(_ context.Context, id uuid.UUID, date string, guests int) (*uc.DayAvailability, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &uc.DayAvailability{RestaurantID: id, Date: date, Guests: guests, Timezone: "Asia/Almaty"}, nil
}

type fakeBlacklist struct{ err error }

func (f *fakeBlacklist) List(context.Context, uc.Actor, uuid.UUID) ([]domain.BlacklistEntry, error) {
	return nil, f.err
}

func (f *fakeBlacklist) Add(context.Context, uc.Actor, uuid.UUID, uc.BlacklistInput) (*domain.BlacklistEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &domain.BlacklistEntry{ID: uuid.New(), IsActive: true}, nil
}

func (f *fakeBlacklist) Remove(context.Context, uc.Actor, uuid.UUID, uuid.UUID) error { return f.err }

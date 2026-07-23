package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/payments"
)

// Hand-written fakes for the usecases (repo convention: no mock framework),
// same shape as bookings' fakes_test.go.

// --- auth plumbing: the test router runs the real middleware.Auth /
// middleware.OptionalAuth, so tests exercise the same AuthUser → Actor path as
// production. The access token is simply the user id.

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
func (f fakeUsers) Delete(context.Context, uuid.UUID) error    { return nil }

// --- CreateUseCase ---

type fakeCreate struct {
	payment *domain.Payment
	err     error
	// lastActor/lastInput capture the call for assertions.
	lastActor uc.Actor
	lastInput uc.CreateInput
	called    int
}

func (f *fakeCreate) CreateForBooking(_ context.Context, actor uc.Actor, in uc.CreateInput) (*domain.Payment, error) {
	f.called++
	f.lastActor = actor
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

// --- CaptureUseCase / VoidUseCase ---

type fakeCapture struct {
	payment   *domain.Payment
	err       error
	lastActor uc.Actor
	called    int
}

func (f *fakeCapture) CaptureOnSeating(_ context.Context, actor uc.Actor, _ uuid.UUID) (*domain.Payment, error) {
	f.called++
	f.lastActor = actor
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

type fakeVoid struct {
	payment    *domain.Payment
	err        error
	lastActor  uc.Actor
	lastReason string
	called     int
}

func (f *fakeVoid) VoidOnRejection(_ context.Context, actor uc.Actor, _ uuid.UUID, reason string) (*domain.Payment, error) {
	f.called++
	f.lastActor = actor
	f.lastReason = reason
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

// --- RefundUseCase ---

type fakeRefund struct {
	payment   *domain.Payment
	err       error
	lastActor uc.Actor
	lastInput uc.SettleInput
	called    int
}

func (f *fakeRefund) Settle(_ context.Context, actor uc.Actor, _ uuid.UUID, in uc.SettleInput) (*domain.Payment, error) {
	f.called++
	f.lastActor = actor
	f.lastInput = in
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

// --- WebhookUseCase ---

type fakeWebhook struct {
	err          error
	lastProvider domain.PaymentProvider
	lastRaw      []byte
	lastHeaders  map[string]string
	called       int
}

func (f *fakeWebhook) HandleWebhook(_ context.Context, provider domain.PaymentProvider, raw []byte, headers map[string]string) error {
	f.called++
	f.lastProvider = provider
	f.lastRaw = raw
	f.lastHeaders = headers
	return f.err
}

// --- StatusUseCase ---

type fakeStatus struct {
	payment       *domain.Payment
	err           error
	lastActor     uc.Actor
	calledGet     int
	calledForBook int
}

func (f *fakeStatus) Get(_ context.Context, actor uc.Actor, _ uuid.UUID) (*domain.Payment, error) {
	f.calledGet++
	f.lastActor = actor
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

func (f *fakeStatus) GetForBooking(_ context.Context, actor uc.Actor, _ uuid.UUID) (*domain.Payment, error) {
	f.calledForBook++
	f.lastActor = actor
	if f.err != nil {
		return nil, f.err
	}
	return f.payment, nil
}

// samplePayment builds a minimal, valid domain.Payment for handler tests that
// do not care about the exact fields.
func samplePayment() *domain.Payment {
	now := time.Now()
	return &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, Purpose: domain.PurposeDeposit,
		Status: domain.PaymentCreated, AmountMinor: 10350, BaseAmountMinor: 10000,
		FeeMinor: 350, Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: now, UpdatedAt: now,
	}
}

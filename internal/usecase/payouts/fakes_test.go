package payouts

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeTx runs fn inline — no real transaction, good enough for usecase tests.
type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
func (fakeTx) Detach(ctx context.Context) context.Context { return ctx }

// fakePerms is the RBAC checker. allow governs a non-superadmin caller.
type fakePerms struct {
	allow   bool
	err     error
	gotPerm domain.Permission
	gotRest uuid.UUID
	gotUser uuid.UUID
	calls   int
}

func (f *fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	f.calls++
	f.gotPerm, f.gotRest, f.gotUser = perm, restaurantID, userID
	if f.err != nil {
		return false, f.err
	}
	return f.allow, nil
}

// fakeDestinations is an in-memory destination repo.
type fakeDestinations struct {
	byRestaurant map[uuid.UUID]*domain.PayoutDestination
	upserts      int
}

func newFakeDestinations() *fakeDestinations {
	return &fakeDestinations{byRestaurant: map[uuid.UUID]*domain.PayoutDestination{}}
}

func (f *fakeDestinations) Upsert(_ context.Context, d *domain.PayoutDestination) error {
	f.upserts++
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	cp := *d
	f.byRestaurant[d.RestaurantID] = &cp
	return nil
}

func (f *fakeDestinations) Get(_ context.Context, restaurantID uuid.UUID) (*domain.PayoutDestination, error) {
	if d, ok := f.byRestaurant[restaurantID]; ok {
		cp := *d
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

// fakePayouts is an in-memory payout repo with a real CAS check.
type fakePayouts struct {
	mu sync.Mutex
	m  map[uuid.UUID]*domain.Payout
}

func newFakePayouts() *fakePayouts { return &fakePayouts{m: map[uuid.UUID]*domain.Payout{}} }

func (f *fakePayouts) Create(_ context.Context, p *domain.Payout) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.m {
		if existing.IdempotencyKey == p.IdempotencyKey {
			return domain.ErrAlreadyExists
		}
	}
	cp := *p
	f.m[p.ID] = &cp
	return nil
}

func (f *fakePayouts) GetByID(_ context.Context, id uuid.UUID) (*domain.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.m[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakePayouts) GetByIdempotencyKey(_ context.Context, key string) (*domain.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.m {
		if p.IdempotencyKey == key {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakePayouts) CompareAndSwapStatus(_ context.Context, id uuid.UUID, from, to domain.PayoutStatus, patch domain.PayoutStatusPatch, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return domain.ErrNotFound
	}
	if p.Status != from {
		return domain.ErrAlreadyExists
	}
	p.Status = to
	p.StatusChangedAt = at
	p.ReconcileAttempts = 0
	p.NeedsManualReview = false
	if patch.ProviderRef != nil {
		p.ProviderRef = patch.ProviderRef
	}
	if patch.FailureCode != nil {
		p.FailureCode = patch.FailureCode
	}
	if patch.FailureReason != nil {
		p.FailureReason = patch.FailureReason
	}
	switch to {
	case domain.PayoutSent:
		p.SentAt = &at
	case domain.PayoutPaid:
		p.PaidAt = &at
	case domain.PayoutFailed:
		p.FailedAt = &at
	}
	return nil
}

func (f *fakePayouts) SetProviderRef(_ context.Context, id uuid.UUID, providerRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok || p.Status != domain.PayoutSent {
		return nil
	}
	if p.ProviderRef == nil || *p.ProviderRef == "" {
		p.ProviderRef = &providerRef
	}
	return nil
}

func (f *fakePayouts) ClaimStale(_ context.Context, statuses []domain.PayoutStatus, before time.Time, limit int) ([]domain.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Payout
	for _, p := range f.m {
		for _, s := range statuses {
			if p.Status == s && p.StatusChangedAt.Before(before) {
				out = append(out, *p)
			}
		}
	}
	return out, nil
}

func (f *fakePayouts) RecordReconcileAttempt(_ context.Context, id uuid.UUID, expectedStatus domain.PayoutStatus, at time.Time, maxAttempts int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return 0, false, domain.ErrNotFound
	}
	if p.Status != expectedStatus {
		return 0, false, domain.ErrAlreadyExists
	}
	p.ReconcileAttempts++
	p.LastReconcileAttemptAt = &at
	p.NeedsManualReview = p.ReconcileAttempts >= maxAttempts
	return p.ReconcileAttempts, p.NeedsManualReview, nil
}

func (f *fakePayouts) List(_ context.Context, restaurantID uuid.UUID, limit int) ([]domain.Payout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Payout
	for _, p := range f.m {
		if p.RestaurantID == restaurantID {
			out = append(out, *p)
		}
	}
	return out, nil
}

// fakeItems is an in-memory claim table enforcing UNIQUE(ledger_entry_id).
type fakeItems struct {
	mu            sync.Mutex
	byEntry       map[uuid.UUID]uuid.UUID // ledgerEntryID -> payoutID
	byPayout      map[uuid.UUID][]domain.PayoutItem
	deletedPayout []uuid.UUID
}

func newFakeItems() *fakeItems {
	return &fakeItems{byEntry: map[uuid.UUID]uuid.UUID{}, byPayout: map[uuid.UUID][]domain.PayoutItem{}}
}

func (f *fakeItems) CreateBatch(_ context.Context, items []domain.PayoutItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(items) == 0 {
		return domain.ErrValidation
	}
	// All-or-nothing, like a single INSERT: check first, then apply.
	for _, it := range items {
		if _, claimed := f.byEntry[it.LedgerEntryID]; claimed {
			return domain.ErrAlreadyExists
		}
	}
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		f.byEntry[items[i].LedgerEntryID] = items[i].PayoutID
		f.byPayout[items[i].PayoutID] = append(f.byPayout[items[i].PayoutID], items[i])
	}
	return nil
}

func (f *fakeItems) DeleteByPayout(_ context.Context, payoutID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedPayout = append(f.deletedPayout, payoutID)
	for _, it := range f.byPayout[payoutID] {
		delete(f.byEntry, it.LedgerEntryID)
	}
	delete(f.byPayout, payoutID)
	return nil
}

func (f *fakeItems) ListByPayout(_ context.Context, payoutID uuid.UUID) ([]domain.PayoutItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.PayoutItem(nil), f.byPayout[payoutID]...), nil
}

// fakeOwed returns preconfigured balances.
type fakeOwed struct {
	byRestaurant map[uuid.UUID][]domain.OwedBalance
	ids          []uuid.UUID
}

func (f *fakeOwed) OwedForRestaurant(_ context.Context, restaurantID uuid.UUID) ([]domain.OwedBalance, error) {
	return f.byRestaurant[restaurantID], nil
}
func (f *fakeOwed) OwedRestaurantIDs(_ context.Context) ([]uuid.UUID, error) { return f.ids, nil }

// fakeGateway counts payout dispatches and lets a test drive the outcome.
type fakeGateway struct {
	mu          sync.Mutex
	payoutCalls int
	getCalls    int
	payoutFn    func(req domain.PayoutRequest) (*domain.GatewayPayout, error)
	getFn       func(orderID string) (*domain.GatewayPayout, error)
}

func (f *fakeGateway) Name() domain.PaymentProvider { return domain.ProviderFreedomPay }

func (f *fakeGateway) Payout(_ context.Context, req domain.PayoutRequest) (*domain.GatewayPayout, error) {
	f.mu.Lock()
	f.payoutCalls++
	f.mu.Unlock()
	if f.payoutFn != nil {
		return f.payoutFn(req)
	}
	return &domain.GatewayPayout{ProviderRef: "prov-" + req.PayoutID.String(), Status: domain.PayoutPaid, Amount: req.Amount}, nil
}

func (f *fakeGateway) GetPayout(_ context.Context, orderID string) (*domain.GatewayPayout, error) {
	f.mu.Lock()
	f.getCalls++
	f.mu.Unlock()
	if f.getFn != nil {
		return f.getFn(orderID)
	}
	return &domain.GatewayPayout{Status: domain.PayoutPaid}, nil
}

// harness builds a UseCase over the fakes.
type harness struct {
	uc      *UseCase
	perms   *fakePerms
	dest    *fakeDestinations
	payouts *fakePayouts
	items   *fakeItems
	owed    *fakeOwed
	gw      *fakeGateway
}

func newHarness() *harness {
	perms := &fakePerms{allow: true}
	dest := newFakeDestinations()
	pays := newFakePayouts()
	items := newFakeItems()
	owed := &fakeOwed{byRestaurant: map[uuid.UUID][]domain.OwedBalance{}}
	gw := &fakeGateway{}
	uc := NewUseCase(Ports{
		Perms:        perms,
		Destinations: dest,
		Payouts:      pays,
		Items:        items,
		Owed:         owed,
		Gateway:      gw,
		Tx:           fakeTx{},
	}, nil)
	return &harness{uc: uc, perms: perms, dest: dest, payouts: pays, items: items, owed: owed, gw: gw}
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func staff() Actor      { return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant} }

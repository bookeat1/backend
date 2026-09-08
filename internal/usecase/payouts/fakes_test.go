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
		// Mirror of uq_payouts_venue_period (migration 0052): at most ONE
		// non-failed payout per (restaurant, currency, venue-local day). A
		// failed payout releases the day, and a NULL period (manual payout)
		// is unconstrained — NULLs are distinct in a unique index.
		if existing.Status != domain.PayoutFailed &&
			existing.RestaurantID == p.RestaurantID &&
			existing.Currency == p.Currency &&
			samePeriod(existing.PeriodDate, p.PeriodDate) {
			return domain.ErrAlreadyExists
		}
	}
	cp := *p
	f.m[p.ID] = &cp
	return nil
}

// samePeriod reports whether two period dates collide in the unique index.
// Two NULLs never collide — that is SQL's rule and the reason a manual payout
// never blocks a scheduled one.
func samePeriod(a, b *time.Time) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
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

// isClaimed reports whether a ledger entry is already owned by a live payout.
func (f *fakeItems) isClaimed(entryID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.byEntry[entryID]
	return ok
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

// fakeOwed returns preconfigured balances, honouring the payout_items claim
// table so a second read after a successful generation sees the entries gone —
// which is what makes the roll-over and idempotency tests meaningful rather
// than a replay of a static fixture.
type fakeOwed struct {
	byRestaurant map[uuid.UUID][]domain.OwedBalance
	ids          []uuid.UUID
	// claims, when set, is consulted to drop already-claimed ledger entries.
	claims *fakeItems
	// entryTimes, when set, gives each ledger entry a creation instant so the
	// venue-local day boundary can actually be exercised. An entry with no time
	// is treated as "long ago" (always in scope).
	entryTimes map[uuid.UUID]time.Time
	// upToCalls records the cutoffs the caller asked for, per restaurant.
	upToCalls map[uuid.UUID][]time.Time
	mu        sync.Mutex
}

func (f *fakeOwed) OwedForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.OwedBalance, error) {
	return f.OwedForRestaurantUpTo(ctx, restaurantID, time.Time{})
}

func (f *fakeOwed) OwedForRestaurantUpTo(_ context.Context, restaurantID uuid.UUID, before time.Time) ([]domain.OwedBalance, error) {
	f.mu.Lock()
	if f.upToCalls == nil {
		f.upToCalls = map[uuid.UUID][]time.Time{}
	}
	f.upToCalls[restaurantID] = append(f.upToCalls[restaurantID], before)
	f.mu.Unlock()

	var out []domain.OwedBalance
	for _, bal := range f.byRestaurant[restaurantID] {
		filtered := domain.OwedBalance{RestaurantID: bal.RestaurantID, Currency: bal.Currency}
		for _, e := range bal.Entries {
			if f.claims != nil && f.claims.isClaimed(e.LedgerEntryID) {
				continue
			}
			if !before.IsZero() {
				if at, ok := f.entryTimes[e.LedgerEntryID]; ok && !at.Before(before) {
					continue
				}
			}
			filtered.AmountMinor += e.AmountSignedMinor
			filtered.Entries = append(filtered.Entries, e)
		}
		if filtered.AmountMinor > 0 {
			out = append(out, filtered)
		}
	}
	return out, nil
}

func (f *fakeOwed) OwedRestaurantIDs(_ context.Context) ([]uuid.UUID, error) { return f.ids, nil }

// fakeSettings is an in-memory domain.PayoutSettingsRepository. It reproduces
// the two properties the usecase depends on: a venue with no row is ABSENT
// (not a zero value), and ForRestaurants answers in one call.
type fakeSettings struct {
	mu sync.Mutex
	m  map[uuid.UUID]domain.PayoutSettings
	// batchCalls counts ForRestaurants calls so a test can assert the daily
	// pass does not do one lookup per venue.
	batchCalls int
	// err, when set, fails every read — the "policy unreadable" path.
	err error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{m: map[uuid.UUID]domain.PayoutSettings{}}
}

func (f *fakeSettings) Get(_ context.Context, restaurantID uuid.UUID) (*domain.PayoutSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if s, ok := f.m[restaurantID]; ok {
		cp := s
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeSettings) Upsert(_ context.Context, s *domain.PayoutSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	f.m[s.RestaurantID] = *s
	return nil
}

func (f *fakeSettings) ForRestaurants(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.PayoutSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uuid.UUID]domain.PayoutSettings, len(ids))
	for _, id := range ids {
		if s, ok := f.m[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

// setVenuePolicy gives one venue its own overrides. A nil pointer means "this
// venue keeps following the platform for that knob".
func (h *harness) setVenuePolicy(rid uuid.UUID, minMinor *int64, maxHoldDays *int) {
	h.settings.m[rid] = domain.PayoutSettings{
		RestaurantID: rid, MinPayoutMinor: minMinor, MaxHoldDays: maxHoldDays,
	}
}

func ptrInt64(v int64) *int64 { return &v }
func ptrInt(v int) *int       { return &v }

// fakeVenues is an in-memory domain.PayoutVenueReader.
type fakeVenues struct{ tz map[uuid.UUID]string }

func (f *fakeVenues) TimezonesFor(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if z, ok := f.tz[id]; ok && z != "" {
			out[id] = z
		}
	}
	return out, nil
}

// fakeLedger is an append-only payout ledger enforcing
// UNIQUE(payout_id, account, direction, entry_type).
type fakeLedger struct {
	mu       sync.Mutex
	byPayout map[uuid.UUID][]domain.PayoutLedgerEntry
	seen     map[string]struct{}
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{byPayout: map[uuid.UUID][]domain.PayoutLedgerEntry{}, seen: map[string]struct{}{}}
}

func (f *fakeLedger) CreateBatch(_ context.Context, entries []domain.PayoutLedgerEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		k := e.PayoutID.String() + "|" + string(e.Account) + "|" + string(e.Direction) + "|" + string(e.EntryType)
		if _, dup := f.seen[k]; dup {
			return domain.ErrAlreadyExists
		}
		keys = append(keys, k)
	}
	for _, k := range keys {
		f.seen[k] = struct{}{}
	}
	for _, e := range entries {
		f.byPayout[e.PayoutID] = append(f.byPayout[e.PayoutID], e)
	}
	return nil
}

func (f *fakeLedger) ListByPayout(_ context.Context, payoutID uuid.UUID) ([]domain.PayoutLedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.PayoutLedgerEntry(nil), f.byPayout[payoutID]...), nil
}

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
	uc       *UseCase
	perms    *fakePerms
	dest     *fakeDestinations
	payouts  *fakePayouts
	items    *fakeItems
	owed     *fakeOwed
	venues   *fakeVenues
	settings *fakeSettings
	ledger   *fakeLedger
	gw       *fakeGateway
}

func newHarness() *harness { return newHarnessWithConfig(Config{}) }

// newHarnessWithConfig builds the usecase over a specific money policy. A zero
// Config gets the FreedomPay tariff defaults (190 bps / 300 ₸ floor, platform
// bears), same as production.
func newHarnessWithConfig(cfg Config) *harness {
	perms := &fakePerms{allow: true}
	dest := newFakeDestinations()
	pays := newFakePayouts()
	items := newFakeItems()
	owed := &fakeOwed{byRestaurant: map[uuid.UUID][]domain.OwedBalance{}, claims: items}
	venues := &fakeVenues{tz: map[uuid.UUID]string{}}
	settings := newFakeSettings()
	ledger := newFakeLedger()
	gw := &fakeGateway{}
	uc := NewUseCase(Ports{
		Perms:        perms,
		Destinations: dest,
		Payouts:      pays,
		Items:        items,
		Owed:         owed,
		Venues:       venues,
		Settings:     settings,
		Ledger:       ledger,
		Gateway:      gw,
		Tx:           fakeTx{},
	}, cfg, nil)
	return &harness{uc: uc, perms: perms, dest: dest, payouts: pays, items: items,
		owed: owed, venues: venues, settings: settings, ledger: ledger, gw: gw}
}

// daily builds the scheduled pass over the same fakes, with a frozen clock so a
// test can place "now" on either side of a venue's local midnight.
func (h *harness) daily(cfg DailyConfig, now time.Time) *DailyRunner {
	d := NewDailyRunner(h.uc, h.venues, cfg, nil)
	d.now = func() time.Time { return now }
	h.uc.now = func() time.Time { return now }
	return d
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func staff() Actor      { return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant} }

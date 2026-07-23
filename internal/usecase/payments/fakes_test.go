package payments

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Hand-written fakes (project convention: no mock framework). fakePaymentRepo,
// fakeLedgerRepo, fakeOutboxRepo and fakeRefundRepo are guarded by a mutex so
// concurrency tests can race real goroutines against them, and each supports
// snapshot/restore so fakeTx can give WithinTx genuine rollback-on-error
// semantics — a Postgres transaction would give us that for free, but a
// hand-written fake has to earn it.

// ---------------------------------------------------------------------------
// payments
// ---------------------------------------------------------------------------

type fakePaymentRepo struct {
	mu   sync.Mutex
	byID map[uuid.UUID]*domain.Payment
}

func newFakePaymentRepo(ps ...*domain.Payment) *fakePaymentRepo {
	f := &fakePaymentRepo{byID: map[uuid.UUID]*domain.Payment{}}
	for _, p := range ps {
		cp := *p
		if cp.StatusChangedAt.IsZero() {
			cp.StatusChangedAt = cp.CreatedAt
		}
		f.byID[p.ID] = &cp
	}
	return f
}

func (f *fakePaymentRepo) snapshot() map[uuid.UUID]*domain.Payment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uuid.UUID]*domain.Payment, len(f.byID))
	for id, p := range f.byID {
		cp := *p
		out[id] = &cp
	}
	return out
}

func (f *fakePaymentRepo) restore(snap map[uuid.UUID]*domain.Payment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID = snap
}

func (f *fakePaymentRepo) Create(_ context.Context, p *domain.Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.Provider == p.Provider && existing.IdempotencyKey == p.IdempotencyKey {
			return domain.ErrAlreadyExists // idx_payments_idempotency
		}
	}
	cp := *p
	f.byID[p.ID] = &cp
	return nil
}

func (f *fakePaymentRepo) Update(_ context.Context, p *domain.Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[p.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *p
	f.byID[p.ID] = &cp
	return nil
}

func (f *fakePaymentRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakePaymentRepo) GetByProviderPaymentID(_ context.Context, provider domain.PaymentProvider, providerPaymentID string) (*domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.Provider == provider && p.ProviderPaymentID != nil && *p.ProviderPaymentID == providerPaymentID {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakePaymentRepo) GetLiveByBookingID(_ context.Context, bookingID uuid.UUID) (*domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.BookingID == bookingID && p.Status.HoldsMoney() {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakePaymentRepo) GetSettleableByBookingID(_ context.Context, bookingID uuid.UUID) (*domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.BookingID == bookingID && p.Status.SettleResolvable() {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakePaymentRepo) GetByIdempotencyKey(_ context.Context, provider domain.PaymentProvider, key string) (*domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.Provider == provider && p.IdempotencyKey == key {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakePaymentRepo) List(context.Context, domain.PaymentFilter) ([]domain.Payment, int, error) {
	return nil, 0, nil
}

func (f *fakePaymentRepo) UpdateStatus(_ context.Context, id uuid.UUID, status domain.PaymentStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	p.Status = status
	stampStatusTime(p, status, at)
	return nil
}

// CompareAndSwapStatus mimics `UPDATE payments SET status=$to ... WHERE
// id=$id AND status=$from`, including the idx_payments_live_per_booking
// partial unique index: a transition INTO authorized/captured fails when
// another payment for the same booking already holds that ground.
func (f *fakePaymentRepo) CompareAndSwapStatus(_ context.Context, id uuid.UUID, from, to domain.PaymentStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if p.Status != from {
		return domain.ErrAlreadyExists
	}
	if to.HoldsMoney() {
		for otherID, other := range f.byID {
			if otherID == id {
				continue
			}
			if other.BookingID == p.BookingID && other.Status.HoldsMoney() {
				return domain.ErrAlreadyExists
			}
		}
	}
	p.Status = to
	stampStatusTime(p, to, at)
	return nil
}

func stampStatusTime(p *domain.Payment, status domain.PaymentStatus, at time.Time) {
	switch status {
	case domain.PaymentAuthorized:
		p.AuthorizedAt = &at
	case domain.PaymentCaptured:
		p.CapturedAt = &at
	case domain.PaymentVoided:
		p.VoidedAt = &at
	case domain.PaymentFailed:
		p.FailedAt = &at
	}
	// A real status transition resets the reconciliation lease clock and
	// attempt counter (migration 0010): a payment that moved on its own no
	// longer needs the stale-attempt bookkeeping that was building up while
	// it was stuck.
	p.StatusChangedAt = at
	p.ReconcileAttempts = 0
	p.LastReconcileAttemptAt = nil
	p.NeedsManualReview = false
}

// ClaimStale mimics `SELECT ... WHERE status = ANY($statuses) AND
// status_changed_at < $before ORDER BY status_changed_at LIMIT $limit`.
func (f *fakePaymentRepo) ClaimStale(_ context.Context, statuses []domain.PaymentStatus, before time.Time, limit int) ([]domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[domain.PaymentStatus]struct{}{}
	for _, s := range statuses {
		want[s] = struct{}{}
	}
	var out []domain.Payment
	for _, p := range f.byID {
		if _, ok := want[p.Status]; !ok {
			continue
		}
		if !p.StatusChangedAt.Before(before) {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StatusChangedAt.Before(out[j].StatusChangedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ClaimExpiredHolds mimics `SELECT ... WHERE status = 'authorized' AND
// expires_at IS NOT NULL AND expires_at < $before ORDER BY expires_at LIMIT
// $limit` (idx_payments_expires).
func (f *fakePaymentRepo) ClaimExpiredHolds(_ context.Context, before time.Time, limit int) ([]domain.Payment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Payment
	for _, p := range f.byID {
		if p.Status != domain.PaymentAuthorized || p.ExpiresAt == nil {
			continue
		}
		if !p.ExpiresAt.Before(before) {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt.Before(*out[j].ExpiresAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordReconcileAttempt mimics the CAS-guarded
// `UPDATE payments SET reconcile_attempts = reconcile_attempts + 1, ...
// WHERE id = $id AND status = $expectedStatus`.
func (f *fakePaymentRepo) RecordReconcileAttempt(_ context.Context, id uuid.UUID, expectedStatus domain.PaymentStatus, at time.Time, maxAttempts int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[id]
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

// ClaimSettlement mimics `UPDATE payments SET settled_at=$at,
// settled_trigger=$trigger, settlement_idempotency_key=$key WHERE id=$id AND
// settled_at IS NULL`.
func (f *fakePaymentRepo) ClaimSettlement(_ context.Context, id uuid.UUID, idempotencyKey string, trigger domain.RefundTrigger, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if p.SettledAt != nil {
		return domain.ErrAlreadyExists
	}
	p.SettledAt = &at
	trig := trigger
	p.SettledTrigger = &trig
	key := idempotencyKey
	p.SettlementIdempotencyKey = &key
	return nil
}

// ---------------------------------------------------------------------------
// ledger
// ---------------------------------------------------------------------------

type fakeLedgerRepo struct {
	mu      sync.Mutex
	entries []domain.PaymentLedgerEntry
	// createBatchErr, when set, is returned ONCE by CreateBatch and then
	// cleared — used to simulate "the local commit failed after the acquirer
	// already moved the money" without permanently wedging a test.
	createBatchErr error
}

func newFakeLedgerRepo() *fakeLedgerRepo { return &fakeLedgerRepo{} }

func (f *fakeLedgerRepo) snapshot() []domain.PaymentLedgerEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PaymentLedgerEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

func (f *fakeLedgerRepo) restore(snap []domain.PaymentLedgerEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = snap
}

func (f *fakeLedgerRepo) CreateBatch(_ context.Context, entries []domain.PaymentLedgerEntry) error {
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return err
	}
	f.mu.Lock()
	if f.createBatchErr != nil {
		err := f.createBatchErr
		f.createBatchErr = nil
		f.mu.Unlock()
		return err
	}
	defer f.mu.Unlock()
	f.entries = append(f.entries, entries...)
	return nil
}

func (f *fakeLedgerRepo) ListByPaymentID(_ context.Context, paymentID uuid.UUID) ([]domain.PaymentLedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.PaymentLedgerEntry
	for _, e := range f.entries {
		if e.PaymentID == paymentID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeLedgerRepo) BalanceByAccount(_ context.Context, paymentID uuid.UUID) (map[domain.LedgerAccount]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.LedgerAccount]int64{}
	for _, e := range f.entries {
		if e.PaymentID != paymentID {
			continue
		}
		if e.Direction == domain.DirectionDebit {
			out[e.Account] += e.AmountMinor
		} else {
			out[e.Account] -= e.AmountMinor
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// outbox
// ---------------------------------------------------------------------------

type fakePaymentOutbox struct {
	mu      sync.Mutex
	created []domain.PaymentOutboxEvent
}

func newFakePaymentOutbox() *fakePaymentOutbox { return &fakePaymentOutbox{} }

func (f *fakePaymentOutbox) snapshot() []domain.PaymentOutboxEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PaymentOutboxEvent, len(f.created))
	copy(out, f.created)
	return out
}

func (f *fakePaymentOutbox) restore(snap []domain.PaymentOutboxEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = snap
}

func (f *fakePaymentOutbox) Create(_ context.Context, e *domain.PaymentOutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, *e)
	return nil
}
func (f *fakePaymentOutbox) ClaimUnpublished(context.Context, int) ([]domain.PaymentOutboxEvent, error) {
	return nil, nil
}
func (f *fakePaymentOutbox) ExistsForPayment(_ context.Context, paymentID uuid.UUID, t domain.PaymentOutboxEventType) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.created {
		if e.PaymentID == paymentID && e.EventType == t {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakePaymentOutbox) MarkPublished(context.Context, []uuid.UUID, time.Time) error { return nil }

func (f *fakePaymentOutbox) types() []domain.PaymentOutboxEventType {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PaymentOutboxEventType, 0, len(f.created))
	for _, e := range f.created {
		out = append(out, e.EventType)
	}
	return out
}

// ---------------------------------------------------------------------------
// payment events (webhooks)
// ---------------------------------------------------------------------------

type fakeEventRepo struct {
	mu   sync.Mutex
	byID map[string]*domain.PaymentEvent // keyed by provider+event id
}

func newFakeEventRepo() *fakeEventRepo {
	return &fakeEventRepo{byID: map[string]*domain.PaymentEvent{}}
}

func eventKey(provider domain.PaymentProvider, eventID string) string {
	return string(provider) + ":" + eventID
}

func (f *fakeEventRepo) Create(_ context.Context, e *domain.PaymentEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := eventKey(e.Provider, e.ProviderEventID)
	if _, ok := f.byID[k]; ok {
		return domain.ErrAlreadyExists
	}
	cp := *e
	f.byID[k] = &cp
	return nil
}

func (f *fakeEventRepo) GetByProviderEventID(_ context.Context, provider domain.PaymentProvider, providerEventID string) (*domain.PaymentEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.byID[eventKey(provider, providerEventID)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (f *fakeEventRepo) ClaimUnprocessed(context.Context, int) ([]domain.PaymentEvent, error) {
	return nil, nil
}

func (f *fakeEventRepo) MarkProcessed(_ context.Context, id uuid.UUID, at time.Time, processErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.ID == id {
			e.ProcessedAt = &at
			if processErr != "" {
				e.ProcessError = &processErr
			}
			return nil
		}
	}
	return domain.ErrNotFound
}

// RecordProcessingError stores why an apply attempt failed WITHOUT setting
// ProcessedAt — mirrors domain.PaymentEventRepository.RecordProcessingError
// (report item #9): the event must stay in the "unprocessed" set.
func (f *fakeEventRepo) RecordProcessingError(_ context.Context, id uuid.UUID, processErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.ID == id {
			e.ProcessError = &processErr
			return nil
		}
	}
	return domain.ErrNotFound
}

// SetPaymentID backfills PaymentID once resolved (report item #16, minor).
func (f *fakeEventRepo) SetPaymentID(_ context.Context, id uuid.UUID, paymentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.ID == id {
			e.PaymentID = &paymentID
			return nil
		}
	}
	return domain.ErrNotFound
}

// ---------------------------------------------------------------------------
// refunds
// ---------------------------------------------------------------------------

type fakeRefundRepo struct {
	mu   sync.Mutex
	byID map[uuid.UUID]*domain.PaymentRefund
}

func newFakeRefundRepo() *fakeRefundRepo {
	return &fakeRefundRepo{byID: map[uuid.UUID]*domain.PaymentRefund{}}
}

func (f *fakeRefundRepo) snapshot() map[uuid.UUID]*domain.PaymentRefund {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uuid.UUID]*domain.PaymentRefund, len(f.byID))
	for id, r := range f.byID {
		cp := *r
		out[id] = &cp
	}
	return out
}

func (f *fakeRefundRepo) restore(snap map[uuid.UUID]*domain.PaymentRefund) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID = snap
}

func (f *fakeRefundRepo) Create(_ context.Context, r *domain.PaymentRefund) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if existing.PaymentID == r.PaymentID && existing.IdempotencyKey == r.IdempotencyKey {
			return domain.ErrAlreadyExists
		}
	}
	cp := *r
	if cp.StatusChangedAt.IsZero() {
		cp.StatusChangedAt = cp.CreatedAt
	}
	f.byID[r.ID] = &cp
	return nil
}

func (f *fakeRefundRepo) Update(_ context.Context, r *domain.PaymentRefund) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[r.ID]; !ok {
		return domain.ErrNotFound
	}
	cp := *r
	f.byID[r.ID] = &cp
	return nil
}

func (f *fakeRefundRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.PaymentRefund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRefundRepo) GetByIdempotencyKey(_ context.Context, paymentID uuid.UUID, key string) (*domain.PaymentRefund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.byID {
		if r.PaymentID == paymentID && r.IdempotencyKey == key {
			cp := *r
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f *fakeRefundRepo) ListByPaymentID(_ context.Context, paymentID uuid.UUID) ([]domain.PaymentRefund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.PaymentRefund
	for _, r := range f.byID {
		if r.PaymentID == paymentID {
			out = append(out, *r)
		}
	}
	return out, nil
}

// CompareAndSwapStatus mimics `UPDATE payment_refunds SET status=$to,
// updated_at=$at WHERE id=$id AND status=$from`.
func (f *fakeRefundRepo) CompareAndSwapStatus(_ context.Context, id uuid.UUID, from, to domain.RefundStatus, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	if r.Status != from {
		return domain.ErrAlreadyExists
	}
	r.Status = to
	r.UpdatedAt = at
	// Same reset as payments' stampStatusTime: a real transition clears the
	// stale-attempt bookkeeping.
	r.StatusChangedAt = at
	r.ReconcileAttempts = 0
	r.LastReconcileAttemptAt = nil
	r.NeedsManualReview = false
	return nil
}

// ClaimStale mimics `SELECT ... WHERE status = ANY($statuses) AND
// status_changed_at < $before ORDER BY status_changed_at LIMIT $limit`.
func (f *fakeRefundRepo) ClaimStale(_ context.Context, statuses []domain.RefundStatus, before time.Time, limit int) ([]domain.PaymentRefund, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[domain.RefundStatus]struct{}{}
	for _, s := range statuses {
		want[s] = struct{}{}
	}
	var out []domain.PaymentRefund
	for _, r := range f.byID {
		if _, ok := want[r.Status]; !ok {
			continue
		}
		if !r.StatusChangedAt.Before(before) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StatusChangedAt.Before(out[j].StatusChangedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordReconcileAttempt mirrors fakePaymentRepo.RecordReconcileAttempt.
func (f *fakeRefundRepo) RecordReconcileAttempt(_ context.Context, id uuid.UUID, expectedStatus domain.RefundStatus, at time.Time, maxAttempts int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return 0, false, domain.ErrNotFound
	}
	if r.Status != expectedStatus {
		return 0, false, domain.ErrAlreadyExists
	}
	r.ReconcileAttempts++
	r.LastReconcileAttemptAt = &at
	r.NeedsManualReview = r.ReconcileAttempts >= maxAttempts
	return r.ReconcileAttempts, r.NeedsManualReview, nil
}

func (f *fakeRefundRepo) SucceededTotal(_ context.Context, paymentID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int64
	for _, r := range f.byID {
		if r.PaymentID == paymentID && r.Status == domain.RefundSucceeded {
			total += r.AmountMinor
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// booking + item readers, restaurant settings
// ---------------------------------------------------------------------------

type fakeBookingReader struct {
	byID map[uuid.UUID]*domain.Booking
	err  error
}

func newFakeBookingReader(bs ...*domain.Booking) *fakeBookingReader {
	f := &fakeBookingReader{byID: map[uuid.UUID]*domain.Booking{}}
	for _, b := range bs {
		f.byID[b.ID] = b
	}
	return f
}

func (f *fakeBookingReader) GetByID(_ context.Context, id uuid.UUID) (*domain.Booking, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

type fakeItemReader struct {
	byBooking map[uuid.UUID][]domain.BookingItem
}

func newFakeItemReader() *fakeItemReader {
	return &fakeItemReader{byBooking: map[uuid.UUID][]domain.BookingItem{}}
}

func (f *fakeItemReader) ListByBooking(_ context.Context, bookingID uuid.UUID) ([]domain.BookingItem, error) {
	return f.byBooking[bookingID], nil
}

type fakeRestaurantSettings struct {
	byRestaurant map[uuid.UUID]domain.PaymentSettingsOverride
}

func newFakeRestaurantSettings() *fakeRestaurantSettings {
	return &fakeRestaurantSettings{byRestaurant: map[uuid.UUID]domain.PaymentSettingsOverride{}}
}

func (f *fakeRestaurantSettings) GetPaymentOverride(_ context.Context, restaurantID uuid.UUID) (domain.PaymentSettingsOverride, error) {
	return f.byRestaurant[restaurantID], nil
}

// ---------------------------------------------------------------------------
// manager checker (tenant scoping, report item #13) + cancel deadline
// ---------------------------------------------------------------------------

// fakeManagerChecker is a hand-written managerChecker: by default every user
// manages every restaurant (so existing tests that never cared about tenant
// scoping keep passing unchanged); tests that DO care register a narrower
// truth via managed.
type fakeManagerChecker struct {
	mu      sync.Mutex
	managed map[uuid.UUID]map[uuid.UUID]bool // userID -> restaurantID -> manages
	// allowAllByDefault mirrors "staff of any single venue" test setups that
	// never register anything: true means every (user, restaurant) pair
	// manages, matching the pre-item-#13 behaviour for tests that are not
	// specifically about tenant scoping.
	allowAllByDefault bool
}

func newFakeManagerChecker() *fakeManagerChecker {
	return &fakeManagerChecker{managed: map[uuid.UUID]map[uuid.UUID]bool{}, allowAllByDefault: true}
}

func (f *fakeManagerChecker) set(userID, restaurantID uuid.UUID, manages bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.managed[userID] == nil {
		f.managed[userID] = map[uuid.UUID]bool{}
	}
	f.managed[userID][restaurantID] = manages
}

func (f *fakeManagerChecker) Manages(_ context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if byRestaurant, ok := f.managed[userID]; ok {
		if v, ok := byRestaurant[restaurantID]; ok {
			return v, nil
		}
	}
	return f.allowAllByDefault, nil
}

// fakeCancelDeadlineResolver is a hand-written cancelDeadlineResolver: it
// always answers with a fixed deadline, set by the test, regardless of the
// booking passed in — real resolution (policy + starts_at) lives in
// usecase/bookings and is out of scope for these unit tests.
type fakeCancelDeadlineResolver struct {
	deadline time.Time
	err      error
}

func (f *fakeCancelDeadlineResolver) CancelDeadlineFor(context.Context, domain.Booking) (time.Time, error) {
	if f.err != nil {
		return time.Time{}, f.err
	}
	return f.deadline, nil
}

// ---------------------------------------------------------------------------
// gateway + resolver
// ---------------------------------------------------------------------------

// fakeGateway is a hand-written domain.PaymentGateway, same convention as
// infrastructure/payment/registry_test.go's fakeGateway, extended with error
// hooks and call counters so failure paths (timeout, provider rejection) and
// idempotent-retry / no-second-call assertions are exercisable.
type fakeGateway struct {
	mu sync.Mutex

	name domain.PaymentProvider

	// authorizeDelay forces concurrent callers to actually overlap inside
	// Authorize instead of one goroutine racing to completion before the next
	// one even starts — a real acquirer's network round-trip does this for
	// free, a fake with an instant return does not.
	authorizeDelay time.Duration

	authorizeErr  error
	authorizeResp *domain.GatewayPayment
	authorizeN    int

	// captureDelay forces concurrent CaptureOnSeating callers to actually
	// overlap, same reasoning as authorizeDelay.
	captureDelay time.Duration
	captureErr   error
	captureResp  *domain.GatewayPayment
	captureN     int

	// voidDelay forces concurrent VoidOnRejection callers to actually
	// overlap, same reasoning as captureDelay.
	voidDelay time.Duration
	voidErr   error
	voidN     int
	voided    []string

	refundErr  error
	refundResp *domain.GatewayRefund
	refundN    int

	// getDelay/getErr/getResp/getN drive Get(), the reconciliation read
	// (usecase/payments.Reconciler) — same convention as capture/void/refund
	// above. getFn, when set, overrides getResp/getErr entirely so a test can
	// answer differently call by call (e.g. "still unknown" then "resolved").
	getDelay time.Duration
	getErr   error
	getResp  *domain.GatewayPayment
	getN     int
	getFn    func(providerPaymentID string, callN int) (*domain.GatewayPayment, error)

	verifyFn func([]byte, map[string]string) (*domain.WebhookEvent, error)
}

func newFakeGateway(name domain.PaymentProvider) *fakeGateway { return &fakeGateway{name: name} }

func (f *fakeGateway) Authorize(_ context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	if f.authorizeDelay > 0 {
		time.Sleep(f.authorizeDelay)
	}
	f.mu.Lock()
	f.authorizeN++
	f.mu.Unlock()
	if f.authorizeErr != nil {
		return nil, f.authorizeErr
	}
	if f.authorizeResp != nil {
		return f.authorizeResp, nil
	}
	return &domain.GatewayPayment{
		ProviderPaymentID: "gw-" + req.PaymentID.String(),
		Status:            domain.PaymentCreated,
		Amount:            req.Amount,
		PaymentURL:        "https://pay.example/" + req.PaymentID.String(),
	}, nil
}

func (f *fakeGateway) Capture(_ context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayPayment, error) {
	if f.captureDelay > 0 {
		time.Sleep(f.captureDelay)
	}
	f.mu.Lock()
	f.captureN++
	f.mu.Unlock()
	if f.captureErr != nil {
		return nil, f.captureErr
	}
	if f.captureResp != nil {
		return f.captureResp, nil
	}
	return &domain.GatewayPayment{ProviderPaymentID: providerPaymentID, Status: domain.PaymentCaptured, Amount: amount}, nil
}

func (f *fakeGateway) Void(_ context.Context, providerPaymentID string) error {
	if f.voidDelay > 0 {
		time.Sleep(f.voidDelay)
	}
	f.mu.Lock()
	f.voidN++
	f.voided = append(f.voided, providerPaymentID)
	f.mu.Unlock()
	return f.voidErr
}

func (f *fakeGateway) Refund(_ context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayRefund, error) {
	f.mu.Lock()
	f.refundN++
	f.mu.Unlock()
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	if f.refundResp != nil {
		return f.refundResp, nil
	}
	return &domain.GatewayRefund{ProviderRefundID: "rf-" + providerPaymentID, Status: domain.RefundSucceeded, Amount: amount}, nil
}

func (f *fakeGateway) Get(_ context.Context, providerPaymentID string) (*domain.GatewayPayment, error) {
	if f.getDelay > 0 {
		time.Sleep(f.getDelay)
	}
	f.mu.Lock()
	f.getN++
	n := f.getN
	f.mu.Unlock()
	if f.getFn != nil {
		return f.getFn(providerPaymentID, n)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getResp != nil {
		return f.getResp, nil
	}
	return &domain.GatewayPayment{ProviderPaymentID: providerPaymentID}, nil
}

func (f *fakeGateway) VerifyWebhook(raw []byte, headers map[string]string) (*domain.WebhookEvent, error) {
	if f.verifyFn != nil {
		return f.verifyFn(raw, headers)
	}
	return nil, errors.New("fakeGateway: no verifyFn configured")
}

func (f *fakeGateway) Name() domain.PaymentProvider { return f.name }

func (f *fakeGateway) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op {
	case "authorize":
		return f.authorizeN
	case "capture":
		return f.captureN
	case "void":
		return f.voidN
	case "refund":
		return f.refundN
	case "get":
		return f.getN
	}
	return 0
}

// fakeGatewayResolver implements gatewayResolver over a fixed set of
// gateways, with an optional error for "no enabled provider" style failures.
type fakeGatewayResolver struct {
	byProvider map[domain.PaymentProvider]domain.PaymentGateway
	resolveErr error
}

func newFakeGatewayResolver(gws ...*fakeGateway) *fakeGatewayResolver {
	m := map[domain.PaymentProvider]domain.PaymentGateway{}
	for _, g := range gws {
		m[g.Name()] = g
	}
	return &fakeGatewayResolver{byProvider: m}
}

func (f *fakeGatewayResolver) Resolve(_ context.Context, preferred domain.PaymentProvider) (domain.PaymentGateway, error) {
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if g, ok := f.byProvider[preferred]; ok {
		return g, nil
	}
	for _, g := range f.byProvider {
		return g, nil // any configured gateway stands in for "the default"
	}
	return nil, errors.New("fakeGatewayResolver: no gateway configured")
}

func (f *fakeGatewayResolver) ForRefund(provider domain.PaymentProvider) (domain.PaymentGateway, error) {
	if g, ok := f.byProvider[provider]; ok {
		return g, nil
	}
	return nil, errors.New("fakeGatewayResolver: unknown provider " + string(provider))
}

// ---------------------------------------------------------------------------
// transaction manager
// ---------------------------------------------------------------------------

// fakeTx gives WithinTx genuine rollback-on-error semantics over the fakes
// above by snapshotting before fn runs and restoring on a non-nil error —
// the property the hard rule "transaction boundaries are explicit" depends
// on for the "rollback on mid-transaction failure" test.
type fakeTx struct {
	mu       sync.Mutex
	payments *fakePaymentRepo
	ledger   *fakeLedgerRepo
	outbox   *fakePaymentOutbox
	refunds  *fakeRefundRepo
}

func (f *fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	// Serializes concurrent transactions exactly like Postgres row/constraint
	// locking would for the narrow set of rows these tests touch — without
	// this a "concurrent" test would just interleave two goroutines' snapshot
	// and restore calls and prove nothing.
	f.mu.Lock()
	defer f.mu.Unlock()

	var paySnap map[uuid.UUID]*domain.Payment
	var ledgerSnap []domain.PaymentLedgerEntry
	var outboxSnap []domain.PaymentOutboxEvent
	var refundSnap map[uuid.UUID]*domain.PaymentRefund
	if f.payments != nil {
		paySnap = f.payments.snapshot()
	}
	if f.ledger != nil {
		ledgerSnap = f.ledger.snapshot()
	}
	if f.outbox != nil {
		outboxSnap = f.outbox.snapshot()
	}
	if f.refunds != nil {
		refundSnap = f.refunds.snapshot()
	}

	err := fn(ctx)
	if err != nil {
		if f.payments != nil {
			f.payments.restore(paySnap)
		}
		if f.ledger != nil {
			f.ledger.restore(ledgerSnap)
		}
		if f.outbox != nil {
			f.outbox.restore(outboxSnap)
		}
		if f.refunds != nil {
			f.refunds.restore(refundSnap)
		}
	}
	return err
}

func (f *fakeTx) Detach(ctx context.Context) context.Context { return ctx }

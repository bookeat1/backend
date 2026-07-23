package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// Reconciler is the background payments janitor the review flagged as a
// safety requirement, not a nice-to-have: three transient states have no
// automatic way out, and money may already have moved at the acquirer while
// the local row sits frozen forever.
//
//   - PaymentCapturing / PaymentVoiding: a process died between winning the
//     local CAS claim (capture.go's CaptureOnSeating / VoidOnRejection) and
//     the acquirer answering. The claim survives; nothing ever resolves it.
//   - RefundInFlight / RefundPending: the same for a refund (refund.go's
//     claimAndCallGateway), plus the case where the acquirer itself answered
//     with a timeout/5xx/malformed body — genuinely unknown, not a failure.
//   - PaymentCreated / PaymentAuthorized gone stale with a provider payment id
//     already assigned: the acquirer may have moved the payment on (captured,
//     voided, expired) while our webhook for that event was lost, dropped, or
//     never sent.
//   - PaymentAuthorized past its own ExpiresAt: our hold TTL lapsed locally,
//     independent of whether the acquirer's own TTL has (spec §5).
//
// The acquirer's own PaymentGateway.Get is the only source of truth for all
// four; this worker's whole job is to ask it and apply EXACTLY the same
// transitions and ledger entries the normal paths already use
// (captureLedgerEntries / settlementLedgerEntries / publishPaymentEvent /
// webhookUseCase.apply), never a transition invented here. Every write is
// CAS-guarded, so a webhook and this worker racing on the same payment
// produce one result and one ledger batch, never two (see reconciler_test.go's
// race test).
//
// Money-safety over completeness: where this build cannot tell an acquirer's
// answer apart from "still unknown" (see resolveRefund's TODO(verify)), it
// leaves the row alone rather than guess. DO NOT run this in production
// before internal/infrastructure/postgres has real PaymentRepository /
// PaymentRefundRepository implementations of ClaimStale / ClaimExpiredHolds /
// RecordReconcileAttempt — only in-memory fakes exist as of this change.
type Reconciler struct {
	payments domain.PaymentRepository
	refunds  domain.PaymentRefundRepository
	ledger   domain.PaymentLedgerRepository
	outbox   domain.PaymentOutboxRepository
	gateways gatewayResolver
	tx       domain.TxManager
	cfg      ReconcilerConfig
	log      *slog.Logger
	now      func() time.Time // injectable clock for tests
	pace     *pacer
}

// ReconcilerConfig is the worker's own scheduling and safety configuration,
// same shape and env-driven convention as bookings.WorkerConfig.
type ReconcilerConfig struct {
	// TickInterval is the pause between two passes. env:
	// PAYMENTS_RECONCILE_TICK_INTERVAL
	TickInterval time.Duration
	// StuckAfter is how long a payment may sit in `capturing`/`voiding`, or a
	// refund in `in_flight`/`pending`, before it counts as stuck rather than
	// "a normal request in flight right now". env:
	// PAYMENTS_RECONCILE_STUCK_AFTER
	StuckAfter time.Duration
	// LostWebhookAfter is how long a `created`/`authorized` payment with a
	// provider payment id may go without a status change before the worker
	// asks the acquirer directly, in case its webhook was lost. Deliberately
	// much longer than StuckAfter: a payment sitting `authorized` for hours is
	// completely normal (the guest paid, the venue has not seated them yet);
	// it is not evidence of anything lost. env:
	// PAYMENTS_RECONCILE_LOST_WEBHOOK_AFTER
	LostWebhookAfter time.Duration
	// BatchSize caps how many rows one pass claims per stage.
	// env: PAYMENTS_RECONCILE_BATCH_SIZE
	BatchSize int
	// MaxAttempts is how many consecutive unresolved reconciliation attempts a
	// single row may accumulate before it is flagged NeedsManualReview and the
	// worker stops calling the acquirer for it. env:
	// PAYMENTS_RECONCILE_MAX_ATTEMPTS
	MaxAttempts int
	// ProviderMinGap is the minimum spacing between two acquirer calls made by
	// this worker (across the whole tick, not per row) — the avalanche guard:
	// a batch of hundreds of stuck rows must not turn into hundreds of
	// simultaneous requests against the acquirer. env:
	// PAYMENTS_RECONCILE_PROVIDER_MIN_GAP
	ProviderMinGap time.Duration
}

const (
	defaultReconcileTickInterval     = 2 * time.Minute
	defaultReconcileStuckAfter       = 10 * time.Minute
	defaultReconcileLostWebhookAfter = time.Hour
	defaultReconcileBatchSize        = 50
	defaultReconcileMaxAttempts      = 5
	defaultReconcileProviderMinGap   = 200 * time.Millisecond
)

func (c ReconcilerConfig) withDefaults() ReconcilerConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultReconcileTickInterval
	}
	if c.StuckAfter <= 0 {
		c.StuckAfter = defaultReconcileStuckAfter
	}
	if c.LostWebhookAfter <= 0 {
		c.LostWebhookAfter = defaultReconcileLostWebhookAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultReconcileBatchSize
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultReconcileMaxAttempts
	}
	if c.ProviderMinGap < 0 {
		c.ProviderMinGap = defaultReconcileProviderMinGap
	}
	return c
}

// NewReconciler constructs the background reconciliation worker.
func NewReconciler(
	paymentsRepo domain.PaymentRepository,
	refundsRepo domain.PaymentRefundRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	tx domain.TxManager,
	cfg ReconcilerConfig,
	log *slog.Logger,
) *Reconciler {
	cfg = cfg.withDefaults()
	return &Reconciler{
		payments: paymentsRepo, refunds: refundsRepo, ledger: ledger, outbox: outbox,
		gateways: gateways, tx: tx, cfg: cfg, log: log, now: time.Now,
		pace: &pacer{minGap: cfg.ProviderMinGap},
	}
}

// ReconcileResult counts what one pass did. This is what an alert is built on
// (spec's "отдельно логировать: сколько нашли зависших, сколько починили,
// сколько осталось непонятными"): zero values are the normal steady state.
type ReconcileResult struct {
	StuckFound   int // rows claimed as candidates this pass
	Resolved     int // reached a terminal-for-now state (captured/authorized/voided/refunded/failed/expired, or confirmed already-consistent)
	StillUnknown int // acquirer answer did not let us decide; attempt counter bumped
	ManualReview int // attempts exhausted this pass, or already flagged from a previous one
}

func (r ReconcileResult) attrs() []any {
	return []any{
		slog.Int("stuck_found", r.StuckFound), slog.Int("resolved", r.Resolved),
		slog.Int("still_unknown", r.StillUnknown), slog.Int("manual_review", r.ManualReview),
	}
}

// Run ticks until ctx is cancelled, same contract as bookings.Worker.Run: a
// failing pass is logged and retried on the next tick, never fatal.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	r.log.Info("payments reconciler started",
		slog.Duration("tick", r.cfg.TickInterval),
		slog.Duration("stuck_after", r.cfg.StuckAfter),
		slog.Duration("lost_webhook_after", r.cfg.LostWebhookAfter))
	for {
		select {
		case <-ctx.Done():
			r.log.Info("payments reconciler stopped")
			return nil
		case <-t.C:
			res, err := r.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				r.log.Error("payments reconciler tick failed", slog.String("error", err.Error()))
				continue
			}
			if res != (ReconcileResult{}) {
				r.log.Info(logging.EventPaymentReconcileTick, res.attrs()...)
			}
		}
	}
}

// Tick runs one pass over every stage. Exported so it can be driven directly
// from tests and from a one-shot invocation.
func (r *Reconciler) Tick(ctx context.Context) (ReconcileResult, error) {
	now := r.now()
	var res ReconcileResult
	if err := r.reconcileCapturing(ctx, now, &res); err != nil {
		return res, fmt.Errorf("capturing pass: %w", err)
	}
	if err := r.reconcileVoiding(ctx, now, &res); err != nil {
		return res, fmt.Errorf("voiding pass: %w", err)
	}
	if err := r.reconcileRefunds(ctx, now, &res); err != nil {
		return res, fmt.Errorf("refunds pass: %w", err)
	}
	if err := r.reconcileLostWebhook(ctx, now, &res); err != nil {
		return res, fmt.Errorf("lost webhook pass: %w", err)
	}
	if err := r.reconcileExpiredHolds(ctx, now, &res); err != nil {
		return res, fmt.Errorf("expired holds pass: %w", err)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// capturing / voiding
// ---------------------------------------------------------------------------

func (r *Reconciler) reconcileCapturing(ctx context.Context, now time.Time, res *ReconcileResult) error {
	due, err := r.payments.ClaimStale(ctx, []domain.PaymentStatus{domain.PaymentCapturing},
		now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range due {
		if err := r.processPayment(ctx, &due[i], now, res, true, r.resolveCapturing); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileVoiding(ctx context.Context, now time.Time, res *ReconcileResult) error {
	due, err := r.payments.ClaimStale(ctx, []domain.PaymentStatus{domain.PaymentVoiding},
		now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range due {
		if err := r.processPayment(ctx, &due[i], now, res, true, r.resolveVoiding); err != nil {
			return err
		}
	}
	return nil
}

// resolveCapturing answers a stuck `capturing` claim: captured → finish with
// the same ledger entries CaptureOnSeating would have written; still just
// authorized at the acquirer (the capture attempt never actually reached it,
// or was rejected before touching the hold) → release the claim back to
// `authorized`, the exact mirror of releaseCaptureClaim. Anything else is an
// anomaly logged distinctly and left alone: this build does not assume what a
// `voided`/`failed`/`expired` answer means for a hold that was mid-capture —
// see the type doc comment on money-safety over completeness.
func (r *Reconciler) resolveCapturing(ctx context.Context, _ domain.PaymentGateway, p *domain.Payment, resp *domain.GatewayPayment, now time.Time) (bool, string, error) {
	switch resp.Status {
	case domain.PaymentCaptured:
		if err := r.finishCapture(ctx, p, now); err != nil {
			return false, "", err
		}
		return true, "", nil
	case domain.PaymentAuthorized:
		if err := r.releaseTransient(ctx, p.ID, domain.PaymentCapturing, now); err != nil {
			return false, "", err
		}
		return true, "", nil
	default:
		return false, fmt.Sprintf(
			"capturing payment reported %q by acquirer, neither captured nor authorized — TODO(verify): confirm on sandbox whether this status can ever mean the capture is safely reversible",
			resp.Status), nil
	}
}

// resolveVoiding is symmetric to resolveCapturing.
func (r *Reconciler) resolveVoiding(ctx context.Context, _ domain.PaymentGateway, p *domain.Payment, resp *domain.GatewayPayment, now time.Time) (bool, string, error) {
	switch resp.Status {
	case domain.PaymentVoided:
		if err := r.finishVoid(ctx, p, now); err != nil {
			return false, "", err
		}
		return true, "", nil
	case domain.PaymentAuthorized:
		if err := r.releaseTransient(ctx, p.ID, domain.PaymentVoiding, now); err != nil {
			return false, "", err
		}
		return true, "", nil
	default:
		return false, fmt.Sprintf(
			"voiding payment reported %q by acquirer, neither voided nor authorized — TODO(verify): confirm on sandbox whether this status can ever mean the void is safely reversible",
			resp.Status), nil
	}
}

// finishCapture applies the exact transition and ledger batch
// CaptureOnSeating's own tail would have written (captureLedgerEntries, the
// same domain.EventPaymentCaptured outbox event). CAS-guarded: if a webhook
// already finished this exact capture between our Get() call and this write,
// the CAS loses with ErrAlreadyExists and that is treated as success, not a
// conflict — the same reasoning as CaptureOnSeating's own final CAS.
func (r *Reconciler) finishCapture(ctx context.Context, p *domain.Payment, at time.Time) error {
	entries := captureLedgerEntries(*p, at)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return err
	}
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCapturing, domain.PaymentCaptured, at); err != nil {
			return err
		}
		if err := r.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		return publishPaymentEvent(ctx, r.outbox, p, domain.EventPaymentCaptured, at)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrAlreadyExists) {
		current, rerr := r.payments.GetByID(ctx, p.ID)
		if rerr != nil {
			return rerr
		}
		if current.Status == domain.PaymentCaptured {
			return nil
		}
	}
	return err
}

// finishVoid mirrors finishCapture for the release path.
func (r *Reconciler) finishVoid(ctx context.Context, p *domain.Payment, at time.Time) error {
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentVoiding, domain.PaymentVoided, at); err != nil {
			return err
		}
		return publishPaymentEvent(ctx, r.outbox, p, domain.EventPaymentVoided, at)
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrAlreadyExists) {
		current, rerr := r.payments.GetByID(ctx, p.ID)
		if rerr != nil {
			return rerr
		}
		if current.Status == domain.PaymentVoided {
			return nil
		}
	}
	return err
}

// releaseTransient reverts a `capturing`/`voiding` claim back to `authorized`
// once the acquirer confirms the hold is still just that — the exact mirror
// of capture.go's releaseCaptureClaim/releaseVoidClaim. ErrAlreadyExists here
// means someone else already resolved it (a webhook, or a concurrent
// reconciler pass), which is success, not a conflict.
func (r *Reconciler) releaseTransient(ctx context.Context, id uuid.UUID, from domain.PaymentStatus, at time.Time) error {
	if err := r.payments.CompareAndSwapStatus(ctx, id, from, domain.PaymentAuthorized, at); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// refunds stuck in_flight / pending
// ---------------------------------------------------------------------------

func (r *Reconciler) reconcileRefunds(ctx context.Context, now time.Time, res *ReconcileResult) error {
	due, err := r.refunds.ClaimStale(ctx, []domain.RefundStatus{domain.RefundInFlight, domain.RefundPending},
		now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range due {
		if err := r.processRefund(ctx, &due[i], now, res); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) processRefund(ctx context.Context, rf *domain.PaymentRefund, now time.Time, res *ReconcileResult) error {
	if rf.NeedsManualReview {
		res.ManualReview++
		return nil
	}
	if rf.LastReconcileAttemptAt != nil && now.Before(rf.LastReconcileAttemptAt.Add(backoffDuration(r.cfg.StuckAfter, rf.ReconcileAttempts))) {
		return nil // still backing off, do not hammer the acquirer
	}
	res.StuckFound++

	p, err := r.payments.GetByID(ctx, rf.PaymentID)
	if err != nil {
		return err
	}
	if p.ProviderPaymentID == nil {
		r.log.Error("payment.reconcile_refund_no_provider_id",
			slog.String("refund_id", rf.ID.String()), slog.String("payment_id", p.ID.String()))
		return r.bumpRefundAttempt(ctx, rf, now, res)
	}
	gw, err := r.gateways.ForRefund(p.Provider)
	if err != nil {
		r.log.Warn("payment.reconcile_gateway_unavailable",
			slog.String("refund_id", rf.ID.String()), slog.String("error", err.Error()))
		return r.bumpRefundAttempt(ctx, rf, now, res)
	}

	r.pace.wait()
	// External call, deliberately outside any DB transaction. Payment-level
	// Get is the only signal available: neither adapter exposes a
	// refund-specific status read yet, but FreedomPay's own status_v2
	// response already reports PaymentRefunded/PaymentPartiallyRefunded when
	// pg_refund_amount is present (see freedompay/gateway.go's status()) —
	// which is exactly the fact this reconciliation needs.
	resp, err := gw.Get(ctx, *p.ProviderPaymentID)
	if err != nil {
		r.log.Warn(logging.EventPaymentReconcileUnknown,
			slog.String("refund_id", rf.ID.String()), slog.String("error", err.Error()))
		return r.bumpRefundAttempt(ctx, rf, now, res)
	}

	switch resp.Status {
	case domain.PaymentRefunded, domain.PaymentPartiallyRefunded:
		if err := r.finishRefund(ctx, p, rf, now); err != nil {
			return err
		}
		res.Resolved++
		r.log.Info(logging.EventPaymentReconcileResolved,
			slog.String("refund_id", rf.ID.String()), slog.String("payment_id", p.ID.String()))
		return nil
	default:
		// TODO(verify): neither FreedomPay's nor TipTopPay's status read is
		// confirmed, on a sandbox, to carry a signal that distinguishes "this
		// refund was explicitly declined" from "not (yet) recorded". Until
		// that is verified, anything other than a confirmed refund is treated
		// as still unknown — never as an explicit failure — so a refund that
		// is merely still processing at the acquirer is never wrongly marked
		// failed. This means requirement "явный отказ — пометить неудачей"
		// cannot be implemented safely with what these adapters expose today;
		// see the final report.
		return r.bumpRefundAttempt(ctx, rf, now, res)
	}
}

// finishRefund applies the exact ledger delta settleWithRefund's own tail
// would have written. It is idempotent from either entry point:
//   - rf.Status may already be domain.RefundSucceeded if a PREVIOUS call (the
//     original settleWithRefund, or an earlier reconciler pass) confirmed the
//     acquirer but crashed before the ledger/status commit — exactly the
//     "needs reconciliation" case settleWithRefund's own doc comment
//     describes. This resumes it instead of re-deriving anything.
//   - p.Status may already be domain.PaymentRefunded if that commit DID
//     complete after all (a race with the original request finishing late) —
//     nothing left to do.
func (r *Reconciler) finishRefund(ctx context.Context, p *domain.Payment, rf *domain.PaymentRefund, now time.Time) error {
	if rf.Status != domain.RefundSucceeded {
		if err := r.refunds.CompareAndSwapStatus(ctx, rf.ID, rf.Status, domain.RefundSucceeded, now); err != nil {
			if !errors.Is(err, domain.ErrAlreadyExists) {
				return err
			}
			current, gerr := r.refunds.GetByID(ctx, rf.ID)
			if gerr != nil {
				return gerr
			}
			if current.Status != domain.RefundSucceeded {
				return fmt.Errorf("refund %s moved to %s while reconciling, needs manual review: %w",
					rf.ID, current.Status, domain.ErrInvalidStatus)
			}
			rf = current
		} else {
			rf.Status = domain.RefundSucceeded
		}
	}
	if p.Status == domain.PaymentRefunded {
		return nil // already fully settled locally too
	}

	// Reconstruct the settlement this refund represents. settleWithRefund
	// only ever reaches the acquirer with RestaurantMinor == PlatformMinor ==
	// 0 (settlementLedgerEntries's doc comment: the guest-cancel-in-time and
	// venue-cancel branches are the only ones that call gw.Refund, and
	// neither changes the restaurant's or the platform's claim), so whatever
	// did not go back to the guest is exactly the acquirer's cut.
	settlement := domain.Settlement{
		GuestMinor:    rf.AmountMinor,
		AcquirerMinor: p.AmountMinor - rf.AmountMinor,
		Currency:      p.Currency,
	}
	if settlement.Total().AmountMinor != p.AmountMinor {
		return fmt.Errorf("reconciled refund %d + acquirer cost %d != payment total %d for payment %s, needs manual review: %w",
			settlement.GuestMinor, settlement.AcquirerMinor, p.AmountMinor, p.ID, domain.ErrValidation)
	}
	entries := settlementLedgerEntries(*p, settlement, &rf.ID, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return err
	}
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		if err := r.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, domain.PaymentRefunded, now); err != nil {
			return err
		}
		return publishPaymentEvent(ctx, r.outbox, p, domain.EventPaymentRefunded, now)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil // settled by someone else already
		}
		return err
	}
	return nil
}

func (r *Reconciler) bumpRefundAttempt(ctx context.Context, rf *domain.PaymentRefund, now time.Time, res *ReconcileResult) error {
	attempts, needsReview, err := r.refunds.RecordReconcileAttempt(ctx, rf.ID, rf.Status, now, r.cfg.MaxAttempts)
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			res.Resolved++ // resolved by someone else between our read and this write
			return nil
		}
		return err
	}
	res.StillUnknown++
	if needsReview {
		res.ManualReview++
		r.log.Error(logging.EventPaymentReconcileManualReview,
			slog.String("refund_id", rf.ID.String()), slog.Int("attempts", attempts))
	}
	return nil
}

// ---------------------------------------------------------------------------
// lost webhook: created/authorized gone stale with a provider payment id
// ---------------------------------------------------------------------------

func (r *Reconciler) reconcileLostWebhook(ctx context.Context, now time.Time, res *ReconcileResult) error {
	due, err := r.payments.ClaimStale(ctx,
		[]domain.PaymentStatus{domain.PaymentCreated, domain.PaymentAuthorized},
		now.Add(-r.cfg.LostWebhookAfter), r.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range due {
		p := &due[i]
		if p.ProviderPaymentID == nil {
			// Nothing to check yet: the guest never got far enough for the
			// acquirer to assign an id (an abandoned checkout, not stuck
			// money). Not counted as stuck.
			continue
		}
		if err := r.processPayment(ctx, p, now, res, true, r.resolveLostWebhook); err != nil {
			return err
		}
	}
	return nil
}

// resolveLostWebhook applies whatever the acquirer's own status turns out to
// be, through the EXACT SAME state-machine code a real webhook delivery would
// run (webhookUseCase.apply) — never a transition invented for this worker.
// If the acquirer's status already matches what is stored locally, nothing
// was actually lost; that still counts as resolved (the payment is no longer
// "unknown", it is confirmed correct).
func (r *Reconciler) resolveLostWebhook(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, resp *domain.GatewayPayment, now time.Time) (bool, string, error) {
	if resp.Status == p.Status {
		return true, "", nil
	}
	eventType, ok := eventTypeForStatus(resp.Status)
	if !ok {
		return false, fmt.Sprintf("acquirer reports %q for payment %s locally %q — no mapped webhook event to replay",
			resp.Status, p.ID, p.Status), nil
	}
	event := &domain.WebhookEvent{
		Provider: p.Provider, ProviderPaymentID: *p.ProviderPaymentID, MerchantPaymentID: p.ID.String(),
		Type: eventType, Status: resp.Status, Amount: resp.Amount, OccurredAt: now, SignatureValid: true,
		FailureCode: resp.FailureCode, FailureMessage: resp.FailureMessage,
	}
	applier := &webhookUseCase{payments: r.payments, ledger: r.ledger, outbox: r.outbox, tx: r.tx}
	if err := applier.apply(ctx, gw, p, event); err != nil {
		if errors.Is(err, domain.ErrInvalidStatus) {
			return false, fmt.Sprintf("acquirer reports %q, not a legal transition from local %q: %s", resp.Status, p.Status, err.Error()), nil
		}
		return false, "", err
	}
	return true, "", nil
}

// ---------------------------------------------------------------------------
// expired holds
// ---------------------------------------------------------------------------

func (r *Reconciler) reconcileExpiredHolds(ctx context.Context, now time.Time, res *ReconcileResult) error {
	due, err := r.payments.ClaimExpiredHolds(ctx, now, r.cfg.BatchSize)
	if err != nil {
		return err
	}
	for i := range due {
		if err := r.processPayment(ctx, &due[i], now, res, true, r.resolveExpiredHold); err != nil {
			return err
		}
	}
	return nil
}

// resolveExpiredHold applies spec §5's hold-TTL rule: our own ExpiresAt
// lapsed. If the acquirer confirms the hold is still just that, this worker
// (not staff) releases it — the guest is never charged for a hold nobody
// ever captured. If the acquirer already captured it (a race with staff
// seating the guest right at the boundary), that wins: the capture is synced
// in, never overwritten by an expiry. Anything the acquirer already resolved
// on its own (voided/failed/expired) is simply synced to match.
func (r *Reconciler) resolveExpiredHold(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, resp *domain.GatewayPayment, now time.Time) (bool, string, error) {
	applier := &webhookUseCase{payments: r.payments, ledger: r.ledger, outbox: r.outbox, tx: r.tx}

	switch resp.Status {
	case domain.PaymentAuthorized:
		// TODO(verify): confirm on the sandbox that a repeated Void is a safe
		// no-op if a previous attempt's response was lost before this line —
		// the shared HTTP client's own Idempotent flag for cancel/void is
		// already false for FreedomPay (see freedompay/gateway.go), so a
		// retry here is a genuinely new request, not a replay.
		if err := gw.Void(ctx, *p.ProviderPaymentID); err != nil {
			return false, "", fmt.Errorf("void expired hold for payment %s: %w", p.ID, err)
		}
		event := &domain.WebhookEvent{
			Provider: p.Provider, ProviderPaymentID: *p.ProviderPaymentID, MerchantPaymentID: p.ID.String(),
			Type: domain.WebhookPaymentVoided, Status: domain.PaymentVoided, OccurredAt: now, SignatureValid: true,
		}
		if err := applier.apply(ctx, gw, p, event); err != nil {
			return false, "", err
		}
		return true, "", nil
	case domain.PaymentCaptured, domain.PaymentVoided, domain.PaymentFailed, domain.PaymentExpired:
		eventType, _ := eventTypeForStatus(resp.Status) // always ok for this set
		event := &domain.WebhookEvent{
			Provider: p.Provider, ProviderPaymentID: *p.ProviderPaymentID, MerchantPaymentID: p.ID.String(),
			Type: eventType, Status: resp.Status, Amount: resp.Amount, OccurredAt: now, SignatureValid: true,
			FailureCode: resp.FailureCode, FailureMessage: resp.FailureMessage,
		}
		if err := applier.apply(ctx, gw, p, event); err != nil {
			if errors.Is(err, domain.ErrInvalidStatus) {
				return true, "", nil // already applied by something else; consistent
			}
			return false, "", err
		}
		return true, "", nil
	default:
		return false, fmt.Sprintf("expired hold %s reported unexpected acquirer status %q", p.ID, resp.Status), nil
	}
}

// eventTypeForStatus maps an acquirer-reported domain.PaymentStatus onto the
// webhook event type that would carry the same fact, so resolveLostWebhook /
// resolveExpiredHold can replay it through webhookUseCase.apply verbatim.
func eventTypeForStatus(s domain.PaymentStatus) (domain.WebhookEventType, bool) {
	switch s {
	case domain.PaymentAuthorized:
		return domain.WebhookPaymentAuthorized, true
	case domain.PaymentCaptured:
		return domain.WebhookPaymentCaptured, true
	case domain.PaymentVoided:
		return domain.WebhookPaymentVoided, true
	case domain.PaymentFailed:
		return domain.WebhookPaymentFailed, true
	case domain.PaymentExpired:
		return domain.WebhookPaymentExpired, true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// shared per-payment driver
// ---------------------------------------------------------------------------

// paymentResolver asks the acquirer's own view (resp) what actually happened
// and applies whatever transition follows from it. It returns:
//   - (true, "", nil) when the row is resolved (fixed, or confirmed already
//     consistent);
//   - (false, anomaly, nil) when resp is a real answer but this build does not
//     have a safe, verified transition for it — anomaly is logged and the
//     attempt counter is bumped, same as an outright unknown;
//   - (false, "", err) only for an infrastructure failure (a DB write failed),
//     which aborts the whole tick rather than silently miscounting.
type paymentResolver func(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, resp *domain.GatewayPayment, now time.Time) (resolved bool, anomaly string, err error)

// processPayment is the shared claim → backoff → rate-limited Get → resolve →
// bump-or-resolve driver behind every payment-level stage (capturing,
// voiding, lost webhook, expired holds). requireProviderID distinguishes "a
// missing provider payment id is itself an anomaly" (capturing/voiding/expired
// holds — always have one by the time they reach these statuses) from "a
// missing id here is just an abandoned checkout, not stuck money" (the lost
// webhook scan sees `created` rows too and filters those before ever calling
// this).
func (r *Reconciler) processPayment(ctx context.Context, p *domain.Payment, now time.Time, res *ReconcileResult, requireProviderID bool, resolve paymentResolver) error {
	if p.NeedsManualReview {
		res.ManualReview++
		return nil
	}
	if p.LastReconcileAttemptAt != nil && now.Before(p.LastReconcileAttemptAt.Add(backoffDuration(r.cfg.StuckAfter, p.ReconcileAttempts))) {
		return nil // still backing off
	}
	res.StuckFound++

	if p.ProviderPaymentID == nil {
		if !requireProviderID {
			return nil
		}
		r.log.Error("payment.reconcile_no_provider_id", slog.String("payment_id", p.ID.String()), slog.String("status", string(p.Status)))
		return r.bumpPaymentAttempt(ctx, p, now, res)
	}

	gw, err := r.gateways.ForRefund(p.Provider)
	if err != nil {
		r.log.Warn("payment.reconcile_gateway_unavailable",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return r.bumpPaymentAttempt(ctx, p, now, res)
	}

	r.pace.wait()
	// External call, deliberately outside any DB transaction (hard rule: the
	// acquirer is never asked from inside a DB transaction).
	resp, err := gw.Get(ctx, *p.ProviderPaymentID)
	if err != nil {
		r.log.Warn(logging.EventPaymentReconcileUnknown,
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return r.bumpPaymentAttempt(ctx, p, now, res)
	}

	resolved, anomaly, err := resolve(ctx, gw, p, resp, now)
	if err != nil {
		return err
	}
	if !resolved {
		if anomaly != "" {
			r.log.Error(logging.EventPaymentReconcileUnknown,
				slog.String("payment_id", p.ID.String()),
				slog.String("acquirer_status", string(resp.Status)),
				slog.String("anomaly", anomaly))
		}
		return r.bumpPaymentAttempt(ctx, p, now, res)
	}
	res.Resolved++
	r.log.Info(logging.EventPaymentReconcileResolved,
		slog.String("payment_id", p.ID.String()), slog.String("acquirer_status", string(resp.Status)))
	return nil
}

func (r *Reconciler) bumpPaymentAttempt(ctx context.Context, p *domain.Payment, now time.Time, res *ReconcileResult) error {
	attempts, needsReview, err := r.payments.RecordReconcileAttempt(ctx, p.ID, p.Status, now, r.cfg.MaxAttempts)
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			res.Resolved++ // resolved by someone else between our read and this write
			return nil
		}
		return err
	}
	res.StillUnknown++
	if needsReview {
		res.ManualReview++
		r.log.Error(logging.EventPaymentReconcileManualReview,
			slog.String("payment_id", p.ID.String()), slog.Int("attempts", attempts))
	}
	return nil
}

// backoffDuration doubles base per attempt (capped) so a row that keeps
// coming back unknown is asked about less and less often, instead of every
// single tick burning one more acquirer call for no new information.
func backoffDuration(base time.Duration, attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	const capAttempts = 6 // 2^6 = 64x base, then it stops growing
	const maxBackoff = time.Hour
	n := attempts
	if n > capAttempts {
		n = capAttempts
	}
	d := base
	for i := 0; i < n; i++ {
		d *= 2
		if d > maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// pacer is the avalanche guard between this worker and the acquirer: at most
// one call every minGap, across however many rows one tick processes. Real
// wall-clock time on purpose (not the injectable business clock) — it paces
// actual network calls, not the staleness bookkeeping.
type pacer struct {
	mu     sync.Mutex
	minGap time.Duration
	last   time.Time
}

func (p *pacer) wait() {
	if p.minGap <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.last.IsZero() {
		if d := p.minGap - now.Sub(p.last); d > 0 {
			time.Sleep(d)
			now = time.Now()
		}
	}
	p.last = now
}

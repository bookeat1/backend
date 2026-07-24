package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend-core/internal/domain"
)

// Reconciler resolves payouts stranded in `sent`: a process may have died
// between winning the pending→sent claim and the acquirer answering, or the
// acquirer's answer was a timeout/5xx that SendPayout correctly refused to
// interpret. Money may already have moved at the acquirer while our row sits
// frozen. The acquirer's own status endpoint (PayoutGateway.GetPayout, queried
// by OUR order id) is the only source of truth; this worker asks it and applies
// EXACTLY the same transitions SendPayout would — never a transition invented
// here, every write CAS-guarded.
//
// The claim/lock discipline is identical to the payments reconciler and exists
// because of a real past bug (ADR-005): ClaimStale runs INSIDE a transaction so
// FOR UPDATE SKIP LOCKED actually holds its lock, but the acquirer call happens
// OUTSIDE that transaction — the lock is never held across the network round
// trip, and correctness comes from the CAS writes afterwards, not the lock.
type Reconciler struct {
	payouts domain.PayoutRepository
	items   domain.PayoutItemRepository
	gateway domain.PayoutGateway
	tx      domain.TxManager
	cfg     ReconcilerConfig
	log     *slog.Logger
	now     func() time.Time
}

// ReconcilerConfig is the worker's scheduling and safety configuration.
type ReconcilerConfig struct {
	TickInterval time.Duration // pause between passes
	StuckAfter   time.Duration // how long a payout may sit `sent` before it is chased
	BatchSize    int           // rows claimed per pass
	MaxAttempts  int           // unresolved attempts before NeedsManualReview
}

const (
	defaultPayoutReconcileTickInterval = 2 * time.Minute
	defaultPayoutReconcileStuckAfter   = 10 * time.Minute
	defaultPayoutReconcileBatchSize    = 50
	defaultPayoutReconcileMaxAttempts  = 5
)

func (c ReconcilerConfig) withDefaults() ReconcilerConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultPayoutReconcileTickInterval
	}
	if c.StuckAfter <= 0 {
		c.StuckAfter = defaultPayoutReconcileStuckAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultPayoutReconcileBatchSize
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultPayoutReconcileMaxAttempts
	}
	return c
}

// NewReconciler builds the payout reconciler.
func NewReconciler(payouts domain.PayoutRepository, items domain.PayoutItemRepository, gateway domain.PayoutGateway, tx domain.TxManager, cfg ReconcilerConfig, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{
		payouts: payouts,
		items:   items,
		gateway: gateway,
		tx:      tx,
		cfg:     cfg.withDefaults(),
		log:     log,
		now:     time.Now,
	}
}

// ReconcileResult reports what a pass did.
type ReconcileResult struct {
	Claimed  int
	Resolved int
	Bumped   int
}

// Run loops until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := r.Tick(ctx); err != nil {
				r.log.Error("payout reconcile tick failed", "err", err.Error())
			}
		}
	}
}

// Tick runs one reconciliation pass.
func (r *Reconciler) Tick(ctx context.Context) (ReconcileResult, error) {
	now := r.now()
	var res ReconcileResult

	if r.gateway == nil {
		return res, nil // nothing wired; safe idle
	}

	var due []domain.Payout
	if err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		var e error
		due, e = r.payouts.ClaimStale(ctx, []domain.PayoutStatus{domain.PayoutSent},
			now.Add(-r.cfg.StuckAfter), r.cfg.BatchSize)
		return e
	}); err != nil {
		return res, fmt.Errorf("claim stale payouts: %w", err)
	}
	res.Claimed = len(due)

	for i := range due {
		resolved, err := r.resolveOne(ctx, &due[i])
		if err != nil {
			return res, err
		}
		if resolved {
			res.Resolved++
		} else {
			res.Bumped++
		}
	}
	return res, nil
}

// resolveOne asks the acquirer about one stuck payout and applies the outcome.
// It returns true when the payout reached a terminal state, false when it was
// left `sent` (still processing, or an unknown answer) and only its attempt
// counter bumped.
func (r *Reconciler) resolveOne(ctx context.Context, p *domain.Payout) (bool, error) {
	resp, err := r.gateway.GetPayout(ctx, p.ID.String())
	if err != nil {
		// Unknown / transport / declined-status-read: cannot act, bump and back off.
		return false, r.bump(ctx, p)
	}
	now := r.now()
	switch resp.Status {
	case domain.PayoutPaid:
		patch := domain.PayoutStatusPatch{}
		if resp.ProviderRef != "" {
			ref := resp.ProviderRef
			patch.ProviderRef = &ref
		}
		if err := r.payouts.CompareAndSwapStatus(ctx, p.ID, domain.PayoutSent, domain.PayoutPaid, patch, now); err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				return true, nil // another pass/send resolved it — fine
			}
			return false, err
		}
		r.log.Info("payout reconciled to paid", "payout_id", p.ID)
		return true, nil
	case domain.PayoutFailed:
		code := "declined"
		reason := resp.FailureMessage
		if reason == "" {
			reason = "acquirer reported the payout failed"
		}
		patch := domain.PayoutStatusPatch{FailureCode: &code, FailureReason: &reason}
		if err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
			if e := r.payouts.CompareAndSwapStatus(ctx, p.ID, domain.PayoutSent, domain.PayoutFailed, patch, now); e != nil {
				return e
			}
			return r.items.DeleteByPayout(ctx, p.ID)
		}); err != nil {
			if errors.Is(err, domain.ErrAlreadyExists) {
				return true, nil
			}
			return false, err
		}
		r.log.Info("payout reconciled to failed, claim released", "payout_id", p.ID)
		return true, nil
	default:
		// Still processing (PayoutSent) or an unmapped status: leave it, bump.
		return false, r.bump(ctx, p)
	}
}

// bump records one unresolved reconciliation attempt, flagging manual review at
// the configured maximum. A payout that moved on between our read and this
// write (ErrAlreadyExists/ErrNotFound) is treated as already resolved.
func (r *Reconciler) bump(ctx context.Context, p *domain.Payout) error {
	_, _, err := r.payouts.RecordReconcileAttempt(ctx, p.ID, domain.PayoutSent, r.now(), r.cfg.MaxAttempts)
	if err != nil && (errors.Is(err, domain.ErrAlreadyExists) || errors.Is(err, domain.ErrNotFound)) {
		return nil
	}
	return err
}

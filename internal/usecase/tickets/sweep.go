package tickets

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// PendingSweeper releases seats held by stale pending tickets. It is the
// backstop the purchase flow relies on: a pending ticket can be left holding a
// seat with NO usable payment — the payment intent was never created (a crash,
// or gw.Authorize failing together with the release write), or its row is in a
// terminal-unpaid state the observer somehow never projected. The payments
// reconciler only scans `payments`, so a pending ticket with no payment row at
// all is invisible to it; this sweep closes that gap.
//
// It never touches a ticket whose payment is still LIVE (created/authorized/
// capturing/voiding): that hold may still capture, and cancelling the ticket
// underneath a capture that then lands would leave a paid guest with a
// cancelled ticket. Those are left to the payments reconciler, whose
// expired-hold pass voids a dead hold and — now that the reconciler carries the
// ticket observer — projects that void onto the ticket.
type PendingSweeper struct {
	tickets  domain.EventTicketRepository
	payments paymentReader
	cfg      SweepConfig
	log      *slog.Logger
	now      func() time.Time
}

// SweepConfig tunes the sweep. Zero values fall back to defaults.
type SweepConfig struct {
	// TickInterval is the pause between passes. env: TICKETS_SWEEP_TICK_INTERVAL
	TickInterval time.Duration
	// StaleAfter is how old a pending ticket must be before it is swept. It must
	// comfortably exceed the payment HoldTTL so a ticket whose hold is still
	// legitimately in flight is never swept. env: TICKETS_SWEEP_STALE_AFTER
	StaleAfter time.Duration
	// BatchSize caps how many tickets one pass processes. env: TICKETS_SWEEP_BATCH_SIZE
	BatchSize int
}

const (
	defaultSweepTickInterval = 5 * time.Minute
	defaultSweepStaleAfter   = 100 * time.Hour // > payments HoldTTL (96h) + slack
	defaultSweepBatchSize    = 100
)

func (c SweepConfig) withDefaults() SweepConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultSweepTickInterval
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = defaultSweepStaleAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultSweepBatchSize
	}
	return c
}

// NewPendingSweeper constructs the sweep worker.
func NewPendingSweeper(tickets domain.EventTicketRepository, payments paymentReader, cfg SweepConfig, log *slog.Logger) *PendingSweeper {
	return &PendingSweeper{tickets: tickets, payments: payments, cfg: cfg.withDefaults(), log: log, now: time.Now}
}

// Run ticks until ctx is cancelled; a failing pass is logged and retried next
// tick, never fatal — same contract as the payments reconciler.
func (s *PendingSweeper) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A graceful shutdown (SIGINT/SIGTERM cancels the shared context) is
			// not an error — return nil so cmd/worker exits 0, same as the
			// booking worker and the notification dispatcher.
			return nil
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				s.log.Error("tickets.sweep_failed", slog.String("error", err.Error()))
			} else if n > 0 {
				s.log.Info("tickets.swept", slog.Int("released", n))
			}
		}
	}
}

// Sweep processes one batch of stale pending tickets and returns how many seats
// it released (cancelled or projected to paid). It is safe to run concurrently
// with the webhook/reconciler: every write is a CAS from `pending`, so a ticket
// that moved on its own between the read and the write is simply skipped.
func (s *PendingSweeper) Sweep(ctx context.Context) (int, error) {
	before := s.now().Add(-s.cfg.StaleAfter)
	stale, err := s.tickets.ListStalePending(ctx, before, s.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	released := 0
	for i := range stale {
		acted, err := s.resolve(ctx, &stale[i])
		if err != nil {
			return released, err
		}
		if acted {
			released++
		}
	}
	return released, nil
}

// resolve decides one stale pending ticket's fate from its payment:
//   - no payment row at all                       → cancel (free the seat);
//   - payment terminal-unpaid (failed/voided/expired/refunded) → cancel;
//   - payment captured (observer missed it)       → project to paid;
//   - payment still live (created/authorized/...)  → leave it for the payments
//     reconciler (cancelling under a hold that may still capture is unsafe).
func (s *PendingSweeper) resolve(ctx context.Context, t *domain.EventTicket) (bool, error) {
	to := domain.TicketCancelled
	if t.PaymentID != nil {
		p, err := s.payments.GetByID(ctx, *t.PaymentID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Payment vanished — free the seat.
				return s.cas(ctx, t, domain.TicketCancelled)
			}
			return false, err
		}
		switch p.Status {
		case domain.PaymentCaptured:
			to = domain.TicketPaid
		case domain.PaymentFailed, domain.PaymentVoided, domain.PaymentExpired, domain.PaymentRefunded:
			to = domain.TicketCancelled
		default:
			// created / authorized / capturing / voiding — still resolvable by
			// the payments reconciler; do not touch the seat.
			return false, nil
		}
	}
	return s.cas(ctx, t, to)
}

func (s *PendingSweeper) cas(ctx context.Context, t *domain.EventTicket, to domain.TicketStatus) (bool, error) {
	err := s.tickets.CompareAndSwapStatus(ctx, t.ID, domain.TicketPending, to, s.now())
	if err == nil {
		logging.FromContext(ctx).Info("tickets.sweep_resolved",
			slog.String("ticket_id", t.ID.String()), slog.String("to", string(to)))
		return true, nil
	}
	if errors.Is(err, domain.ErrAlreadyExists) {
		// Moved on its own between our read and write — not our seat to release.
		return false, nil
	}
	return false, err
}

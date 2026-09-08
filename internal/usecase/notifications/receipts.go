package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend-core/internal/domain"
)

// MobilePushReceipt is the provider's FINAL word about one accepted push.
//
// Reason carries the provider's machine-readable error code (Expo's
// details.error: DeviceNotRegistered, MessageTooBig, MessageRateExceeded,
// MismatchSenderId, InvalidCredentials). It is safe to log — unlike the
// provider's message text, which can quote the push token back at us.
type MobilePushReceipt struct {
	Verdict MobilePushVerdict
	Reason  string
}

// MobilePushReceiptFetcher reads the receipts for a batch of ticket ids. It is
// the second half of the MobilePushSender seam: the sender says "accepted", this
// says what actually happened on the device.
//
// A ticket id MISSING from the returned map means "no receipt yet" — a normal,
// frequent state, observed live on 2026-09-01. The caller must leave such a
// ticket unresolved and ask again later; treating absent as delivered would
// silently swallow exactly the DeviceNotRegistered this whole path exists for.
//
// A non-nil error is a TRANSIENT failure (timeout, 429, 5xx): nothing in the
// batch is decided and the whole batch is retried on a later tick.
type MobilePushReceiptFetcher func(ctx context.Context, ticketIDs []string) (map[string]MobilePushReceipt, error)

// ReceiptWorkerConfig is the receipt poller's schedule and batching.
type ReceiptWorkerConfig struct {
	// TickInterval is the pause between two passes. env:
	// PUSH_RECEIPTS_TICK_INTERVAL
	TickInterval time.Duration
	// MinAge is how long a ticket must age before it is worth asking about.
	// Expo's own guidance is ~15 minutes; asking earlier mostly returns "not
	// ready yet" and burns a request. env: PUSH_RECEIPTS_MIN_AGE
	MinAge time.Duration
	// MaxAge is when an unanswered ticket is force-resolved. Providers keep
	// receipts for 24 hours, so past that the answer no longer exists and the
	// row is pure growth. env: PUSH_RECEIPTS_MAX_AGE
	MaxAge time.Duration
	// BatchSize is how many ticket ids go into ONE provider request. Expo
	// rejects more than 1000 with PUSH_TOO_MANY_RECEIPTS, which is why the
	// value is clamped rather than trusted. env: PUSH_RECEIPTS_BATCH_SIZE
	BatchSize int
	// MaxPerTick caps how many tickets one pass reads from the queue, i.e. how
	// many provider requests a single tick can make (MaxPerTick / BatchSize).
	// env: PUSH_RECEIPTS_MAX_PER_TICK
	MaxPerTick int
}

const (
	defaultReceiptTickInterval = 15 * time.Minute
	defaultReceiptMinAge       = 15 * time.Minute
	// defaultReceiptMaxAge matches the provider's retention exactly: Expo's
	// docs say "push receipts are cleared after 24 hours". A ticket older than
	// that can never be answered.
	defaultReceiptMaxAge     = 24 * time.Hour
	defaultReceiptBatchSize  = 500
	defaultReceiptMaxPerTick = 2000
	// maxReceiptBatch is the provider's hard limit on ids per request.
	maxReceiptBatch = 1000
)

func (c ReceiptWorkerConfig) withDefaults() ReceiptWorkerConfig {
	if c.TickInterval <= 0 {
		c.TickInterval = defaultReceiptTickInterval
	}
	if c.MinAge <= 0 {
		// Zero is not "poll instantly": a receipt that does not exist yet costs
		// a request and answers nothing. An operator who really wants a shorter
		// grace period sets an explicit short duration.
		c.MinAge = defaultReceiptMinAge
	}
	if c.MaxAge <= 0 {
		c.MaxAge = defaultReceiptMaxAge
	}
	if c.MaxAge > defaultReceiptMaxAge {
		// Keeping a ticket longer than the provider keeps its receipt only
		// grows the table: the answer is already gone.
		c.MaxAge = defaultReceiptMaxAge
	}
	if c.MaxAge <= c.MinAge {
		// A window that closes before it opens would force-resolve every ticket
		// before it was ever asked about — the exact blindness this worker
		// exists to remove.
		c.MinAge = defaultReceiptMinAge
		if c.MaxAge <= c.MinAge {
			c.MaxAge = defaultReceiptMaxAge
		}
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultReceiptBatchSize
	}
	if c.BatchSize > maxReceiptBatch {
		c.BatchSize = maxReceiptBatch
	}
	if c.MaxPerTick <= 0 {
		c.MaxPerTick = defaultReceiptMaxPerTick
	}
	if c.MaxPerTick < c.BatchSize {
		c.MaxPerTick = c.BatchSize
	}
	return c
}

// ReceiptWorker closes the loop the send path cannot close on its own.
//
// The mobile providers answer a send with a TICKET, not with a delivery. Until
// this worker existed the ticket id was thrown away, so a device that had been
// deregistered by FCM/APNs looked like a successful delivery forever: the guest
// got nothing, the log was clean and the token stayed is_active. Proven live on
// 2026-09-01 — three production android tokens, all three `ok` on send, two
// DeviceNotRegistered in the receipts.
//
// Discipline, mirroring the notification dispatcher:
//
//   - one instance, a plain ticker, and a pass that is a cheap no-op when the
//     queue is empty;
//   - the provider call happens outside any transaction;
//   - a ticket is resolved ONLY when the provider actually answered about it, or
//     when it aged out of the provider's 24-hour retention. "No receipt yet" is
//     left alone for the next tick — that state is normal, not an error;
//   - a receipt saying the device is gone deactivates the token through the same
//     DevicePushTokenRepository.DeactivateByID the send path uses; every OTHER
//     error leaves the token alone. MessageRateExceeded or MessageTooBig say
//     something about the message, not about the device, and deactivating on
//     them would silence a perfectly live phone.
//
// KNOWN LIMIT: a resolved ticket is marked, not deleted, so push_tickets grows
// by one row per push forever. At BookEat's volume that is small and the
// partial index only covers the unresolved rows, so nothing degrades — but a
// retention pass (delete resolved rows older than N days) is the obvious next
// increment, not something this worker already does.
type ReceiptWorker struct {
	tickets domain.PushTicketRepository
	tokens  domain.DevicePushTokenRepository
	fetch   MobilePushReceiptFetcher
	cfg     ReceiptWorkerConfig
	log     *slog.Logger
	now     func() time.Time // injectable clock for tests
}

// NewReceiptWorker builds the receipt poller.
func NewReceiptWorker(
	tickets domain.PushTicketRepository,
	tokens domain.DevicePushTokenRepository,
	fetch MobilePushReceiptFetcher,
	cfg ReceiptWorkerConfig,
	log *slog.Logger,
) *ReceiptWorker {
	return &ReceiptWorker{
		tickets: tickets,
		tokens:  tokens,
		fetch:   fetch,
		cfg:     cfg.withDefaults(),
		log:     log,
		now:     time.Now,
	}
}

// ReceiptTickResult counts what one pass did. Zero values are the steady state.
type ReceiptTickResult struct {
	Expired   int // aged past the provider's retention → force-resolved
	Polled    int // tickets asked about
	Delivered int // receipts confirming the device got it
	Gone      int // DeviceNotRegistered → token deactivated
	Rejected  int // any other provider error → logged, token untouched
	Pending   int // no receipt yet → left for a later tick
}

func (r ReceiptTickResult) attrs() []any {
	return []any{
		slog.Int("expired", r.Expired), slog.Int("polled", r.Polled),
		slog.Int("delivered", r.Delivered), slog.Int("gone", r.Gone),
		slog.Int("rejected", r.Rejected), slog.Int("pending", r.Pending),
	}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on the
// next tick — a transient provider or database error must not kill the process.
func (w *ReceiptWorker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()
	w.log.Info("push receipt worker started",
		slog.Duration("tick", w.cfg.TickInterval),
		slog.Duration("min_age", w.cfg.MinAge),
		slog.Duration("max_age", w.cfg.MaxAge),
		slog.Int("batch", w.cfg.BatchSize))
	for {
		select {
		case <-ctx.Done():
			w.log.Info("push receipt worker stopped")
			return nil
		case <-t.C:
			res, err := w.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				w.log.Error("push receipt worker tick failed", slog.String("error", err.Error()))
			}
			if res != (ReceiptTickResult{}) {
				w.log.Info("push receipt worker tick", res.attrs()...)
			}
		}
	}
}

// Tick runs one pass. Exported so it can be driven directly from tests.
func (w *ReceiptWorker) Tick(ctx context.Context) (ReceiptTickResult, error) {
	now := w.now()
	var res ReceiptTickResult

	// 1. Age out first. It must happen BEFORE the queue is read: a ticket the
	//    provider no longer has a receipt for would otherwise be asked about on
	//    every single tick for the rest of the table's life.
	expired, err := w.tickets.ExpireOlderThan(ctx, now.Add(-w.cfg.MaxAge), now)
	if err != nil {
		return res, fmt.Errorf("expire stale push tickets: %w", err)
	}
	res.Expired = int(expired)
	if expired > 0 {
		w.log.Info("push receipts: tickets aged out unanswered, force-resolved",
			slog.Int64("count", expired), slog.Duration("max_age", w.cfg.MaxAge))
	}

	// 2. Read the queue: oldest first, only tickets old enough to have a
	//    receipt.
	pending, err := w.tickets.ListUnresolved(ctx, now.Add(-w.cfg.MinAge), w.cfg.MaxPerTick)
	if err != nil {
		return res, fmt.Errorf("list unresolved push tickets: %w", err)
	}
	if len(pending) == 0 {
		return res, nil
	}

	byTicket := make(map[string]domain.PushTicket, len(pending))
	ids := make([]string, 0, len(pending))
	for _, t := range pending {
		byTicket[t.ID] = t
		ids = append(ids, t.ID)
	}

	// 3. One provider request per batch. The provider rejects an oversized
	//    batch outright, so the chunking is a correctness requirement, not a
	//    politeness.
	for start := 0; start < len(ids); start += w.cfg.BatchSize {
		end := start + w.cfg.BatchSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		batchRes, err := w.pollBatch(ctx, chunk, byTicket, now)
		res.add(batchRes)
		if err != nil {
			// Transient: this batch stays unresolved and is re-read next tick.
			// Stop the pass rather than hammer a provider that is already
			// failing.
			return res, err
		}
	}
	return res, nil
}

// pollBatch asks about one batch and applies the answers.
func (w *ReceiptWorker) pollBatch(
	ctx context.Context,
	chunk []string,
	byTicket map[string]domain.PushTicket,
	now time.Time,
) (ReceiptTickResult, error) {
	var res ReceiptTickResult
	res.Polled = len(chunk)

	receipts, err := w.fetch(ctx, chunk)
	if err != nil {
		return res, fmt.Errorf("fetch push receipts: %w", err)
	}

	resolved := make([]string, 0, len(chunk))
	for _, id := range chunk {
		r, ok := receipts[id]
		if !ok {
			// Not ready yet. Expo answers only about the tickets it already has
			// a receipt for, so an absent id is the normal "ask me later".
			res.Pending++
			continue
		}
		switch r.Verdict {
		case MobilePushDelivered:
			res.Delivered++
			resolved = append(resolved, id)
		case MobilePushDeviceGone:
			t := byTicket[id]
			w.log.Info("push receipt: device is gone, deactivating token",
				slog.String("device_token_id", t.DeviceTokenID.String()),
				slog.String("reason", r.Reason))
			if err := w.tokens.DeactivateByID(ctx, t.DeviceTokenID); err != nil {
				// Leave the ticket UNRESOLVED: the whole point of the receipt
				// is the deactivation, so it must be retried. The 24-hour
				// expiry bounds how long that retry can go on.
				w.log.Error("push receipt: deactivate gone device token failed",
					slog.String("device_token_id", t.DeviceTokenID.String()),
					slog.String("error", err.Error()))
				continue
			}
			res.Gone++
			resolved = append(resolved, id)
		default:
			// Rejected: the message was the problem, not the device
			// (MessageTooBig, MessageRateExceeded, MismatchSenderId,
			// InvalidCredentials). The token is deliberately left ACTIVE —
			// silencing a live phone over a rate limit would be a worse bug
			// than the one this worker fixes. MismatchSenderId /
			// InvalidCredentials are an operator's problem and are logged at
			// WARN so they are greppable.
			t := byTicket[id]
			w.log.Warn("push receipt: provider rejected the message",
				slog.String("device_token_id", t.DeviceTokenID.String()),
				slog.String("reason", r.Reason))
			res.Rejected++
			resolved = append(resolved, id)
		}
	}

	if len(resolved) > 0 {
		if err := w.tickets.Resolve(ctx, resolved, now); err != nil {
			// The tickets stay in the queue and are re-polled. Re-deactivating
			// an already deactivated token is a no-op, so a repeat is harmless.
			return res, fmt.Errorf("resolve push tickets: %w", err)
		}
	}
	return res, nil
}

func (r *ReceiptTickResult) add(o ReceiptTickResult) {
	r.Polled += o.Polled
	r.Delivered += o.Delivered
	r.Gone += o.Gone
	r.Rejected += o.Rejected
	r.Pending += o.Pending
}

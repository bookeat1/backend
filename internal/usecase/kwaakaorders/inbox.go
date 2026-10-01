package kwaakaorders

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/kwaaka"
)

// WithInbox enables webhook processing, polling and pruning. Optional: without
// it those stages are no-ops (sending and cancelling do not need them).
func (w *Worker) WithInbox(hooks domain.KwaakaWebhookRepository, pollInterval time.Duration) *Worker {
	w.hooks, w.pollEvery = hooks, pollInterval
	return w
}

// retry schedule for a status about an order we do not know yet (the POST that
// carries Kwaaka's id may not have been recorded): 1, 2, 5 minutes, then give up.
var unknownOrderDelays = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}

const inboxBatch = 20

// ProcessInbox parses and applies stored webhooks. Each batch is one
// transaction (lock → apply → mark), no HTTP inside.
func (w *Worker) ProcessInbox(ctx context.Context) error {
	if w.hooks == nil {
		return nil
	}
	return w.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := w.now()
		evs, err := w.hooks.LockDue(ctx, now, inboxBatch)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := w.processEvent(ctx, e, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *Worker) processEvent(ctx context.Context, e domain.KwaakaWebhookEvent, now time.Time) error {
	switch e.Kind {
	case domain.KwaakaWebhookReserveStatus:
		// Skeleton: kept, nothing applied (reservations do not go to the POS in phase 2).
		return w.hooks.Finish(ctx, e.ID, domain.WebhookOutcomeStored, nil, nil)
	case domain.KwaakaWebhookOrderStatus:
	default:
		msg := "unknown webhook kind"
		return w.hooks.Finish(ctx, e.ID, domain.WebhookOutcomeUnparseable, &msg, nil)
	}
	ev, err := kwaaka.ParseOrderStatus(e.Body)
	if err != nil {
		// Never log the body: it carries a guest's order reference.
		w.log.Error("kwaaka webhook unparseable", slog.String("event_id", e.ID.String()))
		msg := "unparseable"
		return w.hooks.Finish(ctx, e.ID, domain.WebhookOutcomeUnparseable, &msg, nil)
	}
	// A savepoint-free approach: ApplyPOSStatus errors other than "unknown order"
	// abort the batch (rolled back, retried next tick), so nothing is half-applied.
	outcome, orderID, err := w.ApplyPOSStatus(ctx, ev, "webhook")
	switch {
	case errors.Is(err, errUnknownOrder):
		if e.Attempts < len(unknownOrderDelays) {
			return w.hooks.Defer(ctx, e.ID, now.Add(unknownOrderDelays[e.Attempts]))
		}
		w.log.Warn("kwaaka webhook for unknown order dropped", slog.String("event_id", e.ID.String()))
		return w.hooks.Finish(ctx, e.ID, domain.WebhookOutcomeIgnoredUnknownOrder, nil, nil)
	case err != nil:
		return err
	}
	return w.hooks.Finish(ctx, e.ID, outcome, nil, orderID)
}

// PollPass is the reconciliation: orders whose status the webhook did not
// deliver get one GET each. It writes through ApplyPOSStatus, like the webhook.
// pollEvery <= 0 switches it off.
func (w *Worker) PollPass(ctx context.Context) error {
	if w.pollEvery <= 0 {
		return nil
	}
	now := w.now()
	rows, err := w.orders.ListPollDue(ctx, now, now.Add(-w.pollEvery), 50)
	if err != nil {
		return err
	}
	for _, o := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := o.ID.String()
		if o.KwaakaOrderID != nil {
			id = *o.KwaakaOrderID
		}
		got, err := w.pos.GetOrder(ctx, o.KwaakaRestaurantID, id)
		if err != nil {
			continue // not found / POS down: the next pass tries again
		}
		ev := domain.KwaakaOrderStatusEvent{OrderID: id, Raw: got.Raw, State: got.State, TableIDs: got.TableIDs,
			WhenBillPrinted: got.WhenBillPrinted, WhenClosed: got.WhenClosed}
		if err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
			_, _, err := w.ApplyPOSStatus(ctx, ev, "poll")
			return err
		}); err != nil && !errors.Is(err, errLostCAS) && !errors.Is(err, errUnknownOrder) {
			w.log.Error("kwaaka poll apply failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
		}
	}
	return nil
}

// PruneInbox deletes processed webhooks older than 30 days (the body holds a
// guest-linked reference).
func (w *Worker) PruneInbox(ctx context.Context) error {
	if w.hooks == nil {
		return nil
	}
	_, err := w.hooks.PruneProcessed(ctx, w.now().Add(-30*24*time.Hour), 500)
	return err
}

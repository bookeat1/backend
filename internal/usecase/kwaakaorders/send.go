package kwaakaorders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// alert writes the one venue-facing event for a transition, inside the caller's
// transaction. reason ∈ failed | failed_unknown | cancel_failed | rescheduled |
// pos_cancelled.
func (w *Worker) alert(ctx context.Context, o *domain.KitchenOrder, reason, errText string) error {
	payload, err := json.Marshal(map[string]string{"reason": reason, "error_text": errText})
	if err != nil {
		return err
	}
	return w.outbox.Create(ctx, &domain.BookingOutboxEvent{
		ID: newID(), BookingID: o.BookingID, EventType: domain.EventBookingKitchenOrderAttention,
		Payload: payload, CreatedAt: w.now(),
	})
}

// commit writes o with a CAS on (wantStatus, wantAttempts) and, when reason is
// set and the booking is still alive, the alert — in ONE transaction, so the
// alert exists exactly when the transition did. A lost CAS returns errLostCAS.
func (w *Worker) commit(ctx context.Context, o *domain.KitchenOrder, wantStatus domain.KitchenOrderStatus, wantAttempts int, reason string) error {
	return w.tx.WithinTx(ctx, func(ctx context.Context) error {
		ok, err := w.orders.CompareAndSet(ctx, o, wantStatus, wantAttempts)
		if err != nil {
			return err
		}
		if !ok {
			return errLostCAS
		}
		// A cancelled booking gets no "not sent" alert: nobody is waiting for it.
		if reason != "" && o.CancelRequestedAt == nil {
			text := ""
			if o.LastError != nil {
				text = *o.LastError
			}
			return w.alert(ctx, o, reason, text)
		}
		return nil
	})
}

// backoff: 30s, 1, 2, 4, 8 min, capped at 10.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := 30 * time.Second
	for i := 1; i < attempts && d < 10*time.Minute; i++ {
		d *= 2
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// SendPass leases due rows and drives each: sending → POST, cancelling → cancel.
func (w *Worker) SendPass(ctx context.Context) error {
	for {
		rows, err := w.orders.LeaseDue(ctx, w.now(), w.cfg.LeaseFor, 20)
		if err != nil {
			return err
		}
		for i := range rows {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			o := rows[i]
			switch o.Status {
			case domain.KitchenOrderSending:
				w.sendOne(ctx, &o)
			case domain.KitchenOrderCancelling:
				w.cancelOne(ctx, &o)
			}
		}
		if len(rows) < 20 {
			return nil
		}
	}
}

func str(s string) *string { return &s }

func (w *Worker) sendOne(ctx context.Context, o *domain.KitchenOrder) {
	wantAttempts := o.Attempts // the lease already bumped it
	now := w.now()

	// A cancel that landed after the lease but before any request went out:
	// nothing exists in the POS, so there is nothing to cancel.
	if fresh, err := w.orders.GetByID(ctx, o.ID); err == nil && fresh.Status == domain.KitchenOrderSending &&
		fresh.CancelRequestedAt != nil && !fresh.OutcomeUnknown && wantAttempts == 1 {
		fresh.Status, fresh.NextAttemptAt, fresh.CancelledAt = domain.KitchenOrderCancelled, nil, &now
		w.finish(ctx, fresh, domain.KitchenOrderSending, wantAttempts, "")
		return
	} else if err == nil && fresh.Status != domain.KitchenOrderSending {
		return // the webhook or a sweep already moved it
	} else if err == nil {
		o.CancelRequestedAt = fresh.CancelRequestedAt
	}

	if now.After(o.DeadlineAt) || wantAttempts > w.cfg.MaxAttempts {
		code := domain.KitchenErrWindowExpired
		if wantAttempts > w.cfg.MaxAttempts {
			code = domain.KitchenErrAttemptsSpent
		}
		w.giveUp(ctx, o, wantAttempts, code)
		return
	}

	var snap domain.KitchenSnapshot
	if err := json.Unmarshal(o.RequestSnapshot, &snap); err != nil {
		o.Status, o.NextAttemptAt = domain.KitchenOrderFailed, nil
		o.ErrorCode, o.LastError = str(domain.KitchenErrRejected), str("corrupt request snapshot: "+err.Error())
		w.finish(ctx, o, domain.KitchenOrderSending, wantAttempts, "failed")
		return
	}

	res := w.pos.CreateTableOrder(ctx, o.KwaakaRestaurantID, snap)
	now = w.now()
	switch res.Outcome {
	case domain.PosCreated:
		w.markCreated(ctx, o, wantAttempts, res.PosOrderID)
	case domain.PosRejected:
		if !o.OutcomeUnknown {
			o.Status, o.NextAttemptAt = domain.KitchenOrderFailed, nil
			o.ErrorCode, o.LastError = str(domain.KitchenErrRejected), str(res.Message)
			w.finish(ctx, o, domain.KitchenOrderSending, wantAttempts, "failed")
			return
		}
		// 400 after an attempt that may have landed: could be "already exists".
		w.reconcile(ctx, o, wantAttempts, res.Message)
	case domain.PosAuth:
		w.log.Error(logging.EventKwaakaOrderAuthFailed, slog.String("booking_id", o.BookingID.String()),
			slog.String("restaurant_id", o.RestaurantID.String()))
		w.retryLater(ctx, o, wantAttempts, res.Message, false)
	case domain.PosRateLimit:
		w.retryLater(ctx, o, wantAttempts, res.Message, false)
	default: // unknown: may have been applied
		w.retryLater(ctx, o, wantAttempts, res.Message, true)
	}
}

func (w *Worker) markCreated(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, posID string) {
	now := w.now()
	o.SentAt = &now
	if posID != "" {
		o.KwaakaOrderID = &posID
	}
	o.LastError, o.ErrorCode = nil, nil
	if o.CancelRequestedAt != nil { // cancel came in while sending: created → straight to cancelling
		o.Status, o.NextAttemptAt = domain.KitchenOrderCancelling, &now
	} else {
		o.Status, o.NextAttemptAt = domain.KitchenOrderSent, nil
	}
	if err := w.commit(ctx, o, domain.KitchenOrderSending, wantAttempts, ""); err != nil {
		w.afterLostCAS(ctx, o, err, posID)
		return
	}
	w.log.Info(logging.EventKwaakaOrderSent, slog.String("booking_id", o.BookingID.String()),
		slog.String("restaurant_id", o.RestaurantID.String()))
}

// afterLostCAS: somebody else moved the row while our POST was in flight (a
// webhook proved creation, the sweep flagged a cancel). The one thing we must
// not lose is the POS order id.
func (w *Worker) afterLostCAS(ctx context.Context, o *domain.KitchenOrder, err error, posID string) {
	if !errors.Is(err, errLostCAS) {
		w.log.Error("kwaaka order write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
		return
	}
	fresh, gerr := w.orders.GetByID(ctx, o.ID)
	if gerr != nil || posID == "" || fresh.KwaakaOrderID != nil {
		return
	}
	fresh.KwaakaOrderID = &posID
	if fresh.SentAt == nil {
		now := w.now()
		fresh.SentAt = &now
	}
	_, _ = w.orders.CompareAndSet(ctx, fresh, fresh.Status, fresh.Attempts)
}

// retryLater records a retryable failure and schedules the next POST with the
// SAME snapshot (same order_id, same table).
func (w *Worker) retryLater(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, msg string, unknown bool) {
	next := w.now().Add(backoff(wantAttempts))
	o.NextAttemptAt, o.LastError = &next, &msg
	if unknown {
		o.OutcomeUnknown = true
	}
	if err := w.commit(ctx, o, domain.KitchenOrderSending, wantAttempts, ""); err != nil && !errors.Is(err, errLostCAS) {
		w.log.Error("kwaaka order write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
	}
}

// giveUp ends the send window: if an attempt may have landed, ask Kwaaka before
// declaring failure.
func (w *Worker) giveUp(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, code string) {
	msg := "send window ended: " + code
	if o.LastError != nil {
		msg += " (" + *o.LastError + ")"
	}
	o.ErrorCode = &code
	if o.OutcomeUnknown {
		w.reconcile(ctx, o, wantAttempts, msg)
		return
	}
	o.Status, o.NextAttemptAt, o.LastError = domain.KitchenOrderFailed, nil, &msg
	w.finish(ctx, o, domain.KitchenOrderSending, wantAttempts, "failed")
}

// reconcile asks Kwaaka whether the order exists: found → sent, definitively
// absent → failed, cannot tell → failed_unknown. Never a blind re-create.
func (w *Worker) reconcile(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, why string) {
	id := o.ID.String()
	if o.KwaakaOrderID != nil {
		id = *o.KwaakaOrderID
	}
	got, err := w.pos.GetOrder(ctx, o.KwaakaRestaurantID, id)
	switch {
	case err == nil:
		posID := got.ID
		w.markCreated(ctx, o, wantAttempts, posID)
	case errors.Is(err, domain.ErrNotFound):
		o.Status, o.NextAttemptAt = domain.KitchenOrderFailed, nil
		o.ErrorCode, o.LastError = str(domain.KitchenErrRejected), str(why)
		w.finish(ctx, o, domain.KitchenOrderSending, wantAttempts, "failed")
	default:
		msg := fmt.Sprintf("%s; could not verify in POS: %v", why, err)
		o.Status, o.NextAttemptAt = domain.KitchenOrderFailedUnknown, nil
		o.ErrorCode, o.LastError = str(domain.KitchenErrUnknownOutcome), &msg
		w.finish(ctx, o, domain.KitchenOrderSending, wantAttempts, "failed_unknown")
	}
}

// finish commits a terminal transition and logs it.
func (w *Worker) finish(ctx context.Context, o *domain.KitchenOrder, want domain.KitchenOrderStatus, wantAttempts int, alertReason string) {
	if err := w.commit(ctx, o, want, wantAttempts, alertReason); err != nil {
		if !errors.Is(err, errLostCAS) {
			w.log.Error("kwaaka order write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
		}
		return
	}
	switch o.Status {
	case domain.KitchenOrderFailed, domain.KitchenOrderFailedUnknown:
		w.log.Warn(logging.EventKwaakaOrderFailed, slog.String("booking_id", o.BookingID.String()),
			slog.String("restaurant_id", o.RestaurantID.String()), slog.String("status", string(o.Status)))
	case domain.KitchenOrderCancelled:
		w.log.Info(logging.EventKwaakaOrderCancelled, slog.String("booking_id", o.BookingID.String()),
			slog.String("restaurant_id", o.RestaurantID.String()))
	case domain.KitchenOrderCancelFailed:
		w.log.Warn(logging.EventKwaakaOrderCancelFailed, slog.String("booking_id", o.BookingID.String()),
			slog.String("restaurant_id", o.RestaurantID.String()))
	}
}

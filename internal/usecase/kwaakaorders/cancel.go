package kwaakaorders

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newID() uuid.UUID { return uuid.New() }

// cancelWindow is how long we keep trying to cancel in the POS.
const cancelWindow = 2 * time.Hour

const cancelReason = "booking_cancelled"

// CancelSweep turns "booking is cancelled" into a cancel request on the order.
// It reads state rather than catching an event, so a missed event cannot leave
// an order sent-and-forgotten. no_show / completed are not selected at all.
func (w *Worker) CancelSweep(ctx context.Context) error {
	rows, err := w.orders.ListCancelledPending(ctx, 100)
	if err != nil {
		return err
	}
	now := w.now()
	for i := range rows {
		o := rows[i]
		want, attempts := o.Status, o.Attempts
		o.CancelRequestedAt = &now
		o.CancelReason = str(cancelReason)
		switch {
		case o.Status == domain.KitchenOrderSent:
			o.Status, o.NextAttemptAt = domain.KitchenOrderCancelling, &now
		case o.Status == domain.KitchenOrderSending && o.Attempts == 0:
			// Nothing ever left for the POS.
			o.Status, o.NextAttemptAt, o.CancelledAt = domain.KitchenOrderCancelled, nil, &now
		default:
			// sending with attempts: the send outcome will route "created" to cancelling.
		}
		w.finish(ctx, &o, want, attempts, "")
	}
	return nil
}

// cancelOne posts order/cancel for a leased cancelling row.
func (w *Worker) cancelOne(ctx context.Context, o *domain.KitchenOrder) {
	wantAttempts := o.Attempts
	now := w.now()
	start := now
	if o.CancelRequestedAt != nil {
		start = *o.CancelRequestedAt
	}
	if now.After(start.Add(cancelWindow)) || wantAttempts > w.cfg.MaxAttempts {
		w.cancelFailed(ctx, o, wantAttempts, domain.KitchenErrCancelWindow, "cancel window ended")
		return
	}
	posID := o.ID.String()
	if o.KwaakaOrderID != nil {
		posID = *o.KwaakaOrderID
	}
	res := w.pos.CancelTableOrder(ctx, o.KwaakaRestaurantID, posID, cancelReason)
	switch res.Outcome {
	case domain.PosCreated:
		o.Status, o.NextAttemptAt, o.CancelledAt, o.TableReleasedAt = domain.KitchenOrderCancelled, nil, &now, &now
		w.finish(ctx, o, domain.KitchenOrderCancelling, wantAttempts, "")
	case domain.PosRejected:
		// 400/404: gone already, or the POS refuses. Look.
		got, err := w.pos.GetOrder(ctx, o.KwaakaRestaurantID, posID)
		switch {
		case errors.Is(err, domain.ErrNotFound) || (err == nil && got.State == domain.PosStateDeleted):
			o.Status, o.NextAttemptAt, o.CancelledAt, o.TableReleasedAt = domain.KitchenOrderCancelled, nil, &now, &now
			w.finish(ctx, o, domain.KitchenOrderCancelling, wantAttempts, "")
		case err == nil:
			w.cancelFailed(ctx, o, wantAttempts, domain.KitchenErrCancelRefused, res.Message)
		default:
			w.cancelRetry(ctx, o, wantAttempts, res.Message)
		}
	default: // auth, rate limit, unknown: cancel is safe to repeat
		w.cancelRetry(ctx, o, wantAttempts, res.Message)
	}
}

func (w *Worker) cancelRetry(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, msg string) {
	next := w.now().Add(backoff(wantAttempts))
	o.NextAttemptAt, o.LastError = &next, &msg
	if err := w.commit(ctx, o, domain.KitchenOrderCancelling, wantAttempts, ""); err != nil && !errors.Is(err, errLostCAS) {
		w.log.Error("kwaaka cancel write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
	}
}

func (w *Worker) cancelFailed(ctx context.Context, o *domain.KitchenOrder, wantAttempts int, code, msg string) {
	o.Status, o.NextAttemptAt = domain.KitchenOrderCancelFailed, nil
	o.ErrorCode, o.LastError = &code, &msg
	// The booking IS cancelled here, so the generic "no alert for a cancelled
	// booking" rule must not apply: this alert is exactly for that case.
	err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
		ok, err := w.orders.CompareAndSet(ctx, o, domain.KitchenOrderCancelling, wantAttempts)
		if err != nil {
			return err
		}
		if !ok {
			return errLostCAS
		}
		return w.alert(ctx, o, "cancel_failed", msg)
	})
	if err != nil && !errors.Is(err, errLostCAS) {
		w.log.Error("kwaaka cancel write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
		return
	}
	if err == nil {
		w.log.Warn("kwaaka_order.cancel_failed", slog.String("booking_id", o.BookingID.String()),
			slog.String("restaurant_id", o.RestaurantID.String()))
	}
}

// RescheduleSweep raises ONE alert when a sent booking's start moved, and
// refreshes the snapshot time in the same transaction so it never repeats.
func (w *Worker) RescheduleSweep(ctx context.Context) error {
	rows, err := w.orders.ListRescheduled(ctx, 100)
	if err != nil {
		return err
	}
	for i := range rows {
		o := rows[i].Order
		want, attempts := o.Status, o.Attempts
		o.BookingStartsAt = rows[i].NewStartsAt
		err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
			ok, err := w.orders.CompareAndSet(ctx, &o, want, attempts)
			if err != nil {
				return err
			}
			if !ok {
				return errLostCAS
			}
			return w.alert(ctx, &o, "rescheduled", "")
		})
		if err != nil && !errors.Is(err, errLostCAS) {
			w.log.Error("kwaaka reschedule write failed", slog.String("booking_id", o.BookingID.String()), slog.String("error", err.Error()))
		}
	}
	return nil
}

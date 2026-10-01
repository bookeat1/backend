package kwaakaorders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// errUnknownOrder: no kitchen order matches the id in a status message.
var errUnknownOrder = errors.New("kwaaka status: unknown order")

// ApplyPOSStatus is the ONE function that turns a POS status (webhook or poll)
// into a change of a kitchen order. It never touches the booking, its items,
// payments or money. Must run inside the caller's transaction.
//
// Rules (plan §2.8): pos_state only grows (open < bill_printed < closed =
// deleted); an equal or lower rank is stale and writes nothing; a `sending` row
// proven by a status becomes `sent`; `deleted` on `cancelling` finishes the
// cancel; `deleted` on a live booking raises exactly one pos_cancelled alert
// (monotonicity makes it once); closed/deleted or a status listing other tables
// releases our table.
func (w *Worker) ApplyPOSStatus(ctx context.Context, ev domain.KwaakaOrderStatusEvent, source string) (outcome string, orderID *uuid.UUID, err error) {
	o, err := w.findOrder(ctx, ev.OrderID)
	if err != nil {
		return "", nil, err
	}
	now := w.now()
	id := o.ID
	want, attempts := o.Status, o.Attempts

	raw := ev.Raw
	o.PosStatusRaw, o.PosStatusAt, o.PosStatusSource = &raw, &now, &source

	if ev.State == domain.PosStateUnknown {
		// Unfamiliar word: keep it, apply nothing.
		return domain.WebhookOutcomeUnknownStatus, &id, w.cas(ctx, o, want, attempts)
	}
	if o.PosState != nil && ev.State.Rank() <= o.PosState.Rank() {
		return domain.WebhookOutcomeStale, &id, nil
	}
	state := ev.State
	o.PosState = &state

	if o.KwaakaOrderID == nil && ev.OrderID != o.ID.String() {
		posID := ev.OrderID
		o.KwaakaOrderID = &posID
	}
	if o.Status == domain.KitchenOrderSending {
		// Any status proves the order exists.
		o.SentAt = &now
		o.NextAttemptAt = nil
		o.Status = domain.KitchenOrderSent
		if o.CancelRequestedAt != nil {
			o.Status, o.NextAttemptAt = domain.KitchenOrderCancelling, &now
		}
	}
	alertPosCancelled := false
	if state == domain.PosStateDeleted {
		switch {
		case o.Status == domain.KitchenOrderCancelling, o.Status == domain.KitchenOrderSent && o.CancelRequestedAt != nil:
			o.Status, o.NextAttemptAt, o.CancelledAt = domain.KitchenOrderCancelled, nil, &now
		case o.Status == domain.KitchenOrderSent:
			alertPosCancelled = true
		}
	}
	if state.Terminal() || (len(ev.TableIDs) > 0 && !contains(ev.TableIDs, o.KwaakaTableID)) {
		if o.TableReleasedAt == nil {
			o.TableReleasedAt = &now
		}
	}
	if err := w.cas(ctx, o, want, attempts); err != nil {
		return "", nil, err
	}
	if alertPosCancelled {
		if err := w.alert(ctx, o, "pos_cancelled", ""); err != nil {
			return "", nil, err
		}
	}
	return domain.WebhookOutcomeApplied, &id, nil
}

func (w *Worker) cas(ctx context.Context, o *domain.KitchenOrder, want domain.KitchenOrderStatus, attempts int) error {
	ok, err := w.orders.CompareAndSet(ctx, o, want, attempts)
	if err != nil {
		return err
	}
	if !ok {
		return errLostCAS
	}
	return nil
}

func (w *Worker) findOrder(ctx context.Context, id string) (*domain.KitchenOrder, error) {
	if u, perr := uuid.Parse(id); perr == nil {
		o, err := w.orders.GetByID(ctx, u)
		if err == nil {
			return o, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
	}
	o, err := w.orders.GetByPosOrderID(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, errUnknownOrder
	}
	if err != nil {
		return nil, fmt.Errorf("find kitchen order: %w", err)
	}
	return o, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

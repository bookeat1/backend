// Package kwaakaorder is the Postgres implementation of the Kwaaka phase 2
// repositories: kitchen orders, per-venue order settings and the webhook inbox
// (migration 0118).
package kwaakaorder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Orders implements domain.KitchenOrderRepository.
type Orders struct{ pool sqltx.Querier }

// NewOrders builds the kitchen-order repository.
func NewOrders(pool sqltx.Querier) *Orders { return &Orders{pool: pool} }

var _ domain.KitchenOrderRepository = (*Orders)(nil)

const orderCols = `id, booking_id, restaurant_id, kwaaka_restaurant_id, kwaaka_table_id, table_shared,
	kwaaka_order_id, status, trigger, paid, paid_amount_minor, total_minor, partial, request_snapshot,
	booking_starts_at, deadline_at, table_hold_until, table_released_at, attempts, outcome_unknown,
	next_attempt_at, last_error, error_code, sent_at, cancel_requested_at, cancelled_at, cancel_reason,
	pos_status_raw, pos_state, pos_status_at, pos_status_source, created_at, updated_at`

func scanOrder(row pgx.Row) (*domain.KitchenOrder, error) {
	var o domain.KitchenOrder
	var status, trigger string
	var posState *string
	err := row.Scan(&o.ID, &o.BookingID, &o.RestaurantID, &o.KwaakaRestaurantID, &o.KwaakaTableID, &o.TableShared,
		&o.KwaakaOrderID, &status, &trigger, &o.Paid, &o.PaidAmountMinor, &o.TotalMinor, &o.Partial, &o.RequestSnapshot,
		&o.BookingStartsAt, &o.DeadlineAt, &o.TableHoldUntil, &o.TableReleasedAt, &o.Attempts, &o.OutcomeUnknown,
		&o.NextAttemptAt, &o.LastError, &o.ErrorCode, &o.SentAt, &o.CancelRequestedAt, &o.CancelledAt, &o.CancelReason,
		&o.PosStatusRaw, &posState, &o.PosStatusAt, &o.PosStatusSource, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, err
	}
	o.Status = domain.KitchenOrderStatus(status)
	o.Trigger = domain.KitchenOrderTrigger(trigger)
	if posState != nil {
		p := domain.PosOrderState(*posState)
		o.PosState = &p
	}
	return &o, nil
}

func posStateArg(p *domain.PosOrderState) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

func (r *Orders) collect(rows pgx.Rows) ([]domain.KitchenOrder, error) {
	defer rows.Close()
	var out []domain.KitchenOrder
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (r *Orders) one(ctx context.Context, where string, arg any) (*domain.KitchenOrder, error) {
	o, err := scanOrder(sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+orderCols+` FROM kwaaka_kitchen_orders WHERE `+where, arg))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("kitchen order: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("kitchen order: %w", err)
	}
	return o, nil
}

func (r *Orders) GetByID(ctx context.Context, id uuid.UUID) (*domain.KitchenOrder, error) {
	return r.one(ctx, `id = $1`, id)
}

func (r *Orders) GetByBookingID(ctx context.Context, id uuid.UUID) (*domain.KitchenOrder, error) {
	return r.one(ctx, `booking_id = $1`, id)
}

func (r *Orders) GetByPosOrderID(ctx context.Context, posID string) (*domain.KitchenOrder, error) {
	return r.one(ctx, `kwaaka_order_id = $1`, posID)
}

// Insert stores the row; ON CONFLICT (booking_id) DO NOTHING is "already there",
// not an error — two racing claims produce exactly one row.
func (r *Orders) Insert(ctx context.Context, o *domain.KitchenOrder) (bool, error) {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO kwaaka_kitchen_orders
		   (id, booking_id, restaurant_id, kwaaka_restaurant_id, kwaaka_table_id, table_shared, status, trigger,
		    paid, paid_amount_minor, total_minor, partial, request_snapshot, booking_starts_at, deadline_at,
		    table_hold_until, next_attempt_at, last_error, error_code)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		 ON CONFLICT (booking_id) DO NOTHING`,
		o.ID, o.BookingID, o.RestaurantID, o.KwaakaRestaurantID, o.KwaakaTableID, o.TableShared, string(o.Status),
		string(o.Trigger), o.Paid, o.PaidAmountMinor, o.TotalMinor, o.Partial, []byte(o.RequestSnapshot),
		o.BookingStartsAt, o.DeadlineAt, o.TableHoldUntil, o.NextAttemptAt, o.LastError, o.ErrorCode)
	if err != nil {
		return false, fmt.Errorf("insert kitchen order: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Orders) ListCandidates(ctx context.Context, now time.Time, limit int) ([]domain.KitchenCandidate, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT b.id, b.restaurant_id, s.kwaaka_restaurant_id
		   FROM bookings b
		   JOIN restaurant_kwaaka_order_settings s ON s.restaurant_id = b.restaurant_id AND s.orders_enabled
		   JOIN restaurants r ON r.id = b.restaurant_id AND r.kwaaka_restaurant_id = s.kwaaka_restaurant_id
		  WHERE b.status IN ('confirmed','arrived')
		    AND b.starts_at BETWEEN $1 AND $2
		    AND EXISTS (SELECT 1 FROM restaurant_kwaaka_pool_tables t WHERE t.restaurant_id = b.restaurant_id)
		    AND NOT EXISTS (SELECT 1 FROM kwaaka_kitchen_orders k WHERE k.booking_id = b.id)
		    AND EXISTS (SELECT 1 FROM booking_items i WHERE i.booking_id = b.id AND i.status <> 'cancelled')
		  ORDER BY b.starts_at
		  LIMIT $3`, now.Add(-12*time.Hour), now.Add(12*time.Hour), limit)
	if err != nil {
		return nil, fmt.Errorf("list kitchen candidates: %w", err)
	}
	defer rows.Close()
	var out []domain.KitchenCandidate
	for rows.Next() {
		var c domain.KitchenCandidate
		if err := rows.Scan(&c.BookingID, &c.RestaurantID, &c.KwaakaRestaurantID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Orders) LockClaimSubject(ctx context.Context, bookingID uuid.UUID) (*domain.KitchenClaimSubject, error) {
	q := sqltx.From(ctx, r.pool)
	var s domain.KitchenClaimSubject
	var status string
	var tz, kw *string
	err := q.QueryRow(ctx,
		`SELECT b.id, b.restaurant_id, b.status, b.name, b.phone, b.guests, b.starts_at, b.ends_at,
		        b.confirmed_at, b.arrived_at, r.timezone, r.kwaaka_restaurant_id
		   FROM bookings b JOIN restaurants r ON r.id = b.restaurant_id
		  WHERE b.id = $1 FOR UPDATE OF b`, bookingID).
		Scan(&s.BookingID, &s.RestaurantID, &status, &s.Name, &s.Phone, &s.Guests, &s.StartsAt, &s.EndsAt,
			&s.ConfirmedAt, &s.ArrivedAt, &tz, &kw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("claim subject: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("claim subject: %w", err)
	}
	s.Status = domain.BookingStatus(status)
	if tz != nil {
		s.Timezone = *tz
	}
	if kw != nil {
		s.KwaakaID = *kw
	}

	rows, err := q.Query(ctx,
		`SELECT i.item_name, i.item_price_minor, i.currency, i.quantity, i.comment, m.kwaaka_product_id
		   FROM booking_items i LEFT JOIN menu_items m ON m.id = i.menu_item_id
		  WHERE i.booking_id = $1 AND i.status <> 'cancelled'
		  ORDER BY i.created_at, i.id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("claim subject items: %w", err)
	}
	for rows.Next() {
		var it domain.KitchenClaimItem
		if err := rows.Scan(&it.Name, &it.PriceMinor, &it.Currency, &it.Quantity, &it.Comment, &it.KwaakaProductID); err != nil {
			rows.Close()
			return nil, err
		}
		s.Items = append(s.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Paid = a preorder payment in status exactly 'captured' (partially_refunded
	// / refunded are NOT paid). If several, the latest capture.
	err = q.QueryRow(ctx,
		`SELECT amount_minor, captured_at FROM payments
		  WHERE booking_id = $1 AND purpose = 'preorder' AND status = 'captured'
		  ORDER BY captured_at DESC NULLS LAST LIMIT 1`, bookingID).Scan(&s.PaidMinor, &s.CapturedAt)
	switch {
	case err == nil:
		s.Paid = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return nil, fmt.Errorf("claim subject payment: %w", err)
	}
	return &s, nil
}

func (r *Orders) LeaseDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]domain.KitchenOrder, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`UPDATE kwaaka_kitchen_orders SET attempts = attempts + 1, next_attempt_at = $2, updated_at = now()
		  WHERE id IN (SELECT id FROM kwaaka_kitchen_orders
		                WHERE status IN ('sending','cancelling') AND next_attempt_at <= $1
		                ORDER BY next_attempt_at
		                LIMIT $3 FOR UPDATE SKIP LOCKED)
		  RETURNING `+orderCols, now, now.Add(lease), limit)
	if err != nil {
		return nil, fmt.Errorf("lease kitchen orders: %w", err)
	}
	return r.collect(rows)
}

func (r *Orders) CompareAndSet(ctx context.Context, o *domain.KitchenOrder, want domain.KitchenOrderStatus, wantAttempts int) (bool, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE kwaaka_kitchen_orders SET
		   kwaaka_table_id=$4, table_shared=$5, kwaaka_order_id=$6, status=$7, partial=$8, table_released_at=$9,
		   outcome_unknown=$10, next_attempt_at=$11, last_error=$12, error_code=$13, sent_at=$14,
		   cancel_requested_at=$15, cancelled_at=$16, cancel_reason=$17, pos_status_raw=$18, pos_state=$19,
		   pos_status_at=$20, pos_status_source=$21, booking_starts_at=$22, attempts=$23, updated_at=now()
		 WHERE id=$1 AND status=$2 AND attempts=$3`,
		o.ID, string(want), wantAttempts,
		o.KwaakaTableID, o.TableShared, o.KwaakaOrderID, string(o.Status), o.Partial, o.TableReleasedAt,
		o.OutcomeUnknown, o.NextAttemptAt, o.LastError, o.ErrorCode, o.SentAt,
		o.CancelRequestedAt, o.CancelledAt, o.CancelReason, o.PosStatusRaw, posStateArg(o.PosState),
		o.PosStatusAt, o.PosStatusSource, o.BookingStartsAt, o.Attempts)
	if err != nil {
		return false, fmt.Errorf("cas kitchen order: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Orders) TableLoads(ctx context.Context, restaurantID uuid.UUID, now time.Time) (domain.KitchenTableLoad, error) {
	out := domain.KitchenTableLoad{Inflight: map[string]int{}, Live: map[string]int{}}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT kwaaka_table_id,
		        count(*) FILTER (WHERE status = 'sending' OR (status = 'sent' AND sent_at > $2 - interval '5 minutes')),
		        count(*)
		   FROM kwaaka_kitchen_orders
		  WHERE restaurant_id = $1 AND status IN ('sending','sent','cancelling','failed_unknown')
		    AND (pos_state IS NULL OR pos_state NOT IN ('closed','deleted'))
		    AND table_released_at IS NULL AND table_hold_until > $2
		  GROUP BY kwaaka_table_id`, restaurantID, now)
	if err != nil {
		return out, fmt.Errorf("kitchen table loads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var inflight, live int
		if err := rows.Scan(&id, &inflight, &live); err != nil {
			return out, err
		}
		out.Inflight[id], out.Live[id] = inflight, live
	}
	return out, rows.Err()
}

func (r *Orders) ListCancelledPending(ctx context.Context, limit int) ([]domain.KitchenOrder, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+prefixed("k")+` FROM kwaaka_kitchen_orders k JOIN bookings b ON b.id = k.booking_id
		  WHERE k.status IN ('sending','sent') AND b.status = 'cancelled' AND k.cancel_requested_at IS NULL
		  ORDER BY k.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list cancelled kitchen orders: %w", err)
	}
	return r.collect(rows)
}

func (r *Orders) ListRescheduled(ctx context.Context, limit int) ([]domain.KitchenReschedule, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+prefixed("k")+`, b.starts_at FROM kwaaka_kitchen_orders k JOIN bookings b ON b.id = k.booking_id
		  WHERE k.status = 'sent' AND b.status IN ('confirmed','arrived') AND b.starts_at <> k.booking_starts_at
		  ORDER BY k.created_at LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list rescheduled kitchen orders: %w", err)
	}
	defer rows.Close()
	var out []domain.KitchenReschedule
	for rows.Next() {
		var o domain.KitchenOrder
		var status, trigger string
		var posState *string
		var ns time.Time
		if err := rows.Scan(&o.ID, &o.BookingID, &o.RestaurantID, &o.KwaakaRestaurantID, &o.KwaakaTableID, &o.TableShared,
			&o.KwaakaOrderID, &status, &trigger, &o.Paid, &o.PaidAmountMinor, &o.TotalMinor, &o.Partial, &o.RequestSnapshot,
			&o.BookingStartsAt, &o.DeadlineAt, &o.TableHoldUntil, &o.TableReleasedAt, &o.Attempts, &o.OutcomeUnknown,
			&o.NextAttemptAt, &o.LastError, &o.ErrorCode, &o.SentAt, &o.CancelRequestedAt, &o.CancelledAt, &o.CancelReason,
			&o.PosStatusRaw, &posState, &o.PosStatusAt, &o.PosStatusSource, &o.CreatedAt, &o.UpdatedAt, &ns); err != nil {
			return nil, err
		}
		o.Status, o.Trigger = domain.KitchenOrderStatus(status), domain.KitchenOrderTrigger(trigger)
		if posState != nil {
			p := domain.PosOrderState(*posState)
			o.PosState = &p
		}
		out = append(out, domain.KitchenReschedule{Order: o, NewStartsAt: ns})
	}
	return out, rows.Err()
}

func (r *Orders) ListPollDue(ctx context.Context, now, staleBefore time.Time, limit int) ([]domain.KitchenOrder, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+orderCols+` FROM kwaaka_kitchen_orders
		  WHERE status IN ('sent','cancelling')
		    AND (pos_state IS NULL OR pos_state NOT IN ('closed','deleted'))
		    AND sent_at > $1 - interval '12 hours'
		    AND (pos_status_at IS NULL OR pos_status_at < $2)
		  ORDER BY COALESCE(pos_status_at, sent_at) LIMIT $3`, now, staleBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list poll-due kitchen orders: %w", err)
	}
	return r.collect(rows)
}

func (r *Orders) ListByBookingIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.KitchenOrderView, error) {
	out := make(map[uuid.UUID]domain.KitchenOrderView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT k.booking_id, k.status, k.sent_at, k.pos_state, k.error_code, k.partial, COALESCE(t.label, '')
		   FROM kwaaka_kitchen_orders k
		   LEFT JOIN restaurant_kwaaka_pool_tables t
		          ON t.restaurant_id = k.restaurant_id AND t.kwaaka_table_id = k.kwaaka_table_id
		  WHERE k.booking_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("kitchen order views: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var v domain.KitchenOrderView
		var status string
		var pos *string
		if err := rows.Scan(&id, &status, &v.SentAt, &pos, &v.ErrorCode, &v.Partial, &v.TableLabel); err != nil {
			return nil, err
		}
		v.Status = domain.KitchenOrderStatus(status)
		if pos != nil {
			p := domain.PosOrderState(*pos)
			v.PosState = &p
		}
		out[id] = v
	}
	return out, rows.Err()
}

// prefixed returns orderCols with every column qualified by alias.
func prefixed(alias string) string {
	cols := []string{"id", "booking_id", "restaurant_id", "kwaaka_restaurant_id", "kwaaka_table_id", "table_shared",
		"kwaaka_order_id", "status", "trigger", "paid", "paid_amount_minor", "total_minor", "partial", "request_snapshot",
		"booking_starts_at", "deadline_at", "table_hold_until", "table_released_at", "attempts", "outcome_unknown",
		"next_attempt_at", "last_error", "error_code", "sent_at", "cancel_requested_at", "cancelled_at", "cancel_reason",
		"pos_status_raw", "pos_state", "pos_status_at", "pos_status_source", "created_at", "updated_at"}
	s := ""
	for i, c := range cols {
		if i > 0 {
			s += ", "
		}
		s += alias + "." + c
	}
	return s
}

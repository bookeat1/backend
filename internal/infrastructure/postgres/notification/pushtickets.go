package notification

import (
	"context"
	"fmt"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// PushTickets implements domain.PushTicketRepository over push_tickets
// (migration 0102) — the 24-hour poll queue for mobile push receipts.
type PushTickets struct{ pool sqltx.Querier }

// NewPushTickets builds the push-ticket repository.
func NewPushTickets(pool sqltx.Querier) *PushTickets { return &PushTickets{pool: pool} }

var _ domain.PushTicketRepository = (*PushTickets)(nil)

// Record enqueues an accepted ticket. ON CONFLICT DO NOTHING makes it
// idempotent: a resend that repeats a ticket id (or a retry after a crash
// between the send and this write) must not enqueue a second poll, and must not
// resurrect a ticket that has already been resolved.
func (r *PushTickets) Record(ctx context.Context, t domain.PushTicket) error {
	if t.ID == "" {
		return fmt.Errorf("record push ticket: empty ticket id")
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO push_tickets (ticket_id, device_token_id, outbox_event_id, created_at)
		 VALUES ($1,$2,$3, now())
		 ON CONFLICT (ticket_id) DO NOTHING`,
		t.ID, t.DeviceTokenID, t.OutboxEventID); err != nil {
		return fmt.Errorf("record push ticket: %w", err)
	}
	return nil
}

// ListUnresolved returns the oldest tickets still awaiting a receipt that are
// old enough for the provider to have one. The predicate matches
// idx_push_tickets_unresolved exactly.
func (r *PushTickets) ListUnresolved(ctx context.Context, createdBefore time.Time, limit int) ([]domain.PushTicket, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT ticket_id, device_token_id, outbox_event_id, created_at, resolved_at
		   FROM push_tickets
		  WHERE resolved_at IS NULL AND created_at <= $1
		  ORDER BY created_at, ticket_id
		  LIMIT $2`, createdBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list unresolved push tickets: %w", err)
	}
	defer rows.Close()
	var out []domain.PushTicket
	for rows.Next() {
		var t domain.PushTicket
		if err := rows.Scan(&t.ID, &t.DeviceTokenID, &t.OutboxEventID, &t.CreatedAt, &t.ResolvedAt); err != nil {
			return nil, fmt.Errorf("list unresolved push tickets: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Resolve marks the answered tickets done. `resolved_at IS NULL` in the
// predicate keeps it idempotent: a re-poll of a ticket that was already closed
// never rewrites its timestamp.
func (r *PushTickets) Resolve(ctx context.Context, ticketIDs []string, at time.Time) error {
	if len(ticketIDs) == 0 {
		return nil
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_tickets SET resolved_at = $2
		  WHERE ticket_id = ANY($1) AND resolved_at IS NULL`,
		ticketIDs, at); err != nil {
		return fmt.Errorf("resolve push tickets: %w", err)
	}
	return nil
}

// ExpireOlderThan force-resolves tickets the provider no longer has a receipt
// for (Expo clears them after 24 hours). Without it every such ticket would be
// asked about on every tick forever and the table would only ever grow.
func (r *PushTickets) ExpireOlderThan(ctx context.Context, cutoff time.Time, at time.Time) (int64, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_tickets SET resolved_at = $2
		  WHERE resolved_at IS NULL AND created_at < $1`, cutoff, at)
	if err != nil {
		return 0, fmt.Errorf("expire push tickets: %w", err)
	}
	return tag.RowsAffected(), nil
}

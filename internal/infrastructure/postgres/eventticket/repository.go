// Package eventticket is the Postgres implementation of
// domain.EventTicketRepository. Same layering and conventions as
// internal/infrastructure/postgres/payment — read that package first.
//
// The one rule that matters most: capacity is never oversold. The purchase
// usecase runs LockEventForCapacity → SoldCount → Create inside ONE TxManager
// transaction; the row lock on the event serialises concurrent buyers of the
// same event so the SoldCount they read already includes any pending sibling
// reservation, and the last-seat race resolves to exactly one winner. The
// (event_id, purchase_idempotency_key) unique index is the second, DB-enforced
// line of defence against a retried purchase reserving a second batch of seats.
package eventticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	defaultPerPage      = 20
	maxPerPage          = 100
)

// Repository implements domain.EventTicketRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the event-ticket repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.EventTicketRepository = (*Repository)(nil)

const cols = `id, event_id, restaurant_id, user_id, guest_name, guest_phone, guest_email,
	quantity, unit_price_minor, total_minor, currency, status, payment_id,
	purchase_idempotency_key, created_at, updated_at`

// LockEventForCapacity takes a FOR UPDATE row lock on the event. Every buyer of
// the same event blocks here until the previous one's transaction commits, so
// the SoldCount read that follows is race-safe. Must run inside a transaction.
func (r *Repository) LockEventForCapacity(ctx context.Context, eventID uuid.UUID) error {
	var id uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock event for capacity: %w", err)
	}
	return nil
}

// SoldCount sums quantity over tickets that hold capacity (pending + paid).
func (r *Repository) SoldCount(ctx context.Context, eventID uuid.UUID) (int, error) {
	var sold int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COALESCE(SUM(quantity), 0) FROM event_tickets
		 WHERE event_id = $1 AND status IN ('pending', 'paid')`, eventID).Scan(&sold)
	if err != nil {
		return 0, fmt.Errorf("sold count: %w", err)
	}
	return sold, nil
}

// Create inserts a new ticket. A duplicate (event_id, purchase_idempotency_key)
// maps to ErrAlreadyExists; an unknown event/restaurant FK maps to ErrNotFound.
func (r *Repository) Create(ctx context.Context, t *domain.EventTicket) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.Currency == "" {
		t.Currency = domain.CurrencyKZT
	}
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO event_tickets (`+cols+`) VALUES
		 ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		t.ID, t.EventID, t.RestaurantID, t.UserID, t.GuestName, t.GuestPhone, t.GuestEmail,
		t.Quantity, t.UnitPriceMinor, t.TotalMinor, string(t.Currency), string(t.Status), t.PaymentID,
		t.PurchaseIdempotencyKey, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case uniqueViolation:
				return fmt.Errorf("%w: create event ticket", domain.ErrAlreadyExists)
			case foreignKeyViolation:
				return fmt.Errorf("%w: create event ticket (unknown event or restaurant)", domain.ErrNotFound)
			}
		}
		return fmt.Errorf("create event ticket: %w", err)
	}
	return nil
}

// GetByID returns a ticket by id regardless of status.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.EventTicket, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+` FROM event_tickets WHERE id = $1`, id)
	return scanOne(row, "get event ticket")
}

// GetByIdempotencyKey resolves a buyer's retry token for an event.
func (r *Repository) GetByIdempotencyKey(ctx context.Context, eventID uuid.UUID, key string) (*domain.EventTicket, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM event_tickets WHERE event_id = $1 AND purchase_idempotency_key = $2`, eventID, key)
	return scanOne(row, "get event ticket by idempotency key")
}

// SetPaymentID backfills the payment link once the payment intent exists.
func (r *Repository) SetPaymentID(ctx context.Context, id, paymentID uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_tickets SET payment_id = $2, updated_at = now() WHERE id = $1`, id, paymentID)
	if err != nil {
		return fmt.Errorf("set event ticket payment id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// CompareAndSwapStatus is the DB-level guard for a ticket status change.
func (r *Repository) CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to domain.TicketStatus, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_tickets SET status = $3, updated_at = $4 WHERE id = $1 AND status = $2`,
		id, string(from), string(to), at)
	if err != nil {
		return fmt.Errorf("cas event ticket status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Zero rows: either the id is absent or the ticket already moved away
		// from `from`. Disambiguate with one follow-up read purely for the error
		// class (same non-anti-pattern as the payments CAS classifyMiss — the
		// write already happened atomically first).
		var exists bool
		if e := sqltx.From(ctx, r.pool).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM event_tickets WHERE id = $1)`, id).Scan(&exists); e != nil {
			return fmt.Errorf("cas event ticket status classify: %w", e)
		}
		if !exists {
			return domain.ErrNotFound
		}
		return fmt.Errorf("%w: event ticket %s is not in status %s", domain.ErrAlreadyExists, id, from)
	}
	return nil
}

// ListByEvent returns an event's tickets (optionally status-filtered), newest
// first with id as a stable tie-breaker, paginated, plus the total.
func (r *Repository) ListByEvent(ctx context.Context, eventID uuid.UUID, statuses []domain.TicketStatus, pg, perPage int) ([]domain.EventTicket, int, error) {
	limit, offset := paginate(pg, perPage)
	args := []any{eventID}
	where := "event_id = $1"
	if len(statuses) > 0 {
		where += " AND status = ANY($2)"
		args = append(args, statusStrings(statuses))
	}
	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COUNT(*) FROM event_tickets WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count event tickets: %w", err)
	}
	args = append(args, limit, offset)
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM event_tickets WHERE `+where+
			` ORDER BY created_at DESC, id DESC LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list event tickets: %w", err)
	}
	list, err := scanMany(rows)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListByUser returns a guest's own tickets, newest first, paginated.
func (r *Repository) ListByUser(ctx context.Context, userID uuid.UUID, pg, perPage int) ([]domain.EventTicket, int, error) {
	limit, offset := paginate(pg, perPage)
	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COUNT(*) FROM event_tickets WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count user tickets: %w", err)
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM event_tickets WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list user tickets: %w", err)
	}
	list, err := scanMany(rows)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListStalePending returns up to limit pending tickets older than before,
// oldest first — the sweep input (backed by idx_event_tickets_event_status /
// the created_at scan).
func (r *Repository) ListStalePending(ctx context.Context, before time.Time, limit int) ([]domain.EventTicket, error) {
	if limit <= 0 {
		limit = defaultPerPage
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM event_tickets
		 WHERE status = 'pending' AND created_at < $1
		 ORDER BY created_at ASC, id ASC LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list stale pending tickets: %w", err)
	}
	return scanMany(rows)
}

// Counts returns the admin "tickets sold" aggregate for an event. All figures
// are computed SQL-side (never a Go loop over full rows); revenue is summed
// from stored minor units over paid rows only, never recomputed.
func (r *Repository) Counts(ctx context.Context, eventID uuid.UUID) (domain.EventTicketCounts, error) {
	c := domain.EventTicketCounts{EventID: eventID, Currency: domain.CurrencyKZT}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT
			COUNT(*) FILTER (WHERE status = 'pending'),
			COALESCE(SUM(quantity) FILTER (WHERE status = 'pending'), 0),
			COUNT(*) FILTER (WHERE status = 'paid'),
			COALESCE(SUM(quantity) FILTER (WHERE status = 'paid'), 0),
			COUNT(*) FILTER (WHERE status = 'refunded'),
			COUNT(*) FILTER (WHERE status = 'cancelled'),
			COALESCE(SUM(total_minor) FILTER (WHERE status = 'paid'), 0)
		 FROM event_tickets WHERE event_id = $1`, eventID).Scan(
		&c.PendingTickets, &c.PendingQuantity, &c.PaidTickets, &c.PaidQuantity,
		&c.RefundedTickets, &c.CancelledTickets, &c.RevenuePaidMinor)
	if err != nil {
		return domain.EventTicketCounts{}, fmt.Errorf("event ticket counts: %w", err)
	}
	return c, nil
}

func scanOne(row scanner, resource string) (*domain.EventTicket, error) {
	t, err := scanTicket(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", resource, err)
	}
	return t, nil
}

func scanMany(rows pgx.Rows) ([]domain.EventTicket, error) {
	defer rows.Close()
	var out []domain.EventTicket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event ticket: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func scanTicket(row scanner) (*domain.EventTicket, error) {
	var t domain.EventTicket
	var currency, status string
	if err := row.Scan(
		&t.ID, &t.EventID, &t.RestaurantID, &t.UserID, &t.GuestName, &t.GuestPhone, &t.GuestEmail,
		&t.Quantity, &t.UnitPriceMinor, &t.TotalMinor, &currency, &status, &t.PaymentID,
		&t.PurchaseIdempotencyKey, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.Currency = domain.Currency(currency)
	t.Status = domain.TicketStatus(status)
	return &t, nil
}

type scanner interface{ Scan(dest ...any) error }

func statusStrings(ss []domain.TicketStatus) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

func paginate(pg, perPage int) (limit, offset int) {
	if pg <= 0 {
		pg = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return perPage, (pg - 1) * perPage
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

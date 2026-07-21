package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// History implements domain.BookingStatusHistoryRepository.
type History struct{ pool sqltx.Querier }

// NewHistory builds the status audit-trail repository.
func NewHistory(pool sqltx.Querier) *History { return &History{pool: pool} }

var _ domain.BookingStatusHistoryRepository = (*History)(nil)

const historyCols = `id, booking_id, from_status, to_status, actor_type, actor_id, reason, created_at`

func (r *History) Create(ctx context.Context, c *domain.BookingStatusChange) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	var from any
	if c.FromStatus != nil {
		from = string(*c.FromStatus)
	}
	q := `INSERT INTO booking_status_history (` + historyCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, c.ID, c.BookingID, from,
		string(c.ToStatus), string(c.ActorType), c.ActorID, c.Reason, c.CreatedAt); err != nil {
		return mapWrite(err, "create booking status history")
	}
	return nil
}

func (r *History) ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingStatusChange, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+historyCols+` FROM booking_status_history WHERE booking_id=$1 ORDER BY created_at, id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list booking status history: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingStatusChange
	for rows.Next() {
		var c domain.BookingStatusChange
		var from *string
		var to, actor string
		if err := rows.Scan(&c.ID, &c.BookingID, &from, &to, &actor, &c.ActorID,
			&c.Reason, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("list booking status history: %w", err)
		}
		if from != nil {
			s := domain.BookingStatus(*from)
			c.FromStatus = &s
		}
		c.ToStatus = domain.BookingStatus(to)
		c.ActorType = domain.ActorType(actor)
		out = append(out, c)
	}
	return out, rows.Err()
}

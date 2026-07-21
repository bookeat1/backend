package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Messages implements domain.BookingMessageRepository.
type Messages struct{ pool sqltx.Querier }

// NewMessages builds the booking chat repository.
func NewMessages(pool sqltx.Querier) *Messages { return &Messages{pool: pool} }

var _ domain.BookingMessageRepository = (*Messages)(nil)

const messageCols = `id, booking_id, sender_type, sender_id, message, is_read, read_at, created_at`

func (r *Messages) ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingMessage, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+messageCols+` FROM booking_messages WHERE booking_id=$1 ORDER BY created_at, id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list booking messages: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingMessage
	for rows.Next() {
		var m domain.BookingMessage
		var sender string
		if err := rows.Scan(&m.ID, &m.BookingID, &sender, &m.SenderID, &m.Message,
			&m.IsRead, &m.ReadAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("list booking messages: %w", err)
		}
		m.SenderType = domain.SenderType(sender)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Messages) Create(ctx context.Context, m *domain.BookingMessage) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	q := `INSERT INTO booking_messages (` + messageCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, m.ID, m.BookingID,
		string(m.SenderType), m.SenderID, m.Message, m.IsRead, m.ReadAt, m.CreatedAt); err != nil {
		return mapWrite(err, "create booking message")
	}
	return nil
}

// MarkRead flags the counterpart's unread messages as read. Messages written by
// reader itself are left alone — reading your own message is not a response.
func (r *Messages) MarkRead(ctx context.Context, bookingID uuid.UUID, reader domain.SenderType, at time.Time) (int, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_messages SET is_read=true, read_at=$3
		 WHERE booking_id=$1 AND sender_type <> $2 AND NOT is_read`,
		bookingID, string(reader), at)
	if err != nil {
		return 0, fmt.Errorf("mark booking messages read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

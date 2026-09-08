package notification

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Feed implements domain.NotificationFeedRepository: the guest's durable in-app
// «Уведомления» store behind FeedNotifier (writes) and GET /notifications
// (reads).
type Feed struct{ pool sqltx.Querier }

// NewFeed builds the in-app notification feed repository.
func NewFeed(pool sqltx.Querier) *Feed { return &Feed{pool: pool} }

var _ domain.NotificationFeedRepository = (*Feed)(nil)

const feedCols = `id, user_id, type, title, body, booking_id, restaurant_id, outbox_event_id, read_at, created_at`

// Insert appends a feed entry idempotently on (outbox_event_id, user_id). Under
// the at-least-once dispatcher a redelivered event hits ON CONFLICT DO NOTHING
// and inserts nothing; the boolean lets a caller (and tests) tell a fresh write
// from a deduped one.
func (r *Feed) Insert(ctx context.Context, n *domain.Notification) (bool, error) {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	q := `INSERT INTO notifications
	        (id, user_id, type, title, body, booking_id, restaurant_id, outbox_event_id, created_at)
	      VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
	      ON CONFLICT (outbox_event_id, user_id) DO NOTHING`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q,
		n.ID, n.UserID, string(n.Type), n.Title, n.Body, n.BookingID, n.RestaurantID, n.OutboxEventID)
	if err != nil {
		return false, fmt.Errorf("insert notification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListByUser returns the caller's entries newest-first, keyset-paginated on
// (created_at, id). A nil cursor starts at the newest row. The predicate is a
// row-value comparison so the composite index (user_id, created_at DESC, id
// DESC) serves it without a sort.
func (r *Feed) ListByUser(ctx context.Context, userID uuid.UUID, cursor *domain.NotificationCursor, limit int) ([]domain.Notification, error) {
	var (
		curTime any = nil
		curID   any = nil
	)
	if cursor != nil {
		curTime = cursor.CreatedAt
		curID = cursor.ID
	}
	q := `SELECT ` + feedCols + `
	      FROM notifications
	      WHERE user_id = $1
	        AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
	      ORDER BY created_at DESC, id DESC
	      LIMIT $4`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, userID, curTime, curID, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	var out []domain.Notification
	for rows.Next() {
		var (
			n  domain.Notification
			ty string
		)
		if err := rows.Scan(&n.ID, &n.UserID, &ty, &n.Title, &n.Body,
			&n.BookingID, &n.RestaurantID, &n.OutboxEventID, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("list notifications: %w", err)
		}
		n.Type = domain.NotificationFeedType(ty)
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountUnread returns the badge number. Served by the partial index
// idx_notifications_user_unread.
func (r *Feed) CountUnread(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id=$1 AND read_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return n, nil
}

// MarkRead marks ONE entry read, scoped to its owner. COALESCE keeps the first
// read timestamp so a repeat call is a no-op on the data. Zero rows affected
// means the id is unknown or owned by someone else — reported as ErrNotFound so
// a caller cannot distinguish the two and probe another guest's ids.
func (r *Feed) MarkRead(ctx context.Context, id, userID uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE notifications SET read_at = COALESCE(read_at, now()) WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return fmt.Errorf("mark notification read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// MarkAllRead marks every unread entry of the caller read. Nothing unread is a
// no-op success.
func (r *Feed) MarkAllRead(ctx context.Context, userID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE notifications SET read_at = now() WHERE user_id=$1 AND read_at IS NULL`, userID); err != nil {
		return fmt.Errorf("mark all notifications read: %w", err)
	}
	return nil
}

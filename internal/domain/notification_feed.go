package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// NotificationFeedType is the machine-readable kind the mobile app branches on
// (AppNotification.type). Stored as VARCHAR and validated in app code, never a
// DB enum — same discipline as booking status / cancelled_by.
type NotificationFeedType string

const (
	// FeedTypeBooking covers a booking's confirmation and cancellation — the
	// app renders both under its "booking" bucket.
	FeedTypeBooking NotificationFeedType = "booking"
	// FeedTypeReminder is the pre-visit nudge.
	FeedTypeReminder NotificationFeedType = "reminder"
	// FeedTypePromo is a marketing entry. No producer emits it yet; it exists so
	// the app's three known types all have a name on the backend.
	FeedTypePromo NotificationFeedType = "promo"
)

// Notification is one DURABLE entry in a guest's in-app «Уведомления» feed. It
// is written by notifications.FeedNotifier from a booking outbox event and read
// back by the guest over GET /notifications. Unlike a push it is not delivered
// and forgotten: it persists until the account is deleted, carries a read_at,
// and is paginated.
//
// BookingID / RestaurantID are the app's deep-link targets and are NULLABLE:
// the row outlives the booking or venue it came from (ON DELETE SET NULL), so a
// guest never loses a history entry because a venue left the platform.
type Notification struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Type          NotificationFeedType
	Title         string
	Body          string
	BookingID     *uuid.UUID
	RestaurantID  *uuid.UUID
	OutboxEventID uuid.UUID
	ReadAt        *time.Time
	CreatedAt     time.Time
}

// Read reports whether the guest has already seen this entry.
func (n Notification) Read() bool { return n.ReadAt != nil }

// NotificationCursor is the keyset position for feed pagination. The feed is
// ordered (created_at DESC, id DESC); a cursor is the last row a page returned,
// and the next page is everything strictly older than it. id breaks ties so two
// rows with the same created_at are never skipped or repeated.
type NotificationCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// NotificationFeedRepository persists and reads a guest's in-app feed.
type NotificationFeedRepository interface {
	// Insert appends a feed entry, idempotently on (outbox_event_id, user_id):
	// a redelivered outbox event (the dispatcher is at-least-once) is a no-op,
	// not a duplicate row. Reports whether a row was actually inserted.
	Insert(ctx context.Context, n *Notification) (inserted bool, err error)
	// ListByUser returns the caller's entries newest-first, keyset-paginated. A
	// nil cursor starts at the newest; a non-nil cursor returns everything
	// strictly older than it. limit caps the page size (the caller clamps it).
	ListByUser(ctx context.Context, userID uuid.UUID, cursor *NotificationCursor, limit int) ([]Notification, error)
	// CountUnread returns how many of the caller's entries are still unread — the
	// badge number.
	CountUnread(ctx context.Context, userID uuid.UUID) (int, error)
	// MarkRead marks ONE entry read, scoped to its owner. It is idempotent (an
	// already-read entry stays read at its original timestamp) but returns
	// ErrNotFound when the id does not exist OR belongs to another guest — the
	// user_id predicate is the tenant guard, so a caller cannot probe another
	// guest's ids.
	MarkRead(ctx context.Context, id, userID uuid.UUID) error
	// MarkAllRead marks every one of the caller's unread entries read. A caller
	// with nothing unread is a no-op success.
	MarkAllRead(ctx context.Context, userID uuid.UUID) error
}

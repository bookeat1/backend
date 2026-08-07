package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// FeedNotifier is the GUEST-facing DURABLE channel: instead of pushing to a
// device it appends a row to the guest's in-app «Уведомления» feed
// (domain.NotificationFeedRepository). It rides the SAME dispatcher and booking
// outbox as the push channels, so a guest gets both a live push AND a scrollable
// history from one outbox event, with no change to the bookings usecase.
//
// Two things distinguish it from the push channels:
//
//   - the fan-out set is the booking's own user_id. A booking made without an
//     account (phone / admin-entered) has nobody to show a feed to and is
//     skipped (nil error, the event drains);
//   - idempotency is NOT the shared notification_deliveries ledger but the
//     notifications table's own (outbox_event_id, user_id) unique key. The
//     dispatcher is at-least-once, so a redelivered event must not append a
//     duplicate history row — Insert is ON CONFLICT DO NOTHING.
//
// Unlike GuestPushNotifier it does NOT suppress a cancellation the guest
// performed themselves: a push echoing "you just cancelled" reads as a bug, but
// a DURABLE feed is a record — the guest expects their own cancellation to show
// up in their history, so it is written and simply names who cancelled.
type FeedNotifier struct {
	feed   domain.NotificationFeedRepository
	venues venueNameReader
	log    *slog.Logger
}

// NewFeedNotifier builds the in-app feed channel.
func NewFeedNotifier(
	feed domain.NotificationFeedRepository,
	venues venueNameReader,
	log *slog.Logger,
) *FeedNotifier {
	return &FeedNotifier{feed: feed, venues: venues, log: log}
}

var _ Notifier = (*FeedNotifier)(nil)

func (f *FeedNotifier) Channel() domain.NotificationChannel { return domain.ChannelInApp }

// Interested lists the three moments the guest keeps a record of, matching the
// guest push channel: confirmation, cancellation and the pre-visit reminder.
// booking.created is deliberately absent — the guest just tapped "book" and is
// on the confirmation screen; venue bookkeeping (arrived / no_show / completed)
// is nothing the guest gains from.
func (f *FeedNotifier) Interested(t domain.BookingEventType) bool {
	switch t {
	case domain.EventBookingConfirmed, domain.EventBookingCancelled, domain.EventBookingReminder:
		return true
	default:
		return false
	}
}

// Notify appends one feed entry for the booking's guest. It is idempotent on the
// outbox event, so an at-least-once redelivery never doubles a history row.
func (f *FeedNotifier) Notify(ctx context.Context, e Event) error {
	if e.GuestUserID == nil {
		// A phone / admin-entered booking: no account, so no feed to write to.
		// Not an error — the event drains.
		return nil
	}

	venue, err := f.venues.Name(ctx, e.RestaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The venue vanished from the catalog. Degrade to a nameless entry
			// rather than blocking the event forever.
			venue = ""
		} else {
			return fmt.Errorf("feed: read venue name: %w", err)
		}
	}

	feedType, title, body, ok := buildFeedEntry(e, venue)
	if !ok {
		// An event type Interested claims but buildFeedEntry has no text for — a
		// programming error, not a delivery failure. Drain it rather than retry
		// forever.
		f.log.Error("feed: no template for event, skipping",
			slog.String("event_type", string(e.Type)))
		return nil
	}

	bookingID := e.BookingID
	restaurantID := e.RestaurantID
	n := &domain.Notification{
		UserID:        *e.GuestUserID,
		Type:          feedType,
		Title:         title,
		Body:          body,
		BookingID:     &bookingID,
		RestaurantID:  &restaurantID,
		OutboxEventID: e.OutboxEventID,
	}
	if _, err := f.feed.Insert(ctx, n); err != nil {
		return fmt.Errorf("feed: insert notification: %w", err)
	}
	return nil
}

// buildFeedEntry renders the RU title+body and maps the booking event type to
// the AppNotification.type the mobile client branches on: confirmed+cancelled →
// "booking", reminder → "reminder". Returns ok=false for an event type it has no
// template for. Times render in the process local zone, the same convention the
// push channels use.
func buildFeedEntry(e Event, venue string) (domain.NotificationFeedType, string, string, bool) {
	when := e.StartsAt.Local().Format("02.01 в 15:04")
	at := ""
	if venue != "" {
		at = "«" + venue + "» · "
	}
	details := fmt.Sprintf("%s%s · %d чел.", at, when, e.Guests)

	switch e.Type {
	case domain.EventBookingConfirmed:
		return domain.FeedTypeBooking, "Бронь подтверждена", details, true
	case domain.EventBookingCancelled:
		body := details
		if by := cancelledByRU(e.CancelledBy); by != "" {
			body = details + " · отменена " + by
		}
		return domain.FeedTypeBooking, "Бронь отменена", body, true
	case domain.EventBookingReminder:
		return domain.FeedTypeReminder, "Напоминание о брони", details, true
	default:
		return "", "", "", false
	}
}

// cancelledByRU names who cancelled, for the cancellation feed body. Empty when
// unknown (an older event with no cancelled_by), so the body just omits the
// clause rather than printing a blank.
func cancelledByRU(by domain.CancelledBy) string {
	switch by {
	case domain.CancelledByGuest:
		return "вами"
	case domain.CancelledByRestaurant:
		return "рестораном"
	case domain.CancelledBySystem:
		return "системой"
	default:
		return ""
	}
}

// --- read side ---------------------------------------------------------------

// defaultFeedLimit / maxFeedLimit bound a page of the feed. The default keeps a
// first paint small; the max stops a client asking for the whole table in one
// request.
const (
	defaultFeedLimit = 20
	maxFeedLimit     = 100
)

// NotificationFeedUseCase is the read side of the in-app feed: the guest lists
// their own entries and marks them read. Every method is scoped to the caller's
// user id — owning the account IS the authorization, there is no restaurant to
// gate against.
type NotificationFeedUseCase struct {
	feed domain.NotificationFeedRepository
}

// NewNotificationFeedUseCase builds the feed read usecase.
func NewNotificationFeedUseCase(feed domain.NotificationFeedRepository) *NotificationFeedUseCase {
	return &NotificationFeedUseCase{feed: feed}
}

// FeedPage is one page of a guest's feed plus the unread badge count. Next is
// the cursor for the following page, nil when this page is the last one.
type FeedPage struct {
	Items       []domain.Notification
	UnreadCount int
	Next        *domain.NotificationCursor
}

// List returns a page of the caller's feed newest-first plus their unread
// count. limit is clamped to [1, maxFeedLimit] with a sane default. Next is set
// only when a full page came back (there may be more); a short page ends
// pagination.
func (u *NotificationFeedUseCase) List(ctx context.Context, userID uuid.UUID, cursor *domain.NotificationCursor, limit int) (FeedPage, error) {
	if limit <= 0 {
		limit = defaultFeedLimit
	}
	if limit > maxFeedLimit {
		limit = maxFeedLimit
	}
	items, err := u.feed.ListByUser(ctx, userID, cursor, limit)
	if err != nil {
		return FeedPage{}, err
	}
	unread, err := u.feed.CountUnread(ctx, userID)
	if err != nil {
		return FeedPage{}, err
	}
	var next *domain.NotificationCursor
	if len(items) == limit {
		last := items[len(items)-1]
		next = &domain.NotificationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return FeedPage{Items: items, UnreadCount: unread, Next: next}, nil
}

// MarkRead marks one of the caller's entries read. ErrNotFound (from the
// repository's owner-scoped update) surfaces as a 404 in the handler, so a
// caller cannot mark or probe another guest's entry.
func (u *NotificationFeedUseCase) MarkRead(ctx context.Context, id, userID uuid.UUID) error {
	return u.feed.MarkRead(ctx, id, userID)
}

// MarkAllRead clears the caller's unread badge.
func (u *NotificationFeedUseCase) MarkAllRead(ctx context.Context, userID uuid.UUID) error {
	return u.feed.MarkAllRead(ctx, userID)
}

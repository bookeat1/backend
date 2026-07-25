package notifications

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// GuestNotificationGate answers whether a GUEST (end user) wants a notification
// on a given channel, honoring their opt-out
// (domain.UserNotificationPreferenceRepository / user_notification_preferences).
//
// WHO CONSULTS IT, AND WHO MUST NOT:
//
// The STAFF notifiers (WebPushNotifier, TelegramNotifier) fan booking events out
// to a restaurant's registered push subscriptions / Telegram chat, scoped by
// restaurant_id, never to the guest. The guest opt-out does not apply to them,
// and this gate is deliberately kept OUT of the staff path so it cannot break
// it — a venue must keep hearing about its own bookings whatever its guests
// decided about their own phones.
//
// Every GUEST-facing notifier MUST consult it before sending. GuestPushNotifier
// (mobile push to the guest's own devices) is the first: it calls Allows with
// the booking's user id before reading a single token, and on false SKIPS the
// guest, returning nil — an opted-out guest must never leave the event
// unpublished, or a permanent opt-out would jam the outbox forever. Any future
// guest channel (email, SMS) follows the same shape.
type GuestNotificationGate struct {
	prefs domain.UserNotificationPreferenceRepository
}

// NewGuestNotificationGate builds the guest opt-out gate.
func NewGuestNotificationGate(prefs domain.UserNotificationPreferenceRepository) *GuestNotificationGate {
	return &GuestNotificationGate{prefs: prefs}
}

// Allows reports whether userID may be notified on channel. A user who never
// set a preference is allowed (the repository returns the all-enabled default),
// so opting out is always an explicit act. A read error is surfaced (not
// swallowed as "allowed"): a future notifier should treat it as a transient
// failure and retry, rather than notify an opted-out guest by accident.
func (g *GuestNotificationGate) Allows(ctx context.Context, userID uuid.UUID, channel domain.NotificationChannel) (bool, error) {
	pref, err := g.prefs.Get(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("guest notification gate: %w", err)
	}
	return pref.Allows(channel), nil
}

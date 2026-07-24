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
// WHY THIS EXISTS BUT IS NOT WIRED INTO THE DISPATCHER YET:
//
// The increment-1 dispatcher and its notifiers (WebPushNotifier, Telegram) fan
// booking events out to STAFF — the target is a restaurant's registered push
// subscriptions / Telegram chat, scoped by restaurant_id, never the guest. The
// guest opt-out therefore does NOT apply to any of today's sends, and this gate
// is deliberately left OUT of the staff path so it cannot break it.
//
// It is the seam every FUTURE guest-facing notifier (e.g. "your booking was
// confirmed" push/email to the guest's own devices) MUST consult before
// sending: build the notifier with a GuestNotificationGate and call Allows with
// the guest's user id first; on false, skip that guest without erroring the
// event. See the PR description for the go-live checklist.
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

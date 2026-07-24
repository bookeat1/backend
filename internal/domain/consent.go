package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ConsentSource names where a consent decision was captured. Stored as VARCHAR
// and validated in app code — never a DB enum.
type ConsentSource string

const (
	ConsentSourceApp ConsentSource = "app"
	ConsentSourceWeb ConsentSource = "web"
)

// ValidConsentSource reports whether s is a known capture source.
func ValidConsentSource(s ConsentSource) bool {
	return s == ConsentSourceApp || s == ConsentSourceWeb
}

// ConsentRecord is one immutable entry in the append-only consent log: a single
// grant OR revoke of one consent type, at one policy version, captured from one
// source at one instant. Records are never mutated or deleted (only the account
// soft-delete cascade removes them with the user), so the log is legal evidence
// of what the guest agreed to and when.
//
// ConsentType and Version are free-form (e.g. "privacy_policy" / "v3"): the
// canonical list and the versioning scheme are an owner/legal decision, carried
// as data, not hardcoded here.
type ConsentRecord struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	ConsentType string
	Version     string
	Granted     bool
	Source      ConsentSource
	CreatedAt   time.Time
}

// UserConsentRepository persists the append-only consent log.
type UserConsentRepository interface {
	// Append inserts one immutable consent record. It never updates an existing
	// row — a new grant or revoke of the same type is a new row, preserving the
	// full history.
	Append(ctx context.Context, rec *ConsentRecord) error
	// CurrentState returns the LATEST record per consent_type for userID (the
	// user's effective consent state), newest-first by consent_type. An empty
	// slice (never ErrNotFound) means the user has recorded no consent yet.
	CurrentState(ctx context.Context, userID uuid.UUID) ([]ConsentRecord, error)
}

// NotificationPreference is a guest's own notification opt-out. NotificationsEnabled
// is the master switch; the per-channel flags are only consulted when it is true.
type NotificationPreference struct {
	UserID               uuid.UUID
	NotificationsEnabled bool
	PushEnabled          bool
	EmailEnabled         bool
	UpdatedAt            time.Time
}

// DefaultNotificationPreference is the effective preference for a user who has
// never set one: everything enabled (opting out is always an explicit action).
func DefaultNotificationPreference(userID uuid.UUID) NotificationPreference {
	return NotificationPreference{
		UserID:               userID,
		NotificationsEnabled: true,
		PushEnabled:          true,
		EmailEnabled:         true,
	}
}

// Allows reports whether the guest wants a notification on channel: the master
// switch must be on AND the channel's own flag must be on. A channel this
// preference model does not track (e.g. a future one) defaults to allowed as
// long as the master switch is on, so adding a channel never silently mutes it.
func (p NotificationPreference) Allows(channel NotificationChannel) bool {
	if !p.NotificationsEnabled {
		return false
	}
	switch channel {
	case ChannelWebPush:
		return p.PushEnabled
	default:
		return true
	}
}

// UserNotificationPreferenceRepository persists a guest's notification opt-out.
type UserNotificationPreferenceRepository interface {
	// Get returns userID's preference, or DefaultNotificationPreference when no
	// row exists yet. It never returns ErrNotFound for a missing preference row.
	Get(ctx context.Context, userID uuid.UUID) (NotificationPreference, error)
	// Upsert inserts or replaces the user's preference row (keyed by user_id).
	Upsert(ctx context.Context, pref NotificationPreference) error
}

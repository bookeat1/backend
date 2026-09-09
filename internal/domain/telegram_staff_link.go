package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TelegramStaffLink remembers which BookEat account a Telegram account signed in
// as from the venue mini app, so staff type an email and a password once and
// every later open goes straight to the shift screen (spec §5.3).
//
// The venue is deliberately NOT part of this record. Rights are read live from
// restaurant_managers on every request; storing them here would create a second
// source of truth about access, and a second place to forget to update when
// somebody is fired.
type TelegramStaffLink struct {
	// TelegramUserID is the primary key: one Telegram account points at exactly
	// one BookEat account. Signing in under a different email overwrites this
	// row rather than adding another.
	TelegramUserID int64
	UserID         uuid.UUID
	// ChatID is the private chat the mini app was opened from.
	ChatID     int64
	LinkedAt   time.Time
	LastSeenAt *time.Time
	// RevokedAt is set instead of deleting the row, so "why did my access
	// disappear" has an answer. A link is active only while it is nil.
	RevokedAt *time.Time
}

// Active reports whether the link may still be used to sign in.
func (l *TelegramStaffLink) Active() bool { return l != nil && l.RevokedAt == nil }

// TelegramStaffLinkRepository stores the mini app's Telegram ↔ account links.
type TelegramStaffLinkRepository interface {
	// GetByTelegramUserID returns the link for a Telegram account, revoked ones
	// included — the caller decides what a revoked link means. ErrNotFound when
	// the account never signed in.
	GetByTelegramUserID(ctx context.Context, telegramUserID int64) (*TelegramStaffLink, error)
	// Upsert creates the link or repoints an existing one at another account,
	// clearing RevokedAt and refreshing LinkedAt. This is what makes signing in
	// under a colleague's email on the same phone work rather than fail.
	Upsert(ctx context.Context, l *TelegramStaffLink) error
	// Revoke marks one Telegram account's link revoked. Idempotent: revoking an
	// unknown or already-revoked link is a silent success, because both mean
	// what the caller wanted (this device cannot sign in).
	Revoke(ctx context.Context, telegramUserID int64) error
	// RevokeByUser marks every link of one BookEat account revoked — all of an
	// employee's devices at once, for the day their last venue membership is
	// removed. Returns the number of links it touched.
	RevokeByUser(ctx context.Context, userID uuid.UUID) (int, error)
	// TouchLastSeen records that the link was just used. Best-effort telemetry:
	// a failure here must never fail a sign-in.
	TouchLastSeen(ctx context.Context, telegramUserID int64) error
}

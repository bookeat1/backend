// Package telegramlink is the Postgres implementation of the venue mini app's
// Telegram ↔ BookEat account links (migration 0099).
package telegramlink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.TelegramStaffLinkRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the link repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.TelegramStaffLinkRepository = (*Repository)(nil)

const cols = `telegram_user_id, user_id, chat_id, linked_at, last_seen_at, revoked_at`

// GetByTelegramUserID returns the link, revoked ones included: the caller needs
// to tell "never signed in" (show the password form) apart from "access was
// withdrawn" (say so), and a filter here would collapse both into ErrNotFound.
func (r *Repository) GetByTelegramUserID(ctx context.Context, telegramUserID int64) (*domain.TelegramStaffLink, error) {
	var l domain.TelegramStaffLink
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM telegram_staff_links WHERE telegram_user_id=$1`, telegramUserID,
	).Scan(&l.TelegramUserID, &l.UserID, &l.ChatID, &l.LinkedAt, &l.LastSeenAt, &l.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get telegram staff link: %w", err)
	}
	return &l, nil
}

// Upsert writes the link, repointing an existing row at another account. The
// conflict branch resets revoked_at to NULL and linked_at to now: a colleague
// signing in on the same phone, or the same person coming back after being
// revoked, is a NEW link that happens to reuse a Telegram id, and leaving the
// old revoked_at in place would let a fresh, password-checked sign-in be treated
// as withdrawn.
func (r *Repository) Upsert(ctx context.Context, l *domain.TelegramStaffLink) error {
	q := `INSERT INTO telegram_staff_links (telegram_user_id, user_id, chat_id, linked_at, revoked_at)
	      VALUES ($1,$2,$3, now(), NULL)
	      ON CONFLICT (telegram_user_id) DO UPDATE
	        SET user_id    = EXCLUDED.user_id,
	            chat_id    = EXCLUDED.chat_id,
	            linked_at  = now(),
	            revoked_at = NULL
	      RETURNING linked_at`
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx, q,
		l.TelegramUserID, l.UserID, l.ChatID).Scan(&l.LinkedAt); err != nil {
		return fmt.Errorf("upsert telegram staff link: %w", err)
	}
	l.RevokedAt = nil
	return nil
}

// Revoke marks one Telegram account's link revoked. The `revoked_at IS NULL`
// predicate keeps the FIRST revocation time rather than overwriting it on every
// repeat, which is the timestamp anyone asking "since when" wants.
func (r *Repository) Revoke(ctx context.Context, telegramUserID int64) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE telegram_staff_links SET revoked_at = now()
		 WHERE telegram_user_id=$1 AND revoked_at IS NULL`, telegramUserID); err != nil {
		return fmt.Errorf("revoke telegram staff link: %w", err)
	}
	return nil
}

// RevokeByUser revokes every device of one account and reports how many it
// touched, so the caller can log a real number instead of "done".
func (r *Repository) RevokeByUser(ctx context.Context, userID uuid.UUID) (int, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE telegram_staff_links SET revoked_at = now()
		 WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke telegram staff links by user: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// TouchLastSeen records a successful use of the link.
func (r *Repository) TouchLastSeen(ctx context.Context, telegramUserID int64) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE telegram_staff_links SET last_seen_at=$2 WHERE telegram_user_id=$1`,
		telegramUserID, time.Now().UTC()); err != nil {
		return fmt.Errorf("touch telegram staff link: %w", err)
	}
	return nil
}

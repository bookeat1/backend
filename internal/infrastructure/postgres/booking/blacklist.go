package booking

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

// Blacklist implements domain.BookingBlacklistRepository.
type Blacklist struct{ pool sqltx.Querier }

// NewBlacklist builds the guest stop-list repository.
func NewBlacklist(pool sqltx.Querier) *Blacklist { return &Blacklist{pool: pool} }

var _ domain.BookingBlacklistRepository = (*Blacklist)(nil)

const blacklistCols = `id, restaurant_id, user_id, phone_normalized, email, reason,
	created_by, is_active, created_at, updated_at`

// Match returns the venue-scoped entry first, then the global one, so the
// caller sees the most specific reason. An empty phone/email never matches:
// without the guards a guest with no email would hit every NULL-email row.
func (r *Blacklist) Match(ctx context.Context, q domain.BlacklistQuery) (*domain.BlacklistEntry, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+blacklistCols+` FROM booking_blacklist
		 WHERE is_active
		   AND (restaurant_id IS NULL OR restaurant_id = $1)
		   AND ( (user_id IS NOT NULL AND user_id = $2)
		      OR ($3 <> '' AND phone_normalized = $3)
		      OR ($4 <> '' AND email = $4) )
		 ORDER BY (restaurant_id IS NULL), created_at
		 LIMIT 1`,
		q.RestaurantID, q.UserID, q.PhoneNormalized, q.Email)
	e, err := scanBlacklist(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("match blacklist: %w", err)
	}
	return e, nil
}

func (r *Blacklist) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.BlacklistEntry, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+blacklistCols+` FROM booking_blacklist
		 WHERE is_active AND (restaurant_id = $1 OR restaurant_id IS NULL)
		 ORDER BY created_at DESC, id`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list blacklist: %w", err)
	}
	defer rows.Close()
	var out []domain.BlacklistEntry
	for rows.Next() {
		e, err := scanBlacklist(rows)
		if err != nil {
			return nil, fmt.Errorf("list blacklist: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *Blacklist) Create(ctx context.Context, e *domain.BlacklistEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	now := time.Now()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	q := `INSERT INTO booking_blacklist (` + blacklistCols + `) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, e.ID, e.RestaurantID, e.UserID,
		e.PhoneNormalized, e.Email, e.Reason, e.CreatedBy, e.IsActive, e.CreatedAt, e.UpdatedAt); err != nil {
		return mapWrite(err, "create blacklist entry")
	}
	return nil
}

func (r *Blacklist) Deactivate(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_blacklist SET is_active=false, updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("deactivate blacklist entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func scanBlacklist(row scanner) (*domain.BlacklistEntry, error) {
	var e domain.BlacklistEntry
	if err := row.Scan(&e.ID, &e.RestaurantID, &e.UserID, &e.PhoneNormalized, &e.Email,
		&e.Reason, &e.CreatedBy, &e.IsActive, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

// RateLog implements domain.BookingRateLogRepository.
type RateLog struct{ pool sqltx.Querier }

// NewRateLog builds the anti-fraud log repository.
func NewRateLog(pool sqltx.Querier) *RateLog { return &RateLog{pool: pool} }

var _ domain.BookingRateLogRepository = (*RateLog)(nil)

func (r *RateLog) Create(ctx context.Context, e *domain.BookingRateLogEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	q := `INSERT INTO booking_rate_log
		(id, user_id, phone_normalized, email, restaurant_id, action, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, e.ID, e.UserID, e.PhoneNormalized,
		e.Email, e.RestaurantID, string(e.Action), e.CreatedAt); err != nil {
		return mapWrite(err, "create rate log entry")
	}
	return nil
}

func (r *RateLog) CountSince(ctx context.Context, phoneNormalized string, action domain.RateLogAction, since time.Time) (int, error) {
	var n int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM booking_rate_log
		 WHERE phone_normalized = $1 AND action = $2 AND created_at >= $3`,
		phoneNormalized, string(action), since).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rate log entries: %w", err)
	}
	return n, nil
}

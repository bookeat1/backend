package homefeed

import (
	"context"
	"fmt"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Promotions implements domain.PromotionRepository.
type Promotions struct{ pool sqltx.Querier }

// NewPromotions builds the promotion repository.
func NewPromotions(pool sqltx.Querier) *Promotions { return &Promotions{pool: pool} }

var _ domain.PromotionRepository = (*Promotions)(nil)

func (r *Promotions) ListActive(ctx context.Context) ([]domain.Promotion, error) {
	// The active window is checked against the database clock (now()) so the
	// filter is deterministic and independent of the app server's timezone. A
	// NULL bound is open-ended on that side.
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, restaurant_id, title, discount_label, starts_at, ends_at,
			image_url, is_active, sort, created_at, updated_at
		 FROM promotions
		 WHERE is_active = true
			AND (starts_at IS NULL OR starts_at <= now())
			AND (ends_at IS NULL OR ends_at >= now())
		 ORDER BY sort, created_at`)
	if err != nil {
		return nil, fmt.Errorf("list promotions: %w", err)
	}
	defer rows.Close()
	var out []domain.Promotion
	for rows.Next() {
		var p domain.Promotion
		if err := rows.Scan(&p.ID, &p.RestaurantID, &p.Title, &p.DiscountLabel,
			&p.StartsAt, &p.EndsAt, &p.ImageURL, &p.IsActive, &p.Sort,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Package homefeed is the Postgres implementation of the mobile Home
// ("Explore") screen read repositories: cuisines, promotions and articles.
package homefeed

import (
	"context"
	"fmt"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Cuisines implements domain.CuisineRepository.
type Cuisines struct{ pool sqltx.Querier }

// NewCuisines builds the cuisine repository.
func NewCuisines(pool sqltx.Querier) *Cuisines { return &Cuisines{pool: pool} }

var _ domain.CuisineRepository = (*Cuisines)(nil)

func (r *Cuisines) ListActive(ctx context.Context) ([]domain.Cuisine, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, name, image_url, sort, is_active, created_at, updated_at
		 FROM cuisines WHERE is_active = true ORDER BY sort, name`)
	if err != nil {
		return nil, fmt.Errorf("list cuisines: %w", err)
	}
	defer rows.Close()
	var out []domain.Cuisine
	for rows.Next() {
		var c domain.Cuisine
		if err := rows.Scan(&c.ID, &c.Name, &c.ImageURL, &c.Sort, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

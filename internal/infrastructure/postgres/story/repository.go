// Package story is the Postgres implementation of the restaurant story
// repository.
package story

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.StoryRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the story repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.StoryRepository = (*Repository)(nil)

// selCols lists restaurant_stories columns for reads.
const selCols = `id, restaurant_id, image_url, caption, sort_order, is_active, created_at`

func (r *Repository) ListActiveByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error) {
	q := `SELECT ` + selCols + ` FROM restaurant_stories
	      WHERE restaurant_id=$1 AND is_active
	      ORDER BY sort_order ASC, created_at ASC`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list restaurant stories: %w", err)
	}
	defer rows.Close()
	var stories []domain.Story
	for rows.Next() {
		s, err := scanStory(rows)
		if err != nil {
			return nil, fmt.Errorf("list restaurant stories: %w", err)
		}
		stories = append(stories, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restaurant stories: %w", err)
	}
	return stories, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanStory(row scanner) (*domain.Story, error) {
	var s domain.Story
	if err := row.Scan(
		&s.ID, &s.RestaurantID, &s.ImageURL, &s.Caption, &s.SortOrder, &s.IsActive, &s.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

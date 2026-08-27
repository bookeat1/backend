// Package story is the Postgres implementation of the restaurant story
// repository.
package story

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.StoryRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the story repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.StoryRepository = (*Repository)(nil)

// selCols lists restaurant_stories columns for reads.
const selCols = `id, restaurant_id, image_url, caption, action_url, sort_order, is_active, expires_at, created_at`

// ListActiveByRestaurant is the guest read: a venue's rail as of the instant
// now. Two independent gates, both server-side so every client — app, cabinet,
// anything later — sees the same truth:
//
//	is_active                              the venue's manual switch
//	expires_at IS NULL OR expires_at > now  the optional lifetime (0088)
//
// The NULL branch is the load-bearing half: every story written before 0088 has
// expires_at IS NULL and must keep being served exactly as before. Writing this
// as a plain `expires_at > now` would silently retire the entire existing
// catalogue, because NULL > now is NULL, not true.
//
// Filtering does not touch sort_order, so the remaining cards keep their
// relative order and simply close ranks — the rail is shorter, never reshuffled.
func (r *Repository) ListActiveByRestaurant(ctx context.Context, restaurantID uuid.UUID, now time.Time) ([]domain.Story, error) {
	// id is the final tie-break: now() is constant within a transaction, so a
	// bulk insert gives every row the same created_at — without id the order of
	// same-sort_order cards would not be stable between reads. The listing index
	// carries all three sort columns so this stays an index-ordered scan; the
	// expires_at predicate is a recheck on the handful of rows a venue has (see
	// migration 0088 on why it is not in the index).
	q := `SELECT ` + selCols + ` FROM restaurant_stories
	      WHERE restaurant_id=$1 AND is_active
	        AND (expires_at IS NULL OR expires_at > $2)
	      ORDER BY sort_order ASC, created_at ASC, id ASC`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, restaurantID, now)
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

// ListByRestaurant returns ALL of a restaurant's stories (active, inactive and
// EXPIRED) for the admin cabinet, in the same display order as the public read.
// Only the visibility filters are dropped — the ordering (and its stable id
// tie-break) is identical so the cabinet shows cards in the exact order guests
// would see them. There is deliberately no clock here: a card that stopped
// being served must still appear in the venue's own list, marked as expired, so
// the venue can extend or delete it instead of hunting for a card that silently
// disappeared.
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error) {
	q := `SELECT ` + selCols + ` FROM restaurant_stories
	      WHERE restaurant_id=$1
	      ORDER BY sort_order ASC, created_at ASC, id ASC`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list restaurant stories (admin): %w", err)
	}
	defer rows.Close()
	var stories []domain.Story
	for rows.Next() {
		s, err := scanStory(rows)
		if err != nil {
			return nil, fmt.Errorf("list restaurant stories (admin): %w", err)
		}
		stories = append(stories, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restaurant stories (admin): %w", err)
	}
	return stories, nil
}

// GetByID returns a story by its id regardless of is_active.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Story, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selCols+` FROM restaurant_stories WHERE id=$1`, id)
	s, err := scanStory(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get restaurant story: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get restaurant story: %w", err)
	}
	return s, nil
}

// Create inserts a new story. An unknown restaurant_id (FK violation) maps to
// ErrNotFound. created_at is written back onto s.
func (r *Repository) Create(ctx context.Context, s *domain.Story) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO restaurant_stories (id, restaurant_id, image_url, caption, action_url, sort_order, is_active, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING created_at`,
		s.ID, s.RestaurantID, s.ImageURL, s.Caption, s.ActionURL, s.SortOrder, s.IsActive, s.ExpiresAt).
		Scan(&s.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create restaurant story: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create restaurant story: %w", err)
	}
	return nil
}

// Update overwrites the mutable fields of s.ID, scoped to s.RestaurantID: an id
// belonging to another tenant matches zero rows and maps to ErrNotFound, so a
// caller can never edit a card outside the restaurant it was authorized against.
func (r *Repository) Update(ctx context.Context, s *domain.Story) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurant_stories
		 SET image_url=$3, caption=$4, action_url=$5, sort_order=$6, is_active=$7, expires_at=$8
		 WHERE id=$1 AND restaurant_id=$2`,
		s.ID, s.RestaurantID, s.ImageURL, s.Caption, s.ActionURL, s.SortOrder, s.IsActive, s.ExpiresAt)
	if err != nil {
		return fmt.Errorf("update restaurant story: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update restaurant story: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes the story id scoped to restaurantID. A zero-rows delete (absent
// id, or an id owned by another restaurant) maps to ErrNotFound.
func (r *Repository) Delete(ctx context.Context, id, restaurantID uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM restaurant_stories WHERE id=$1 AND restaurant_id=$2`, id, restaurantID)
	if err != nil {
		return fmt.Errorf("delete restaurant story: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete restaurant story: %w", domain.ErrNotFound)
	}
	return nil
}

// Reorder rewrites sort_order so each id's new value is its zero-based position
// in orderedIDs. Done as a single set-based UPDATE joined against the list's
// ordinality: it is atomic without an explicit transaction, the restaurant_id
// predicate silently drops any id that is not this venue's (cross-tenant ids
// cannot renumber our rows), and any of the venue's rows absent from the list
// simply keep their current sort_order. An empty list is a no-op.
func (r *Repository) Reorder(ctx context.Context, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error {
	if len(orderedIDs) == 0 {
		return nil
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurant_stories s
		 SET sort_order = v.ord
		 FROM (
		   SELECT id, (ordinality - 1)::int AS ord
		   FROM unnest($2::uuid[]) WITH ORDINALITY AS t(id, ordinality)
		 ) v
		 WHERE s.id = v.id AND s.restaurant_id = $1`,
		restaurantID, orderedIDs)
	if err != nil {
		return fmt.Errorf("reorder restaurant stories: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanStory(row scanner) (*domain.Story, error) {
	var s domain.Story
	if err := row.Scan(
		&s.ID, &s.RestaurantID, &s.ImageURL, &s.Caption, &s.ActionURL, &s.SortOrder, &s.IsActive, &s.ExpiresAt, &s.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

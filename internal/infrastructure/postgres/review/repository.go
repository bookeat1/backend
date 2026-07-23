// Package review is the Postgres implementation of domain.ReviewRepository.
package review

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

// Repository implements domain.ReviewRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the review repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.ReviewRepository = (*Repository)(nil)

// Upsert inserts a new review or overwrites the rating/body of the caller's
// existing one for the same restaurant. ON CONFLICT targets the
// (restaurant_id, user_id) unique constraint, so a guest editing their review
// is one round-trip with no check-then-write race. status and owner_reply are
// intentionally NOT in the DO UPDATE set: a hidden review stays hidden and an
// existing venue reply survives the guest editing their text. An unknown
// restaurant_id (FK violation) maps to ErrNotFound, same as favorites.
func (r *Repository) Upsert(ctx context.Context, rv *domain.Review) error {
	if rv.ID == uuid.Nil {
		rv.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO reviews (id, restaurant_id, user_id, rating, body)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (restaurant_id, user_id) DO UPDATE
		   SET rating = EXCLUDED.rating,
		       body = EXCLUDED.body,
		       updated_at = now()
		 RETURNING id, rating, body, status, owner_reply, replied_at, created_at, updated_at`,
		rv.ID, rv.RestaurantID, rv.UserID, rv.Rating, rv.Body).
		Scan(&rv.ID, &rv.Rating, &rv.Body, &rv.Status, &rv.OwnerReply, &rv.RepliedAt, &rv.CreatedAt, &rv.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("upsert review: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("upsert review: %w", err)
	}
	return nil
}

// GetOwn returns the caller's own review for a restaurant.
func (r *Repository) GetOwn(ctx context.Context, restaurantID, userID uuid.UUID) (*domain.Review, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id, restaurant_id, user_id, rating, body, status, owner_reply, replied_at, created_at, updated_at
		 FROM reviews WHERE restaurant_id = $1 AND user_id = $2`, restaurantID, userID)
	return scanReview(row, "get own review")
}

// GetByID returns a review by its id — used by staff moderation/reply to
// resolve the review's own restaurant before authorizing.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Review, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id, restaurant_id, user_id, rating, body, status, owner_reply, replied_at, created_at, updated_at
		 FROM reviews WHERE id = $1`, id)
	return scanReview(row, "get review")
}

// DeleteOwn removes the caller's own review. Zero rows affected (nothing to
// delete) is not an error, matching the idempotent contract.
func (r *Repository) DeleteOwn(ctx context.Context, restaurantID, userID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM reviews WHERE restaurant_id = $1 AND user_id = $2`, restaurantID, userID)
	if err != nil {
		return fmt.Errorf("delete review: %w", err)
	}
	return nil
}

// ListPublished returns a restaurant's published reviews joined with the
// reviewer's display name, newest first with id as a stable tie-breaker so a
// page boundary never duplicates or drops a review, plus the total published
// count. The WHERE/ORDER BY match idx_reviews_published_listing exactly.
func (r *Repository) ListPublished(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.ReviewListItem, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM reviews WHERE restaurant_id = $1 AND status = 'published'`,
		restaurantID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count published reviews: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT rv.id, rv.restaurant_id, rv.user_id, rv.rating, rv.body, rv.status,
		        rv.owner_reply, rv.replied_at, rv.created_at, rv.updated_at,
		        COALESCE(u.full_name, '')
		 FROM reviews rv
		 JOIN users u ON u.id = rv.user_id
		 WHERE rv.restaurant_id = $1 AND rv.status = 'published'
		 ORDER BY rv.created_at DESC, rv.id DESC
		 LIMIT $2 OFFSET $3`,
		restaurantID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list published reviews: %w", err)
	}
	defer rows.Close()

	var items []domain.ReviewListItem
	for rows.Next() {
		var it domain.ReviewListItem
		if err := rows.Scan(&it.ID, &it.RestaurantID, &it.UserID, &it.Rating, &it.Body, &it.Status,
			&it.OwnerReply, &it.RepliedAt, &it.CreatedAt, &it.UpdatedAt, &it.AuthorName); err != nil {
			return nil, 0, fmt.Errorf("list published reviews: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list published reviews: %w", err)
	}
	return items, total, nil
}

// Aggregate computes AVG(rating) and COUNT(*) over a restaurant's PUBLISHED
// reviews in one query. COALESCE turns the AVG-of-nothing NULL into 0 so a
// never-reviewed restaurant returns {0, 0} rather than a NULL scan target.
func (r *Repository) Aggregate(ctx context.Context, restaurantID uuid.UUID) (domain.RatingAggregate, error) {
	agg := domain.RatingAggregate{RestaurantID: restaurantID}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT COALESCE(AVG(rating), 0)::float8, COUNT(*)
		 FROM reviews WHERE restaurant_id = $1 AND status = 'published'`,
		restaurantID).Scan(&agg.Average, &agg.Count)
	if err != nil {
		return domain.RatingAggregate{}, fmt.Errorf("aggregate rating: %w", err)
	}
	return agg, nil
}

// SetStatus moderates a review. A zero-rows UPDATE means the id is absent.
func (r *Repository) SetStatus(ctx context.Context, id uuid.UUID, status domain.ReviewStatus) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE reviews SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set review status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set review status: %w", domain.ErrNotFound)
	}
	return nil
}

// SetReply writes the venue's reply plus its timestamp together (the DB CHECK
// requires both or neither). A zero-rows UPDATE means the id is absent.
func (r *Repository) SetReply(ctx context.Context, id uuid.UUID, reply string, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE reviews SET owner_reply = $2, replied_at = $3, updated_at = now() WHERE id = $1`,
		id, reply, at)
	if err != nil {
		return fmt.Errorf("set review reply: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set review reply: %w", domain.ErrNotFound)
	}
	return nil
}

func scanReview(row pgx.Row, op string) (*domain.Review, error) {
	var rv domain.Review
	err := row.Scan(&rv.ID, &rv.RestaurantID, &rv.UserID, &rv.Rating, &rv.Body, &rv.Status,
		&rv.OwnerReply, &rv.RepliedAt, &rv.CreatedAt, &rv.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return &rv, nil
}

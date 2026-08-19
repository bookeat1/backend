// Package favorite is the Postgres implementation of domain.FavoriteRepository.
package favorite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.FavoriteRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the favorite repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.FavoriteRepository = (*Repository)(nil)

// Add is a single idempotent upsert: ON CONFLICT DO NOTHING means bookmarking
// an already-favorited restaurant twice is a silent no-op rather than a
// unique-violation error, and there is no separate existence check racing
// against the insert.
func (r *Repository) Add(ctx context.Context, userID, restaurantID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_favorites (user_id, restaurant_id) VALUES ($1, $2)
		 ON CONFLICT (user_id, restaurant_id) DO NOTHING`, userID, restaurantID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("add favorite: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("add favorite: %w", err)
	}
	return nil
}

// Remove is a plain DELETE: zero rows affected (already not favorited, or
// restaurantID never existed) is not an error, matching the idempotent
// contract in domain.FavoriteRepository.
func (r *Repository) Remove(ctx context.Context, userID, restaurantID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM restaurant_favorites WHERE user_id=$1 AND restaurant_id=$2`, userID, restaurantID)
	if err != nil {
		return fmt.Errorf("remove favorite: %w", err)
	}
	return nil
}

// ListByUser is ListRestaurantsByUser without the bookmark timestamps — the
// shape the original GET /favorites answers with. Both go through the same
// query so the two screens can never disagree about which venues are in there.
func (r *Repository) ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.RestaurantListItem, error) {
	items, err := r.ListRestaurantsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RestaurantListItem, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant)
	}
	return out, nil
}

// ListRestaurantsByUser joins through restaurant_favorites, reusing
// restaurant.Columns / restaurant.ScanListItem so a favorited restaurant
// serializes identically to one returned by the public catalog listing
// (including its primary image). Deactivated restaurants are excluded, same
// visibility rule as the catalog.
func (r *Repository) ListRestaurantsByUser(ctx context.Context, userID uuid.UUID) ([]domain.FavoriteRestaurantItem, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+prefixed(restaurant.Columns, "r")+`, `+restaurant.ListExtraColumns+`, f.created_at
		 FROM restaurant_favorites f
		 JOIN restaurants r ON r.id = f.restaurant_id
		 WHERE f.user_id = $1 AND r.is_active = true
		 ORDER BY f.created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	defer rows.Close()

	var items []domain.FavoriteRestaurantItem
	for rows.Next() {
		// The bookmark timestamp is the LAST column of the row, so it is peeled
		// off with a wrapper scanner rather than by re-implementing
		// restaurant.ScanListItem with one more destination.
		var favoritedAt time.Time
		base, primary, err := restaurant.ScanListItem(trailingScanner{row: rows, extra: []any{&favoritedAt}})
		if err != nil {
			return nil, fmt.Errorf("list favorites: %w", err)
		}
		items = append(items, domain.FavoriteRestaurantItem{
			Restaurant:  domain.RestaurantListItem{Restaurant: *base, PrimaryImage: primary},
			FavoritedAt: favoritedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list favorites: %w", err)
	}
	return items, nil
}

// trailingScanner appends extra destinations to whatever a shared scan helper
// asks for, so a caller can select one more column after a borrowed column list
// without forking the helper. The extras must be the LAST columns of the SELECT.
type trailingScanner struct {
	row   interface{ Scan(dest ...any) error }
	extra []any
}

func (s trailingScanner) Scan(dest ...any) error {
	return s.row.Scan(append(dest, s.extra...)...)
}

// FavoriteSet reports which of restaurantIDs are favorited by userID. Never
// queries with an empty ANY($2) slice (a no-op that would still round-trip to
// the DB for nothing).
func (r *Repository) FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT restaurant_id FROM restaurant_favorites WHERE user_id=$1 AND restaurant_id = ANY($2)`,
		userID, restaurantIDs)
	if err != nil {
		return nil, fmt.Errorf("favorite set: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("favorite set: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("favorite set: %w", err)
	}
	return out, nil
}

// prefixed rewrites a bare column list into a table-qualified one — same
// logic as restaurant.prefixed (unexported there), needed here for the same
// reason restaurant.Columns/ScanListItem are exported: this package selects
// the same column set through its own join.
func prefixed(colList, alias string) string {
	parts := strings.Split(colList, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

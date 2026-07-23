package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Favorite links a user to a restaurant they bookmarked.
type Favorite struct {
	UserID       uuid.UUID
	RestaurantID uuid.UUID
	CreatedAt    time.Time
}

// FavoriteRepository persists a user's bookmarked restaurants. Every operation
// is scoped by userID: there is no "get favorite by id" — a favorite is
// addressed by the (user, restaurant) pair only, so a caller can never reach
// another user's bookmark.
type FavoriteRepository interface {
	// Add bookmarks restaurantID for userID. Idempotent: bookmarking an
	// already-favorited restaurant is a silent no-op, not an error. Returns
	// ErrNotFound if restaurantID does not exist.
	Add(ctx context.Context, userID, restaurantID uuid.UUID) error
	// Remove un-bookmarks restaurantID for userID. Idempotent: removing a
	// favorite that doesn't exist (or never existed) is a silent no-op, not
	// an error.
	Remove(ctx context.Context, userID, restaurantID uuid.UUID) error
	// ListByUser returns userID's bookmarked restaurants that are still
	// active, most recently favorited first. A restaurant deactivated after
	// being favorited is excluded, same visibility rule as the public catalog.
	ListByUser(ctx context.Context, userID uuid.UUID) ([]RestaurantListItem, error)
	// FavoriteSet reports which of restaurantIDs are favorited by userID.
	// A restaurant id absent from the returned map is "not favorited" — the
	// map never holds an explicit false entry.
	FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

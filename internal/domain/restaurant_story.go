package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Story is an Instagram-highlight-style promo card a restaurant pins to its
// storefront: a picture plus an optional short caption, rendered in a horizontal
// rail in the guest app. Caption is a pointer because it is optional per product
// (a wordless highlight card is valid); nil means "no caption", not an empty
// string. SortOrder governs left-to-right display order; IsActive lets a venue
// retire a card without deleting it.
type Story struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	ImageURL     string
	Caption      *string
	SortOrder    int
	IsActive     bool
	CreatedAt    time.Time
}

// StoryRepository reads and writes restaurant stories. The public guest app uses
// only ListActiveByRestaurant; the rest is the venue/admin cabinet surface.
type StoryRepository interface {
	// ListActiveByRestaurant returns the active stories of restaurantID ordered
	// by sort_order ASC, then created_at ASC (a stable tie-break so two cards
	// sharing a sort_order never reshuffle between reads). Inactive cards are
	// never returned. An absent restaurant is not an error — it simply has no
	// stories, so the result is an empty slice.
	ListActiveByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]Story, error)

	// ListByRestaurant returns ALL of a restaurant's stories (active AND
	// inactive) for the admin cabinet, in the same display order as the public
	// read. An absent restaurant lists as an empty slice, not an error.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]Story, error)

	// GetByID returns one story by its id regardless of is_active, or ErrNotFound.
	// The admin update/delete routes carry only the story id, so the usecase
	// resolves the owning restaurant (for the RBAC check) through this read.
	GetByID(ctx context.Context, id uuid.UUID) (*Story, error)

	// Create inserts a new story. An unknown restaurant_id (FK violation) maps to
	// ErrNotFound. CreatedAt is populated on the passed struct.
	Create(ctx context.Context, s *Story) error

	// Update overwrites the mutable fields (image_url, caption, sort_order,
	// is_active) of s.ID, scoped to s.RestaurantID so an id from another tenant
	// can never be updated. A zero-rows update maps to ErrNotFound.
	Update(ctx context.Context, s *Story) error

	// Delete removes the story id, scoped to restaurantID so a caller who manages
	// one venue can never delete another venue's card by guessing its id. A
	// zero-rows delete maps to ErrNotFound.
	Delete(ctx context.Context, id, restaurantID uuid.UUID) error

	// Reorder rewrites sort_order to match the position of each id in orderedIDs
	// (first → 0, second → 1, …), scoped to restaurantID. Ids that do not belong
	// to the restaurant are ignored; ids of the restaurant not present in the
	// list keep their current sort_order.
	Reorder(ctx context.Context, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error
}

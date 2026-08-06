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

// StoryRepository reads restaurant stories.
type StoryRepository interface {
	// ListActiveByRestaurant returns the active stories of restaurantID ordered
	// by sort_order ASC, then created_at ASC (a stable tie-break so two cards
	// sharing a sort_order never reshuffle between reads). Inactive cards are
	// never returned. An absent restaurant is not an error — it simply has no
	// stories, so the result is an empty slice.
	ListActiveByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]Story, error)
}

// Package favorites is the application logic for a user's bookmarked
// restaurants. Every operation is scoped by the caller's own user id — there
// is no cross-user read/write surface, so a caller can never reach another
// user's bookmarks. Routes must be registered on a group already protected by
// middleware.Auth (see transport/rest/favorites).
package favorites

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes the current user's favorite-restaurants operations.
type Facade interface {
	// Add bookmarks restaurantID for userID. Idempotent (see
	// domain.FavoriteRepository.Add).
	Add(ctx context.Context, userID, restaurantID uuid.UUID) error
	// Remove un-bookmarks restaurantID for userID. Idempotent (see
	// domain.FavoriteRepository.Remove).
	Remove(ctx context.Context, userID, restaurantID uuid.UUID) error
	// List returns userID's bookmarked, still-active restaurants.
	List(ctx context.Context, userID uuid.UUID) ([]domain.RestaurantListItem, error)
	// FavoriteSet reports which of restaurantIDs are favorited by userID —
	// used by the restaurants catalog to attach an "is_favorite" flag to a
	// listing/detail response for the current caller.
	FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

type facade struct{ repo domain.FavoriteRepository }

// NewFacade constructs the favorites Facade.
func NewFacade(repo domain.FavoriteRepository) Facade { return &facade{repo: repo} }

func (f *facade) Add(ctx context.Context, userID, restaurantID uuid.UUID) error {
	return f.repo.Add(ctx, userID, restaurantID)
}

func (f *facade) Remove(ctx context.Context, userID, restaurantID uuid.UUID) error {
	return f.repo.Remove(ctx, userID, restaurantID)
}

func (f *facade) List(ctx context.Context, userID uuid.UUID) ([]domain.RestaurantListItem, error) {
	return f.repo.ListByUser(ctx, userID)
}

func (f *facade) FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	return f.repo.FavoriteSet(ctx, userID, restaurantIDs)
}

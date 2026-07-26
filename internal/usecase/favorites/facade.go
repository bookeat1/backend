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

// venueStateAttacher fills the public venue state (structured weekly schedule,
// "open now", "accepts online bookings") on catalog rows. Declared here as a
// minimal local port and bound in bootstrap/deps.go to the SAME
// usecase/restaurants.VenueState instance the catalog listing and search use.
//
// Favorites renders the exact same public shape as the catalog
// (transport/rest/restaurants.PublicListItem), so it must go through the exact
// same enrichment. Reading the rows straight from the repository — as this
// facade used to — left every favorited venue with an absent schedule, which a
// client cannot tell apart from "this venue has no hours recorded": the same
// restaurant would show a schedule in the catalog and none in favorites, for
// no reason in the data.
type venueStateAttacher interface {
	AttachList(ctx context.Context, items []domain.RestaurantListItem)
}

type facade struct {
	repo  domain.FavoriteRepository
	venue venueStateAttacher
}

// FacadeOption configures optional facade dependencies without breaking the
// constructor's existing positional callers (tests pass none).
type FacadeOption func(*facade)

// WithVenueState wires the shared catalog enrichment. Left unwired, the
// favorites listing omits those fields exactly like the catalog does — never
// guesses them.
func WithVenueState(v venueStateAttacher) FacadeOption {
	return func(f *facade) { f.venue = v }
}

// NewFacade constructs the favorites Facade.
func NewFacade(repo domain.FavoriteRepository, opts ...FacadeOption) Facade {
	f := &facade{repo: repo}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *facade) Add(ctx context.Context, userID, restaurantID uuid.UUID) error {
	return f.repo.Add(ctx, userID, restaurantID)
}

func (f *facade) Remove(ctx context.Context, userID, restaurantID uuid.UUID) error {
	return f.repo.Remove(ctx, userID, restaurantID)
}

func (f *facade) List(ctx context.Context, userID uuid.UUID) ([]domain.RestaurantListItem, error) {
	items, err := f.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if f.venue != nil {
		f.venue.AttachList(ctx, items)
	}
	return items, nil
}

func (f *facade) FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	return f.repo.FavoriteSet(ctx, userID, restaurantIDs)
}

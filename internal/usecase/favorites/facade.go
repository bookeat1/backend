// Package favorites is the application logic for a user's bookmarked
// restaurants. Every operation is scoped by the caller's own user id — there
// is no cross-user read/write surface, so a caller can never reach another
// user's bookmarks. Routes must be registered on a group already protected by
// middleware.Auth (see transport/rest/favorites).
package favorites

import (
	"context"
	"time"

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

	// AddEvent / RemoveEvent bookmark an event for userID. Idempotent, and
	// series-aware for recurring events (see domain.FavoriteRepository).
	AddEvent(ctx context.Context, userID, eventID uuid.UUID) error
	RemoveEvent(ctx context.Context, userID, eventID uuid.UUID) error
	// AddPromo / RemovePromo bookmark a promo for userID. Idempotent.
	AddPromo(ctx context.Context, userID, promoID uuid.UUID) error
	RemovePromo(ctx context.Context, userID, promoID uuid.UUID) error

	// ListAll returns everything userID has bookmarked — venues, events and
	// promos — in ONE read, each list newest-saved first and carrying the whole
	// card, so the four-tab favorites screen («Все / Рестораны / События /
	// Акции») renders from a single request with no per-item follow-up.
	//
	// Only items the guest can still open are returned; see the repository
	// contract for the exact rule per kind.
	ListAll(ctx context.Context, userID uuid.UUID) (domain.FavoriteCollection, error)
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
	now   func() time.Time
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

// WithClock overrides the instant event/promo visibility windows are evaluated
// against. Tests pin it; production leaves it at time.Now.
func WithClock(now func() time.Time) FacadeOption {
	return func(f *facade) {
		if now != nil {
			f.now = now
		}
	}
}

// NewFacade constructs the favorites Facade.
func NewFacade(repo domain.FavoriteRepository, opts ...FacadeOption) Facade {
	f := &facade{repo: repo, now: time.Now}
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

func (f *facade) AddEvent(ctx context.Context, userID, eventID uuid.UUID) error {
	return f.repo.AddEvent(ctx, userID, eventID)
}

func (f *facade) RemoveEvent(ctx context.Context, userID, eventID uuid.UUID) error {
	return f.repo.RemoveEvent(ctx, userID, eventID)
}

func (f *facade) AddPromo(ctx context.Context, userID, promoID uuid.UUID) error {
	return f.repo.AddPromo(ctx, userID, promoID)
}

func (f *facade) RemovePromo(ctx context.Context, userID, promoID uuid.UUID) error {
	return f.repo.RemovePromo(ctx, userID, promoID)
}

// ListAll reads the three kinds independently and returns them as three typed
// lists.
//
// The three reads always run, even when the caller asked for a single tab: the
// screen shows a count per tab, and a count that excluded the other kinds would
// either be wrong or need a second round trip anyway. A guest's favorites are
// tens of rows, not thousands — this is three indexed lookups, not a scan.
//
// Venue enrichment goes through the SAME shared attacher the catalog and search
// use, for the reason spelled out on venueStateAttacher.
func (f *facade) ListAll(ctx context.Context, userID uuid.UUID) (domain.FavoriteCollection, error) {
	var out domain.FavoriteCollection

	restaurants, err := f.repo.ListRestaurantsByUser(ctx, userID)
	if err != nil {
		return out, err
	}
	if f.venue != nil && len(restaurants) > 0 {
		items := make([]domain.RestaurantListItem, 0, len(restaurants))
		for _, it := range restaurants {
			items = append(items, it.Restaurant)
		}
		f.venue.AttachList(ctx, items)
		for i := range restaurants {
			restaurants[i].Restaurant = items[i]
		}
	}
	out.Restaurants = restaurants

	now := f.now()
	if out.Events, err = f.repo.ListEventsByUser(ctx, userID, now); err != nil {
		return domain.FavoriteCollection{}, err
	}
	if out.Promos, err = f.repo.ListPromosByUser(ctx, userID, now); err != nil {
		return domain.FavoriteCollection{}, err
	}
	return out, nil
}

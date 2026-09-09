// Package homepicks is the application logic for «Выбрали для вас» — the venue
// rail on the main screen.
//
// The whole feature is one decision: WHO chooses the venues. Until migration
// 0090 nobody did — the rail was a side effect of the `is_popular` flag and the
// catalog's `display_order`, computed on the client. Now the platform can name
// the venues and their order by hand, per city, and the old rule stays as the
// answer for every city nobody has curated yet.
//
// RESOLUTION ORDER for a guest, in three steps, first non-empty wins:
//
//  1. the manual list of the guest's city;
//  2. the manual list for ALL cities (domain.HomePicksAllCities), which is how
//     one curated rail can serve a city that has no rail of its own;
//  3. the AUTOMATIC rule — `is_popular = true`, catalog order — which is
//     exactly what the app computed before this package existed.
//
// Step 3 is the load-bearing one. Nothing here is opt-in for the platform: the
// day this ships, every city is at step 3 and every guest sees the rail they
// saw yesterday. The rail can only become curated by somebody deliberately
// saving a list, and it goes back to automatic the moment that list is cleared.
//
// The automatic step deliberately does NOT filter by city. That is not an
// oversight and not a city bug left in place: it is what the app does today
// (GET /restaurants?is_popular=true, no city parameter), and "empty manual list
// = yesterday's rail, exactly" is worth more than fixing an unrelated
// inaccuracy inside a change nobody would connect it to. Curation is the fix:
// a city that wants its own venues gets them by being curated.
package homepicks

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// DefaultLimit is how many venues the rail returns when the caller does not
// say. It matches what the app draws today (RECOMMENDED_LIMIT = 8 in
// use-explore-data.ts); asking for more only moves cards nobody scrolls to.
const DefaultLimit = 8

// MaxLimit caps what a caller may ask for. The rail is merchandising, not a
// catalog page — a client that wants the whole catalog has /restaurants.
const MaxLimit = 50

// cityResolver is the minimal slice of usecase/cities this package needs:
// "which dictionary entry does this written spelling mean". Optional — unwired,
// the city key is used exactly as it arrives, which is the behaviour this
// package had before the dictionary existed.
type cityResolver interface {
	Resolve(ctx context.Context, raw string) (*domain.CityEntry, error)
}

// catalog is the slice of usecase/restaurants this package needs: one filtered
// catalog read, with everything the listing normally attaches (image, cuisines,
// features, venue state). Declared here and bound in bootstrap, so the rail
// never re-implements a catalog query and can never drift from one.
type catalog interface {
	List(ctx context.Context, f domain.RestaurantFilter, vs domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error)
}

// Facade is the rail's application surface: one guest read and the editor's
// read/write pair.
type Facade interface {
	// Guest returns the rail for one city, resolved through the three steps
	// described in the package doc, capped at limit. Deactivated venues are
	// never in it.
	Guest(ctx context.Context, city string, limit int) ([]domain.RestaurantListItem, error)
	// Editor returns ONE city's manual list exactly as stored — in editorial
	// order, INCLUDING venues that are currently deactivated, and empty when
	// nothing is curated. No fallback: the editor is asking "what did I pick",
	// and answering with the automatic rail would make them think they had
	// curated something they had not.
	//
	// Deactivated venues are present here and absent from Guest on purpose:
	// the editor has to be able to see that slot 3 of their rail is currently
	// dark, or the number of cards they see will not match what a guest sees
	// and nobody will be able to explain why. (Same posture as the gastroguide
	// editor's collection detail.)
	Editor(ctx context.Context, city string) ([]domain.RestaurantListItem, error)
	// Replace sets one city's whole list, in order. An empty list clears the
	// curation and hands the city back to the automatic rail.
	Replace(ctx context.Context, city string, restaurantIDs []uuid.UUID) error
}

type facade struct {
	picks   domain.HomePicksRepository
	catalog catalog
	cities  cityResolver
}

// Option configures the facade.
type Option func(*facade)

// WithCityResolver folds the city dictionary (migration 0081) into the KEY of
// the curation.
//
// It matters because the two sides of this feature write the city down
// independently: the panel saves the name it took from the dictionary, and the
// phone sends whatever spelling it has stored on the device — possibly a code,
// possibly a spelling the platform has since renamed. Without the resolver
// those two strings can differ by an alias, the lookup misses, and the owner
// sees a rail they curated quietly not appear. With it, both sides normalize to
// the dictionary's canonical name before the key is used.
func WithCityResolver(r cityResolver) Option {
	return func(f *facade) { f.cities = r }
}

// NewFacade builds the rail's usecase.
func NewFacade(picks domain.HomePicksRepository, c catalog, opts ...Option) Facade {
	f := &facade{picks: picks, catalog: c}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// cityKey folds a written city name to the key the curation is stored under.
// The all-cities key is never resolved: it is not a city name, it is the
// absence of one.
func (f *facade) cityKey(ctx context.Context, city string) string {
	if f.cities == nil || strings.TrimSpace(city) == domain.HomePicksAllCities {
		return city
	}
	entry, err := f.cities.Resolve(ctx, city)
	if err != nil {
		// A dictionary blip must not lose the curation: fall back to the raw
		// spelling, which is what the panel most likely stored anyway.
		slog.Warn("city dictionary lookup failed, using the raw home-picks city key",
			"city", city, "error", err)
		return city
	}
	if entry == nil {
		return city
	}
	return entry.Name
}

func (f *facade) Guest(ctx context.Context, city string, limit int) ([]domain.RestaurantListItem, error) {
	limit = normalizeLimit(limit)
	city = f.cityKey(ctx, city)

	ids, err := f.picks.ListIDs(ctx, city)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 && city != domain.HomePicksAllCities {
		if ids, err = f.picks.ListIDs(ctx, domain.HomePicksAllCities); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return f.automatic(ctx, limit)
	}

	items, err := f.byIDs(ctx, ids, false)
	if err != nil {
		return nil, err
	}
	// A curated list that has gone entirely dark — every venue deactivated —
	// is not a reason to show the guest an empty main screen. It falls back
	// exactly like an empty list does; the curation itself is untouched and
	// comes back the moment a venue is switched on again.
	if len(items) == 0 {
		return f.automatic(ctx, limit)
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (f *facade) Editor(ctx context.Context, city string) ([]domain.RestaurantListItem, error) {
	ids, err := f.picks.ListIDs(ctx, f.cityKey(ctx, city))
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []domain.RestaurantListItem{}, nil
	}
	return f.byIDs(ctx, ids, true)
}

func (f *facade) Replace(ctx context.Context, city string, restaurantIDs []uuid.UUID) error {
	seen := make(map[uuid.UUID]bool, len(restaurantIDs))
	for _, id := range restaurantIDs {
		if id == uuid.Nil {
			return fmt.Errorf("%w: an empty restaurant id in the picks order", domain.ErrValidation)
		}
		if seen[id] {
			// Not de-duplicated silently: an ordered list that names the same
			// venue twice means the panel and the server disagree about the
			// rail, and guessing which slot was meant is guessing. Same rule as
			// the menu's top picks.
			return fmt.Errorf("%w: duplicate restaurant %s in the picks order", domain.ErrValidation, id)
		}
		seen[id] = true
	}
	if len(restaurantIDs) > MaxLimit {
		return fmt.Errorf("%w: at most %d venues may be picked, got %d",
			domain.ErrValidation, MaxLimit, len(restaurantIDs))
	}
	return f.picks.Replace(ctx, f.cityKey(ctx, city), restaurantIDs)
}

// automatic is yesterday's rail, unchanged: the popular venues of the catalog,
// in catalog order.
func (f *facade) automatic(ctx context.Context, limit int) ([]domain.RestaurantListItem, error) {
	popular := true
	items, _, err := f.catalog.List(ctx, domain.RestaurantFilter{
		IsPopular: &popular,
		Page:      1,
		PerPage:   limit,
	}, domain.VenueStateFilter{})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []domain.RestaurantListItem{}
	}
	return items, nil
}

// byIDs reads the picked venues through the ordinary catalog listing and puts
// them back into EDITORIAL order — the listing answers in catalog order, which
// is precisely the order this feature exists to override.
//
// Ids the listing does not answer for simply drop out. That covers both halves
// of "what if a picked venue goes away": deleted (the membership row went with
// it, so it is not even in ids) and deactivated (filtered by the listing unless
// includeInactive). Neither breaks the rail; it just gets shorter.
func (f *facade) byIDs(ctx context.Context, ids []uuid.UUID, includeInactive bool) ([]domain.RestaurantListItem, error) {
	items, _, err := f.catalog.List(ctx, domain.RestaurantFilter{
		IDs:             ids,
		IncludeInactive: includeInactive,
		Page:            1,
		// The page has to be able to hold every picked venue, or the tail of a
		// curated rail would silently disappear behind the default page size.
		PerPage: len(ids),
	}, domain.VenueStateFilter{})
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]domain.RestaurantListItem, len(items))
	for _, it := range items {
		byID[it.Restaurant.ID] = it
	}
	out := make([]domain.RestaurantListItem, 0, len(ids))
	for _, id := range ids {
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	return out, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

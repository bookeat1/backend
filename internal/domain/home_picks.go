package domain

import (
	"context"

	"github.com/google/uuid"
)

// The manual side of «Выбрали для вас» — the venue rail on the main screen
// (migration 0090). The platform picks the venues and their order by hand; a
// city with no manual list falls back to the automatic rule that used to be the
// whole feature (see usecase/homepicks).
//
// This is platform editorial content, exactly like the gastroguide: a venue
// owner must never be able to put themselves onto the main screen, so every
// write here is superadmin-only.

// HomePicksAllCities is the city key of the list shown in every city that has
// no list of its own. It is a real stored value (an empty string in the column), not NULL —
// see migration 0090 for why.
const HomePicksAllCities = ""

// HomePicksRepository persists the manual rail. Both methods take the city KEY
// as stored: a concrete city name, or HomePicksAllCities. Neither method falls
// back to another key — resolution order is a product decision and lives in the
// usecase.
type HomePicksRepository interface {
	// ListIDs returns the venues of one city's list in editorial order.
	// A city with no list yields an empty slice, never an error: "nothing was
	// picked" is a normal state, and it is the state every city is in until
	// somebody opens the panel.
	//
	// The venues are NOT filtered by is_active here — the caller decides,
	// because the guest rail must hide a deactivated venue while the editor's
	// screen must show it.
	ListIDs(ctx context.Context, city string) ([]uuid.UUID, error)
	// Replace sets one city's whole list in one transaction: the given ids, in
	// the given order, and nothing else. An empty ids slice clears the list,
	// which hands the city back to the automatic rule.
	//
	// Whole-list replacement rather than add/remove/reorder because that is
	// what the panel's screen actually does — the editor drags rows around and
	// presses save — and because it makes the ordering atomic instead of a
	// sequence of writes another request could interleave with.
	Replace(ctx context.Context, city string, restaurantIDs []uuid.UUID) error
}

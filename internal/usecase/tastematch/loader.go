// Package tastematch assembles domain.TasteProfile — the ONE shared read of
// a guest's taste that domain.ScoreTasteMatch is scored against. Spec
// foodie-personalization-v1-20260916.md, task BE-1 (§8): BE-2
// (/restaurants/picks), BE-3 (/feed) and BE-4 (/events?sort=for_you) all call
// Loader.LoadTasteProfile instead of each composing this input on its own —
// three independent assemblies of "this guest's taste" is exactly the drift
// the spec's cross-vendor review flagged. Scoring itself (domain.
// ScoreTasteMatch) stays a pure function in domain; this package is only the
// I/O that feeds it.
//
// Layering: the package never imports another domain's concrete repository —
// the three things it needs (the user's row for foodie_budget_tier, the
// cuisine dictionary's venue links, the booking repository's read) are
// declared as minimal local ports in ports.go and bound to the concrete
// implementations in bootstrap/deps.go, same convention as usecase/bookings.
package tastematch

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Loader assembles domain.TasteProfile for one guest. This is the interface
// BE-2/BE-3/BE-4 depend on — never domain.FoodieProfileRepository or
// domain.BookingRepository directly for this purpose.
type Loader interface {
	// LoadTasteProfile never returns domain.ErrNotFound: a guest who never
	// opened the foodie-profile wizard and never booked anything gets a
	// zero-value TasteProfile (empty CuisineCodes/Diets/BookedCuisineCodes/
	// BookedRestaurantIDs, nil Budget) — same "empty, not an error"
	// convention as domain.FoodieProfileRepository.Get and
	// usecase/users.Facade.GetFoodieProfile, which this reuses the shape of.
	LoadTasteProfile(ctx context.Context, userID uuid.UUID) (domain.TasteProfile, error)
}

// userReader is the minimal slice of the user repository this package needs:
// domain.User.FoodieBudgetTier, the single-column half of the foodie profile
// (the multi-value half is domain.FoodieProfileRepository, used directly —
// it already lives in domain and is this package's own natural dependency,
// not "another domain's concrete repository").
type userReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}

// cuisineReader is the minimal slice of the cuisine dictionary repository
// this package needs: turn booked restaurant ids into their cuisine codes.
type cuisineReader interface {
	ListByRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.Cuisine, error)
}

// bookingReader is the minimal slice of the booking repository this package
// needs. See domain.BookingRepository.ListBookedRestaurantIDs for the
// "бронировал" definition (status ∉ {cancelled}).
type bookingReader interface {
	ListBookedRestaurantIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

type loader struct {
	foodie   domain.FoodieProfileRepository
	users    userReader
	cuisines cuisineReader
	bookings bookingReader
}

// NewLoader constructs a Loader. foodie is domain.FoodieProfileRepository
// directly (not a local port): it is domain's own contract for exactly this
// data, the same dependency usecase/users already takes unwrapped.
func NewLoader(foodie domain.FoodieProfileRepository, users userReader, cuisines cuisineReader, bookings bookingReader) Loader {
	return &loader{foodie: foodie, users: users, cuisines: cuisines, bookings: bookings}
}

// LoadTasteProfile reads the caller's foodie profile (explicit cuisines
// mapped through domain.FoodieCuisineDictionaryCodes, diets verbatim, budget
// translated to a PriceCategory) and the cuisines of every restaurant they
// have a non-cancelled booking at.
//
// Four reads, not fewer: the foodie-profile multi-value tables, the user row
// (budget), the booking list, and (only when there IS a booking) the
// cuisine-dictionary lookup for those restaurants. BE-2's own per-request
// budget (spec criterion 13, "не более 4 SQL-запросов на ответ") is a
// DIFFERENT budget that also has to cover candidate venues and the manual
// pick list — reconciling the two is BE-2's job, not this function's; this
// function does the minimum work ITS OWN four inputs require and no more
// (the cuisine lookup is skipped entirely for a guest with no bookings).
func (l *loader) LoadTasteProfile(ctx context.Context, userID uuid.UUID) (domain.TasteProfile, error) {
	prefs, err := l.foodie.Get(ctx, userID)
	if err != nil {
		return domain.TasteProfile{}, err
	}
	u, err := l.users.GetByID(ctx, userID)
	if err != nil {
		return domain.TasteProfile{}, err
	}

	var budget *domain.PriceCategory
	if u.FoodieBudgetTier != nil {
		if pc, ok := domain.FoodieBudgetTierToPriceCategory[*u.FoodieBudgetTier]; ok {
			budget = &pc
		}
	}

	bookedRestaurantIDs, err := l.bookings.ListBookedRestaurantIDs(ctx, userID)
	if err != nil {
		return domain.TasteProfile{}, err
	}

	var bookedCuisineCodes []string
	if len(bookedRestaurantIDs) > 0 {
		byRestaurant, err := l.cuisines.ListByRestaurants(ctx, bookedRestaurantIDs)
		if err != nil {
			return domain.TasteProfile{}, err
		}
		bookedCuisineCodes = dedupCuisineCodes(bookedRestaurantIDs, byRestaurant)
	}

	return domain.TasteProfile{
		CuisineCodes:        domain.MapFoodieCuisinesToDictionaryCodes(prefs.Cuisines),
		Budget:              budget,
		Diets:               prefs.Diets,
		BookedCuisineCodes:  bookedCuisineCodes,
		BookedRestaurantIDs: bookedRestaurantIDs,
	}, nil
}

// dedupCuisineCodes flattens byRestaurant's cuisine codes across every booked
// restaurant, de-duplicates and sorts them — deliberately DB-order
// independent (never trusts ListBookedRestaurantIDs' or ListByRestaurants'
// own row order to be the reproducibility guarantee), so two calls with the
// same underlying data always return byte-identical output regardless of
// which order Postgres happened to walk either table in.
func dedupCuisineCodes(restaurantIDs []uuid.UUID, byRestaurant map[uuid.UUID][]domain.Cuisine) []string {
	seen := make(map[string]struct{}, len(restaurantIDs))
	out := make([]string, 0, len(restaurantIDs))
	for _, rid := range restaurantIDs {
		for _, c := range byRestaurant[rid] {
			if _, dup := seen[c.Code]; dup {
				continue
			}
			seen[c.Code] = struct{}{}
			out = append(out, c.Code)
		}
	}
	sort.Strings(out)
	return out
}

package domain

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// VenueFeature is one entry of the platform-wide venue-feature ("удобства")
// dictionary (migration 0082): Wi-Fi, летняя терраса, намазхана and the like.
//
// It replaces the free-text restaurant_features table, which was dropped by
// the same migration: that table let three different classification axes share
// one column — a real feature («Терраса»), a CUISINE («Восточная кухня») and
// even a district («Коктобе»).
//
// Like the cuisine dictionary (ADR-022), the list is owned by the platform:
// a venue PICKS from it and never invents an entry. Letting venues type their
// own is exactly how the legacy data ended up unusable as a filter.
type VenueFeature struct {
	// Code is the permanent machine key (latin, snake_case). Names get edited
	// and translated, Code does not: the filter travels by code (language
	// independent) and the app keys its bundled icon off it.
	Code string
	Name string
	// NameI18n holds translations; absent locales fall back to Name.
	NameI18n I18n
	// DisplayOrder drives the dictionary listing (ascending, then Name).
	DisplayOrder int
	// IsActive false = hidden. There is no hard delete: venues reference
	// features (the FK is RESTRICT).
	IsActive bool
	// VenueCount is how many ACTIVE venues currently carry this feature. It is
	// a read-side projection, not stored: List fills it, everything else
	// leaves it zero.
	//
	// It exists because a filter value nobody can match is a lie told to the
	// guest — the panel needs to see which features are still empty, and the
	// owner asked to be able to measure demand per feature. The public list
	// deliberately still returns features with zero venues: the owner fills the
	// data himself and hiding them would only delay him (decision 2026-08-25).
	VenueCount int
	ID         uuid.UUID
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NormalizeFeatureKey folds a written feature name to the lookup key stored in
// venue_feature_aliases.alias: trimmed, lower-cased, inner whitespace
// collapsed.
//
// It MUST stay identical to the SQL used in migration 0082
// (`lower(btrim(...))`) plus the whitespace collapse, because the same key is
// produced on both sides: the migration seeds aliases, Go looks them up.
func NormalizeFeatureKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeFeatureKeys maps NormalizeFeatureKey over in, dropping blanks and
// duplicates while preserving first-seen order.
//
// De-duplication is not cosmetic here: the catalog filter emits ONE EXISTS
// subquery per key (AND semantics), so a repeated key would cost an extra
// join for no change in the result.
func NormalizeFeatureKeys(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		k := NormalizeFeatureKey(s)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// VenueFeatureFilter narrows a dictionary listing.
type VenueFeatureFilter struct {
	// IncludeInactive lifts the `is_active = true` restriction. Only the
	// platform's own management screen sets it — a hidden feature has to stay
	// visible to whoever hid it, or it can never be brought back.
	IncludeInactive bool
}

// VenueFeatureRepository persists the feature dictionary and the venue links.
// Get* return ErrNotFound when absent.
type VenueFeatureRepository interface {
	// List returns dictionary entries ordered by display_order, then name,
	// with VenueCount filled.
	List(ctx context.Context, f VenueFeatureFilter) ([]VenueFeature, error)
	GetByID(ctx context.Context, id uuid.UUID) (*VenueFeature, error)
	// Create inserts a new entry AND its own lookup aliases (normalized name +
	// code) atomically. The aliases are not decoration: the catalog filter
	// resolves `?features=` through venue_feature_aliases ONLY — unlike the
	// cuisine filter it has no legacy-string fallback — so an entry without
	// them is invisible to the filter completely; see NormalizeFeatureKey.
	//
	// A duplicate code or a duplicate normalized name returns ErrAlreadyExists,
	// and so does a spelling already owned by another feature — the unique
	// indexes are the guard, never a read-then-write check (two admins can race).
	Create(ctx context.Context, f *VenueFeature) error
	// Update writes name/i18n/order/active in place and ADDS the aliases of
	// the current name/code, never removing the previous ones: a former
	// spelling still means this feature. Same duplicate rule as Create;
	// ErrNotFound when the id is absent.
	Update(ctx context.Context, f *VenueFeature) error

	// ListByRestaurants returns each restaurant's features in link order
	// (position, then display_order). Restaurants with no features are simply
	// absent from the map. One query for a whole page — never per row.
	ListByRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]VenueFeature, error)
	// ResolveIDs returns the features with the given ids, in the order given.
	// An id that does not exist makes the whole call fail with ErrValidation:
	// silently dropping it would tell the venue it saved a feature it did not.
	ResolveIDs(ctx context.Context, ids []uuid.UUID) ([]VenueFeature, error)
	// SetForRestaurant replaces a restaurant's feature set with ids, in the
	// given order (position = index).
	SetForRestaurant(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID) error
}

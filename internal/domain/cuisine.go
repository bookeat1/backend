package domain

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cuisine is one entry of the platform-wide cuisine dictionary (migration
// 0079). It replaces the free-text restaurants.cuisine_type as the SOURCE of
// truth; that column stays, but only as a derived, comma-joined rendering of a
// venue's cuisine set for store builds that read a single string (ADR-022).
//
// The dictionary is country-wide, not per-city: a cuisine does not depend on
// geography — a venue does, and a venue carries the city. Only the platform
// (RoleAdmin) may create or edit entries; a venue picks from the list.
type Cuisine struct {
	// Code is the permanent machine key (latin, snake_case). Names get edited
	// and translated, Code does not: clients key their bundled fallback image
	// off it and a language-independent filter travels by it.
	Code string
	// ImageURL is the round chip image shown in the app. Kept in the
	// dictionary (R2) so a NEW cuisine ships with a picture without a store
	// release. Nil = the client falls back to its bundled asset for Code.
	ImageURL *string
	// DisplayOrder drives the dictionary listing (ascending, then Name).
	DisplayOrder int
	// IsActive false = hidden. There is no hard delete in the API: venues and
	// guest preferences reference cuisines (both FKs are RESTRICT).
	IsActive  bool
	Name      string
	NameI18n  I18n
	ID        uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NormalizeCuisineKey folds a written cuisine name to the lookup key stored in
// cuisine_aliases.alias: trimmed, lower-cased, inner whitespace collapsed.
//
// It MUST stay identical to the SQL used in migration 0079
// (`lower(btrim(...))`) plus the whitespace collapse, because the same key is
// produced on both sides: the migration seeds aliases, Go looks them up.
func NormalizeCuisineKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeCuisineKeys maps NormalizeCuisineKey over in, dropping blanks and
// duplicates while preserving first-seen order.
func NormalizeCuisineKeys(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		k := NormalizeCuisineKey(s)
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

// JoinCuisineNames renders a cuisine set the way the legacy single-string
// field expects it: names separated by ", " in set order. This is the exact
// backward-compatibility contract for restaurants.cuisine_type — an app build
// already in the store reads that one string and must keep working.
//
// lang selects the translation for each name (falling back to the Russian base
// via I18n.Resolve); an empty lang renders the base names, which is what the
// stored cuisine_type column holds.
func JoinCuisineNames(cs []Cuisine, lang string) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		if n := c.NameI18n.Resolve(lang, c.Name); n != "" {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, ", ")
}

// CuisineI18nFromSet builds the i18n counterpart of JoinCuisineNames: one
// comma-joined string per supported locale. A locale is included only when at
// least one cuisine in the set actually has a translation for it — never a
// re-listing of the Russian names under an "en" key, which would look like a
// translation and be a lie.
func CuisineI18nFromSet(cs []Cuisine) I18n {
	out := I18n{}
	for _, lang := range SupportedLocales {
		if lang == LocaleRU {
			continue // ru is the base column, not a translation
		}
		any := false
		for _, c := range cs {
			if v, ok := c.NameI18n[lang]; ok && v != "" {
				any = true
				break
			}
		}
		if !any {
			continue
		}
		out[lang] = JoinCuisineNames(cs, lang)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CuisineFilter narrows a dictionary listing.
type CuisineFilter struct {
	// IncludeInactive lifts the `is_active = true` restriction. Only the
	// platform's own management screen sets it — a hidden cuisine has to stay
	// visible to whoever hid it, or it can never be brought back.
	IncludeInactive bool
}

// CuisineRepository persists the cuisine dictionary and the venue links.
// Get* return ErrNotFound when absent.
type CuisineRepository interface {
	// List returns dictionary entries ordered by display_order, then name.
	List(ctx context.Context, f CuisineFilter) ([]Cuisine, error)
	GetByID(ctx context.Context, id uuid.UUID) (*Cuisine, error)
	// Create inserts a new entry. A duplicate code or a duplicate normalized
	// name returns ErrAlreadyExists — the unique indexes are the guard, never a
	// read-then-write check (two admins can race).
	Create(ctx context.Context, c *Cuisine) error
	// Update writes name/i18n/image/order/active in place. Same duplicate rule
	// as Create; ErrNotFound when the id is absent.
	Update(ctx context.Context, c *Cuisine) error

	// ListByRestaurants returns each restaurant's cuisines in link order
	// (position, then display_order). Restaurants with no cuisines are simply
	// absent from the map. One query for a whole page — never per row.
	ListByRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]Cuisine, error)
	// ResolveIDs returns the cuisines with the given ids, in the order given.
	// An id that does not exist makes the whole call fail with ErrValidation:
	// silently dropping it would tell the venue it saved a cuisine it did not.
	ResolveIDs(ctx context.Context, ids []uuid.UUID) ([]Cuisine, error)
	// SetForRestaurant replaces a restaurant's cuisine set with ids, in the
	// given order (position = index). NOT atomic on its own (delete + inserts)
	// — callers MUST run it inside a TxManager.WithinTx, together with the
	// derived cuisine_type rewrite, so a failure never leaves the venue with
	// a cuisine set and a string that disagree.
	SetForRestaurant(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID) error
}

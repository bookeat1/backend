package domain

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CityEntry is one row of the platform-wide city dictionary (migration 0081).
//
// It is the SOURCE of truth for "which cities exist"; the free-text
// restaurants.city column stays, but only as the backward-compatibility
// rendering a store build reads and sends back as ?city= (ADR-023). The struct
// is deliberately NOT called City: that name is the string type this catalog
// has stored in restaurants.city since migration 0002 and dozens of call sites
// still pass around, and quietly repurposing it would turn a compile error into
// a runtime one.
//
// Only the platform (RoleAdmin) may create or edit entries; a venue picks a
// city, it never invents one.
type CityEntry struct {
	// Code is the permanent machine key (latin, snake_case). Names get edited
	// and translated, Code does not: a language-independent filter travels by
	// it and clients key local assets off it.
	Code string
	// Name is the Russian base name and is ALSO what lands in
	// restaurants.city — the two must agree, which is why a rename rewrites
	// the venues' string in the same transaction (see usecase/cities).
	Name     string
	NameI18n I18n
	// DisplayOrder drives the dictionary listing (ascending, then Name).
	DisplayOrder int
	// IsActive false = hidden. There is no hard delete: venues reference the
	// city (FK RESTRICT) and carry its name as a live string.
	IsActive  bool
	ID        uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NormalizeCityKey folds a written city name to the lookup key stored in
// city_aliases.alias: trimmed, lower-cased, inner whitespace collapsed.
//
// It MUST stay identical to the SQL function city_key(text) created by
// migration 0081, because the same key is produced on both sides: the
// migration and the trigger seed and match aliases in SQL, Go looks them up.
func NormalizeCityKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// CityFilter narrows a dictionary listing.
type CityFilter struct {
	// IncludeInactive lifts the `is_active = true` restriction. Only the
	// platform's own management screen sets it — a hidden city has to stay
	// visible to whoever hid it, or it can never be brought back.
	IncludeInactive bool
}

// CityRepository persists the city dictionary and its spelling aliases.
// Get* return ErrNotFound when absent.
type CityRepository interface {
	// List returns dictionary entries ordered by display_order, then name.
	List(ctx context.Context, f CityFilter) ([]CityEntry, error)
	GetByID(ctx context.Context, id uuid.UUID) (*CityEntry, error)
	// Create inserts a new entry and seeds its own name and code as aliases.
	// A duplicate code or a duplicate normalized name returns ErrAlreadyExists
	// — the unique indexes are the guard, never a read-then-write check (two
	// admins can race).
	Create(ctx context.Context, c *CityEntry) error
	// Update writes name/i18n/order/active in place and keeps the alias table
	// complete: the new name becomes an alias, and the PREVIOUS name is kept
	// as one, so a client that still filters by the old spelling keeps
	// working. Same duplicate rule as Create; ErrNotFound when id is absent.
	//
	// It does NOT rewrite restaurants.city — that is the caller's job, inside
	// the same transaction (see usecase/cities.Update).
	Update(ctx context.Context, c *CityEntry) error
	// Reorder assigns display_order following the given id order. Ids not in
	// the dictionary are ignored rather than failing the batch: reordering is
	// a cosmetic operation and a stale id from a client's list must not block
	// the rest of it.
	Reorder(ctx context.Context, ids []uuid.UUID) error

	// ResolveAlias finds the city a written spelling (a name, a code, a
	// historical spelling) refers to. Returns ErrNotFound when nothing
	// matches — an unknown city string is a normal, expected answer here.
	ResolveAlias(ctx context.Context, raw string) (*CityEntry, error)
	// AddAlias registers an extra spelling for a city. Idempotent: an alias
	// already pointing at the SAME city is a no-op; pointing at another city
	// returns ErrAlreadyExists.
	AddAlias(ctx context.Context, cityID uuid.UUID, alias string) error
}

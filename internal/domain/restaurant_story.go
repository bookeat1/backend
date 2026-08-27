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
//
// ActionURL is the EXTERNAL link the guest is sent to when they tap the card —
// a different thing from ImageURL, which is where the card's PICTURE lives. It
// is optional (nil = tapping the card leads nowhere) and, when set, has passed
// ValidateExternalActionURL, the same validator the event action button uses.
//
// Unlike EventAction there is no companion label: an event draws a BUTTON, and
// a button needs a caption, whereas a story's whole card is the tap target and
// there is nothing to caption.
//
// ExpiresAt is the story's LIFETIME, the second and independent visibility axis
// next to IsActive: IsActive is the venue's hand on the switch, ExpiresAt is the
// timer it may set instead of remembering to flip that switch. nil means "no
// timer, this card never expires" — which is what every story written before
// migration 0088 carries, and what a venue gets when it clears the field.
type Story struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	ImageURL     string
	Caption      *string
	ActionURL    *string
	SortOrder    int
	IsActive     bool
	// ExpiresAt is an INSTANT, not a wall-clock date: it is compared against
	// "now" and never re-read in the venue's own zone. Same shape as
	// Event.EndsAt and otp expiry; see migration 0088 for why the venue's
	// timezone is not part of it.
	ExpiresAt *time.Time
	CreatedAt time.Time
}

// IsExpired reports whether the story's lifetime has run out at now. A story
// with no expiry (nil) is NEVER expired — that is the whole meaning of nil, and
// the reason existing cards survived migration 0088 untouched.
//
// The comparison is strict (expires_at > now is still alive), matching the SQL
// predicate in the repository exactly, so a story cannot be alive to the
// database and expired to the API in the same millisecond.
func (s Story) IsExpired(now time.Time) bool {
	return s.ExpiresAt != nil && !s.ExpiresAt.After(now)
}

// StoryRepository reads and writes restaurant stories. The public guest app uses
// only ListActiveByRestaurant; the rest is the venue/admin cabinet surface.
type StoryRepository interface {
	// ListActiveByRestaurant returns the stories of restaurantID a guest may see
	// at the instant now, ordered by sort_order ASC, then created_at ASC (a
	// stable tie-break so two cards sharing a sort_order never reshuffle between
	// reads). Two things are excluded: inactive cards, and cards whose
	// expires_at has passed — a nil expires_at never expires. An absent
	// restaurant is not an error — it simply has no stories, so the result is an
	// empty slice.
	//
	// now is passed in rather than read from the database clock for the same
	// reason EventRepository.ListPublishedUpcoming takes it: the caller's clock
	// is the one a test can freeze, and one instant is then shared by every
	// query of a request instead of drifting between them.
	//
	// Ordering is unaffected by the filtering: sort_order values are NOT
	// renumbered when a card drops out, so the survivors keep their relative
	// order and the guest simply sees a shorter rail.
	ListActiveByRestaurant(ctx context.Context, restaurantID uuid.UUID, now time.Time) ([]Story, error)

	// ListByRestaurant returns ALL of a restaurant's stories for the admin
	// cabinet — inactive AND expired ones included — in the same display order
	// as the public read. Deliberately clock-free: an expired card must stay in
	// the venue's own list so it can be extended or deleted, rather than
	// vanishing from the cabinet the moment it stops being served to guests.
	// An absent restaurant lists as an empty slice, not an error.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]Story, error)

	// GetByID returns one story by its id regardless of is_active OR expiry, or
	// ErrNotFound. The admin update/delete routes carry only the story id, so the
	// usecase resolves the owning restaurant (for the RBAC check) through this
	// read — an expired card must still be resolvable, otherwise "extend it"
	// and "delete it" would both 404. There is no PUBLIC read by id for stories
	// (the guest app only ever lists a venue's rail), so this is not a way for a
	// guest to reach an expired card; if a public story deep link is ever added
	// it must take now and filter like ListActiveByRestaurant does, the way
	// EventRepository.GetPublicByID already does.
	GetByID(ctx context.Context, id uuid.UUID) (*Story, error)

	// Create inserts a new story. An unknown restaurant_id (FK violation) maps to
	// ErrNotFound. CreatedAt is populated on the passed struct.
	Create(ctx context.Context, s *Story) error

	// Update overwrites the mutable fields (image_url, caption, action_url,
	// sort_order, is_active, expires_at) of s.ID, scoped to s.RestaurantID so an id from another tenant
	// can never be updated. A zero-rows update maps to ErrNotFound.
	Update(ctx context.Context, s *Story) error

	// Delete removes the story id, scoped to restaurantID so a caller who manages
	// one venue can never delete another venue's card by guessing its id. A
	// zero-rows delete maps to ErrNotFound.
	Delete(ctx context.Context, id, restaurantID uuid.UUID) error

	// Reorder rewrites sort_order to match the position of each id in orderedIDs
	// (first → 0, second → 1, …), scoped to restaurantID. Ids that do not belong
	// to the restaurant are ignored; ids of the restaurant not present in the
	// list keep their current sort_order.
	Reorder(ctx context.Context, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error
}

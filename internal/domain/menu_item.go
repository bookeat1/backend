package domain

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// priceRe matches a non-negative decimal with up to 2 fractional digits.
var priceRe = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)

// ValidPrice reports whether s is a well-formed price string (e.g. "4500.00").
func ValidPrice(s string) bool { return priceRe.MatchString(s) }

// PriceStringToMinor converts a well-formed decimal price string (major units,
// e.g. "4500.00" or "4500" or "4500.5") into int64 MINOR units (tiyn), i.e. it
// multiplies by 100. It is exact: the integer and fractional parts are parsed
// as separate integers, never through a float, so there is no rounding error
// (a menu price of "0.10" is exactly 10 tiyn, not 9). It rejects anything that
// is not a valid price (see ValidPrice) so a malformed column value can never
// silently become a wrong amount a guest is charged. This is the single place
// a menu price crosses from the stored decimal string into the money domain's
// integer minor units — pre-order line prices are computed with it, never with
// a client-sent amount.
func PriceStringToMinor(s string) (int64, error) {
	if !ValidPrice(s) {
		return 0, fmt.Errorf("%w: malformed price %q", ErrValidation, s)
	}
	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: price %q out of range", ErrValidation, s)
	}
	var cents int64
	if hasFrac {
		// ValidPrice guarantees 1..2 fractional digits; normalise to exactly 2.
		if len(fracPart) == 1 {
			fracPart += "0"
		}
		cents, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: price %q out of range", ErrValidation, s)
		}
	}
	// Guard the multiplication against overflow (a menu price is never anywhere
	// near this, but an untrusted/corrupt column value must not wrap).
	if whole > (math.MaxInt64-cents)/100 {
		return 0, fmt.Errorf("%w: price %q too large", ErrValidation, s)
	}
	return whole*100 + cents, nil
}

// MenuItem is a dish on a restaurant's menu. Price is a decimal string
// ("4500.00"). Category/Subcategory are free text (not FKs).
//
// Language LABELS the row's own text (nil = the base/Russian row). It is NOT a
// selector for reads: translations belong in the *_i18n maps, and the guest
// listing serves base rows only (see MenuItemRepository.ListByRestaurant).
// Values are normalized to SupportedLocales on write — the import spelled
// Kazakh 'kz', the rest of the system 'kk'.
type MenuItem struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	Name            string
	NameI18n        I18n
	Description     string
	DescriptionI18n I18n
	Price           string
	ImageURL        *string
	IsAvailable     bool
	// IsFeatured marks the dish as an editorial pick for the cross-venue
	// "chef's picks" rail on the main screen. It is independent of
	// IsAvailable: a picked dish that ran out stays picked but drops out of the
	// guest rail until it is available again, so staff do not have to re-pick
	// it after every stop list.
	IsFeatured bool
	// TopPickPosition is the venue's OWN placement of this dish in the
	// «Лучшие позиции» rail of its storefront (1..MenuTopPickLimit), nil when
	// the venue has not marked it. It is deliberately NOT IsFeatured: that flag
	// feeds the cross-venue "chef's picks" rail of the main screen and is a
	// platform-level decision, this one is the venue's own shop window.
	//
	// Like IsFeatured it is independent of IsAvailable: a marked dish that ran
	// out keeps its slot (so staff do not re-mark it after every stop list) but
	// is not served to guests until it is available again.
	TopPickPosition *int
	Category        *string
	CategoryI18n    I18n
	Subcategory     *string
	SubcategoryI18n I18n
	PortionSize     *string
	PortionSizeI18n I18n
	// Language is the label described above, not a read filter.
	Language     *string
	DisplayOrder *int
	Tags         []MenuItemTag
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MenuItemTag is a free-text tag attached to a menu item.
type MenuItemTag struct {
	ID         uuid.UUID
	MenuItemID uuid.UUID
	Tag        string
	CreatedAt  time.Time
}

// MenuItemFilter narrows a menu listing.
//
// There is deliberately NO language field: WHICH dishes a venue serves does not
// depend on the language the guest reads them in. The requested locale is a
// presentation concern and is resolved from the *_i18n maps at the transport
// edge (domain.I18n.Resolve), exactly like every other localized entity. See
// MenuItemRepository.ListByRestaurant for what the row-per-language leftovers
// in the data do instead.
type MenuItemFilter struct {
	RestaurantID uuid.UUID
}

// MenuTopPickLimit is the number of dishes one venue may mark as «Лучшие
// позиции». It matches what the storefront rail actually renders
// (MENU_HIGHLIGHT_LIMIT = 8 in the mobile app): letting a venue mark a ninth
// dish would be letting it mark something no guest can ever see. The same
// bound is enforced by the database (CHECK top_pick_position BETWEEN 1 AND 8
// plus a partial UNIQUE per venue), so it cannot be exceeded by a race, a data
// migration or a second writer.
const MenuTopPickLimit = 8

// MenuHighlightFilter narrows a venue's «Лучшие позиции» rail. RestaurantID is
// required; Limit is clamped by the usecase.
type MenuHighlightFilter struct {
	RestaurantID uuid.UUID
	Limit        int
}

// FeaturedMenuFilter narrows the cross-venue "chef's picks" rail. City is
// required: the rail is a city feed, and showing an Almaty dish to a guest in
// Astana is the same mistake the main feed already refuses to make. Limit is
// clamped by the usecase, not here.
type FeaturedMenuFilter struct {
	City  City
	Limit int
}

// FeaturedMenuItem is one card of the "chef's picks" rail: the dish plus the
// venue it belongs to, so the card can be opened without a second request.
type FeaturedMenuItem struct {
	Item           MenuItem
	RestaurantName string
	RestaurantI18n I18n
}

// MenuItemRepository persists menu items. Get* return ErrNotFound when absent.
type MenuItemRepository interface {
	// ListByRestaurant returns items (with Tags) for f.RestaurantID, ordered by
	// display_order (NULLs last), then name — the SAME set of dishes whatever
	// language the caller reads them in.
	//
	// menu_items.language exists because part of the imported data stores a
	// translation as a SEPARATE ROW (a Kazakh copy of a dish that also exists in
	// Russian). Returning those rows alongside their originals would show the
	// dish twice, so the listing returns the venue's BASE rows (language NULL or
	// 'ru') and lets the *_i18n maps carry the translations. A venue whose menu
	// consists ONLY of non-base rows is the one exception: it gets those rows,
	// because a hidden menu is worse than an untranslated one.
	ListByRestaurant(ctx context.Context, f MenuItemFilter) ([]MenuItem, error)
	GetByID(ctx context.Context, id uuid.UUID) (*MenuItem, error)
	Create(ctx context.Context, m *MenuItem) error
	Update(ctx context.Context, m *MenuItem) error
	Delete(ctx context.Context, id uuid.UUID) error
	SetAvailable(ctx context.Context, id uuid.UUID, available bool) error
	// ListFeatured returns editorially picked dishes ACROSS venues for the main
	// screen, newest pick first. It returns only dishes that are both featured
	// and available, and only from active venues in f.City — a rail that offers
	// a dish from a hidden venue, or one the kitchen has stopped, is worse than
	// a shorter rail. Tags are NOT loaded: the rail card shows a photo, a name,
	// a price and the venue, and loading tags would cost a second query per
	// screen for something nothing renders.
	ListFeatured(ctx context.Context, f FeaturedMenuFilter) ([]FeaturedMenuItem, error)
	// SetFeatured flips is_featured for one item that belongs to restaurantID.
	// The restaurant_id filter is the tenant guard, exactly as in
	// SetAvailableBulk: a manager of one venue cannot promote another venue's
	// dish onto the main screen by guessing an id. Returns ErrNotFound when the
	// item does not exist or belongs elsewhere — the two are deliberately
	// indistinguishable to the caller.
	SetFeatured(ctx context.Context, restaurantID, id uuid.UUID, featured bool) error
	// SetAvailableBulk flips is_available for every item in ids that belongs to
	// restaurantID, in ONE statement. The restaurant_id filter is the tenant
	// guard: ids belonging to another restaurant are silently skipped, never
	// mutated, so a caller cannot stop-list a competitor's menu by guessing item
	// ids. Returns the number of rows actually changed. This is the fast "we ran
	// out" path (stop list); a nil/empty ids slice is a no-op returning 0.
	SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID, available bool) (int, error)
	// ListTopPicks returns the dishes restaurantID has marked as «Лучшие
	// позиции», ordered by top_pick_position ASC, REGARDLESS of availability —
	// the admin panel must see a marked dish that is currently stopped, and the
	// usecase needs the occupied slots to allocate a free one. Filtering
	// unavailable dishes out is the guest-facing read's job, not this one's.
	// Tags are not loaded (the rail card does not render them).
	ListTopPicks(ctx context.Context, restaurantID uuid.UUID) ([]MenuItem, error)
	// SetTopPickPosition writes top_pick_position for one item that belongs to
	// restaurantID; a nil position unmarks it. The restaurant_id filter is the
	// tenant guard, exactly as in SetFeatured: an id belonging to another venue
	// matches zero rows and comes back as ErrNotFound.
	//
	// Taking a slot another dish of the same venue already holds violates the
	// partial UNIQUE index and is reported as ErrAlreadyExists — the caller is
	// expected to pick another slot, NOT to pre-check and hope.
	SetTopPickPosition(ctx context.Context, restaurantID, id uuid.UUID, position *int) error
	// ClearTopPicks unmarks every dish of restaurantID in ONE statement and
	// returns how many rows it changed. It exists so a full re-ordering can be
	// done as clear-then-set inside one transaction, without a slot of the old
	// arrangement colliding with a slot of the new one.
	ClearTopPicks(ctx context.Context, restaurantID uuid.UUID) (int, error)
	// ReplaceTags deletes the item's tags and inserts items (call within a tx).
	ReplaceTags(ctx context.Context, menuItemID uuid.UUID, tags []MenuItemTag) error
}

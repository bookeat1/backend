package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// FoodieOption is one entry of the platform-wide "Фуди-профиль" option
// dictionary (migration 0110, spec
// foodie-profile-admin-dictionaries-20260916.md): a cuisine tile, a diet, an
// allergy or a budget tier the mobile wizard offers a guest, editable by the
// platform (RoleAdmin) instead of living in Go constants
// (internal/domain/foodie_profile.go, pre-migration) and the frontend's
// packages/i18n.
//
// Code is what actually travels: it is the exact string a guest's
// FoodieProfile.Cuisines/Diets/Allergies entry or users.foodie_budget_tier
// holds (migration 0109), and it is IMMUTABLE once created — renaming a
// display name never breaks a stored profile, but changing Code would
// silently detach every guest who already picked it. There is no FK from
// user_foodie_* onto this table (see migration 0110's header): the
// composite key (kind, code) cannot be expressed as one, and a Code is never
// deleted, only hidden (IsActive), so referential integrity holds without
// one.
type FoodieOption struct {
	ID   uuid.UUID
	Kind FoodieOptionKind
	Code string
	Name string
	// NameI18n never carries a "ru" key — the Russian text is Name itself,
	// same convention as Cuisine.NameI18n.
	NameI18n I18n
	// ImageURL is the R2 tile picture. Nil = the client falls back to its
	// bundled asset for Code (spec 3.3) — never a broken tile.
	ImageURL *string
	// Description/DescriptionI18n/PriceLabel/PriceLabelI18n/PriceCategory are
	// used by the mobile wizard ONLY when Kind == FoodieOptionKindBudget
	// (spec §5); every other kind carries them nil/empty.
	Description     *string
	DescriptionI18n I18n
	PriceLabel      *string
	PriceLabelI18n  I18n
	// PriceCategory is the budget tier's equivalent on the venue price scale
	// (PriceLow/PriceMid/PriceHigh). Nil means "no ₸-equivalent yet" (spec
	// 3.10: a new tier can be created without one) — such a tier never
	// contributes to budget_match until an admin sets it.
	PriceCategory *PriceCategory
	// Cuisines are the venue cuisine-dictionary entries this option maps to
	// (🔴1 = A, migration 0110's foodie_option_cuisines) — populated ONLY for
	// Kind == FoodieOptionKindCuisine, always empty for the other three
	// kinds. Sorted by Cuisine.DisplayOrder then Name for a deterministic
	// response, never by link insertion order (the link table carries none).
	Cuisines     []Cuisine
	DisplayOrder int
	// IsActive false = hidden. No hard delete: a guest's stored profile can
	// still reference a hidden code (spec 3.5/3.9, criteria 9/12) and it must
	// keep scoring and keep saving until the guest's next PUT.
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FoodieOptionKind names which of the wizard's four steps an option belongs
// to. A plain string, validated in application code — never a Postgres enum
// (CLAUDE.md).
type FoodieOptionKind string

const (
	FoodieOptionKindCuisine FoodieOptionKind = "cuisine"
	FoodieOptionKindDiet    FoodieOptionKind = "diet"
	FoodieOptionKindAllergy FoodieOptionKind = "allergy"
	FoodieOptionKindBudget  FoodieOptionKind = "budget"
)

// Valid reports whether k is one of the four known kinds.
func (k FoodieOptionKind) Valid() bool {
	switch k {
	case FoodieOptionKindCuisine, FoodieOptionKindDiet, FoodieOptionKindAllergy, FoodieOptionKindBudget:
		return true
	default:
		return false
	}
}

// AffectsMatching reports whether this option currently contributes any
// points to domain.ScoreTasteMatch — the "Влияет на подбор" column of the
// admin table (spec criterion 5). It is a DERIVED read, never a stored flag:
// recomputed from today's cuisine links / price category / dietAxes rule
// set, so relinking a tile or setting a budget's price category changes the
// answer immediately, with no migration and no second source of truth to
// keep in sync.
func (o FoodieOption) AffectsMatching() bool {
	switch o.Kind {
	case FoodieOptionKindCuisine:
		return len(o.Cuisines) > 0
	case FoodieOptionKindBudget:
		return o.PriceCategory != nil
	case FoodieOptionKindDiet:
		// dietAxes (🔴2 = A, taste_match.go) is the one and only place the
		// diet -> venue-signal RULE lives; this only asks "does one exist for
		// this code", never reimplements or edits it.
		return FoodieDietHasVenueAxis(o.Code)
	default: // allergy: v1 personalization never scores allergies (spec §2).
		return false
	}
}

// FoodieOptionFilter narrows a dictionary listing.
type FoodieOptionFilter struct {
	// Kind restricts to one wizard step. Nil lists every kind (the public and
	// admin "all four tabs" reads).
	Kind *FoodieOptionKind
	// IncludeInactive lifts the is_active restriction. Set by the admin
	// listing (an admin must be able to find and restore a hidden option) AND
	// by usecase/tastematch.Loader (a hidden option a guest still has picked
	// must keep resolving to its cuisine/price-category mapping — criterion
	// 12). The public route and the guest-profile validation never set it
	// through this filter; see FoodieOptionRepository.AllExistingCodes for
	// the latter.
	IncludeInactive bool
}

// FoodieOptionRepository persists the foodie-profile option dictionary and
// its cuisine-tile links. Get* return ErrNotFound when absent.
type FoodieOptionRepository interface {
	// List returns dictionary entries ordered by kind, then display_order,
	// then name, then id (a total order — see cuisine.listOrder for the same
	// reasoning). Each cuisine-kind entry carries its Cuisines populated;
	// every other kind's Cuisines is always empty.
	List(ctx context.Context, f FoodieOptionFilter) ([]FoodieOption, error)
	GetByID(ctx context.Context, id uuid.UUID) (*FoodieOption, error)
	// Create inserts a new option. Kind/Code/Name and the duplicate checks
	// are the caller's job (usecase); the unique indexes on (kind, code) and
	// (kind, lower(btrim(name))) are the actual race-safe guard and return
	// ErrAlreadyExists, never a read-then-write check (spec 3.8: two admins
	// can race).
	Create(ctx context.Context, o *FoodieOption) error
	// Update rewrites every column of o EXCEPT its cuisine links (see
	// SetCuisineLinks) — Kind and Code are expected to be UNCHANGED from what
	// GetByID returned; the usecase enforces that before calling this, this
	// method does not re-check it. ErrNotFound when the id is absent.
	Update(ctx context.Context, o *FoodieOption) error
	// SetCuisineLinks replaces optionID's linked cuisine set wholesale
	// (delete + insert, same convention as CuisineRepository.SetForRestaurant
	// — NOT atomic on its own, callers MUST run it inside the same
	// TxManager.WithinTx as the Create/Update it accompanies). Only valid for
	// a cuisine-kind option; the usecase enforces that.
	SetCuisineLinks(ctx context.Context, optionID uuid.UUID, cuisineIDs []uuid.UUID) error
	// CountActive returns how many options of kind currently have
	// is_active = true. It is the read behind "cannot hide the last active
	// option of its kind" (criterion 8) — a dedicated count instead of
	// List+len so hiding one option is one small query, not a full
	// dictionary read with its cuisine-link joins.
	CountActive(ctx context.Context, kind FoodieOptionKind) (int, error)
	// AllExistingCodes returns every code that exists, keyed by kind, ACTIVE
	// OR HIDDEN. This is the ONE read PUT /users/me/foodie-profile needs
	// (criterion 9, "одно чтение всех кодов за запрос"): a code a guest
	// submits is accepted as long as it exists at all, regardless of
	// is_active — an old store build's hardcoded list must never be told
	// "unknown id" for an option the admin has since hidden. One query
	// covering all four kinds, not four separate ones, so the guest-profile
	// save costs exactly one extra round trip.
	AllExistingCodes(ctx context.Context) (map[FoodieOptionKind]map[string]struct{}, error)
}

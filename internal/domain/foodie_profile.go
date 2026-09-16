package domain

import (
	"context"

	"github.com/google/uuid"
)

// FoodieProfile is the guest-facing "Фуди-профиль" 4-step wizard state
// (mobile PR #222): cuisines/diets/allergies picked from a static,
// frontend-owned list of string ids, plus an optional single budget tier.
//
// These ids are deliberately NOT the cuisine dictionary (Cuisine, migration
// 0079) that venues pick from — see migration 0109's header for why merging
// the two would be wrong. Each id is a named Go string constant below,
// checked with ValidFoodieCuisineID/ValidFoodieDietID/ValidFoodieAllergyID/
// ValidFoodieBudgetTier — never a Postgres enum or a DB CHECK against this
// list, per CLAUDE.md's "VARCHAR + app-side validation, no DB enums": the
// wizard's option set is expected to grow without a migration.
type FoodieProfile struct {
	// Cuisines holds at most FoodieCuisineSelectionLimit ids, each one of
	// FoodieCuisineIDs.
	Cuisines []string
	// Diets holds diet ids, each one of FoodieDietIDs. FoodieDietExclusiveID
	// is exclusive: when present, it is the ONLY entry.
	Diets []string
	// Allergies holds allergy ids, each one of FoodieAllergyIDs. No limit,
	// no exclusivity.
	Allergies []string
	// Budget is nil when the guest has not answered the (optional) budget
	// step. When set, it is one of FoodieBudgetTierIDs.
	Budget *string
}

// FoodieCuisineSelectionLimit is the wizard's hard cap on the cuisine step:
// a tap on a 6th tile is rejected, never silently evicts the oldest pick (see
// the mobile PR's own decision log, foodie-profile-selection.ts).
const FoodieCuisineSelectionLimit = 5

// Cuisine option ids the mobile wizard's cuisine step ships
// (foodie-profile-options.ts, CUISINE_OPTIONS).
const (
	FoodieCuisineKazakh   = "kazakh"
	FoodieCuisineAsian    = "asian"
	FoodieCuisineEuropean = "european"
	FoodieCuisineJapanese = "japanese"
	FoodieCuisineItalian  = "italian"
	FoodieCuisineKorean   = "korean"
	FoodieCuisineSeafood  = "seafood"
	FoodieCuisineMeat     = "meat"
	FoodieCuisineVegan    = "vegan"
	FoodieCuisineDesserts = "desserts"
	FoodieCuisineCoffee   = "coffee"
	FoodieCuisineHealthy  = "healthy"
	FoodieCuisineFastfood = "fastfood"
	FoodieCuisineSpicy    = "spicy"
	FoodieCuisineBBQ      = "bbq"
)

// FoodieCuisineIDs is the closed set of valid FoodieProfile.Cuisines entries.
var FoodieCuisineIDs = []string{
	FoodieCuisineKazakh, FoodieCuisineAsian, FoodieCuisineEuropean, FoodieCuisineJapanese,
	FoodieCuisineItalian, FoodieCuisineKorean, FoodieCuisineSeafood, FoodieCuisineMeat,
	FoodieCuisineVegan, FoodieCuisineDesserts, FoodieCuisineCoffee, FoodieCuisineHealthy,
	FoodieCuisineFastfood, FoodieCuisineSpicy, FoodieCuisineBBQ,
}

// Diet option ids the wizard's diet step ships (DIET_OPTIONS).
// FoodieDietExclusiveID ("no_diet") is exclusive: it cannot coexist with any
// other diet id — picking it clears every other diet, and picking any other
// diet clears it.
const (
	FoodieDietExclusiveID = "no_diet"
	FoodieDietVegan       = "vegan"
	FoodieDietPescetarian = "pescetarian"
	FoodieDietHalal       = "halal"
	FoodieDietKosher      = "kosher"
	FoodieDietKeto        = "keto"
	FoodieDietLowCarb     = "low_carb"
	FoodieDietPaleo       = "paleo"
	FoodieDietNoLactose   = "no_lactose"
	FoodieDietNoGluten    = "no_gluten"
)

// FoodieDietIDs is the closed set of valid FoodieProfile.Diets entries.
var FoodieDietIDs = []string{
	FoodieDietExclusiveID, FoodieDietVegan, FoodieDietPescetarian, FoodieDietHalal,
	FoodieDietKosher, FoodieDietKeto, FoodieDietLowCarb, FoodieDietPaleo,
	FoodieDietNoLactose, FoodieDietNoGluten,
}

// Allergy option ids the wizard's allergy step ships (ALLERGY_OPTIONS).
const (
	FoodieAllergyNuts      = "nuts"
	FoodieAllergyDairy     = "dairy"
	FoodieAllergyEggs      = "eggs"
	FoodieAllergySeafood   = "seafood"
	FoodieAllergySoy       = "soy"
	FoodieAllergyWheat     = "wheat"
	FoodieAllergyShellfish = "shellfish"
	FoodieAllergySesame    = "sesame"
)

// FoodieAllergyIDs is the closed set of valid FoodieProfile.Allergies
// entries.
var FoodieAllergyIDs = []string{
	FoodieAllergyNuts, FoodieAllergyDairy, FoodieAllergyEggs, FoodieAllergySeafood,
	FoodieAllergySoy, FoodieAllergyWheat, FoodieAllergyShellfish, FoodieAllergySesame,
}

// Budget tier ids the wizard's (optional) budget step ships (BUDGET_TIERS).
const (
	FoodieBudgetTierBudget  = "budget"
	FoodieBudgetTierMid     = "mid"
	FoodieBudgetTierPremium = "premium"
)

// FoodieBudgetTierIDs is the closed set of valid FoodieProfile.Budget
// values.
var FoodieBudgetTierIDs = []string{FoodieBudgetTierBudget, FoodieBudgetTierMid, FoodieBudgetTierPremium}

func contains(set []string, id string) bool {
	for _, s := range set {
		if s == id {
			return true
		}
	}
	return false
}

// ValidFoodieCuisineID reports whether id is one of FoodieCuisineIDs.
func ValidFoodieCuisineID(id string) bool { return contains(FoodieCuisineIDs, id) }

// ValidFoodieDietID reports whether id is one of FoodieDietIDs.
func ValidFoodieDietID(id string) bool { return contains(FoodieDietIDs, id) }

// ValidFoodieAllergyID reports whether id is one of FoodieAllergyIDs.
func ValidFoodieAllergyID(id string) bool { return contains(FoodieAllergyIDs, id) }

// ValidFoodieBudgetTier reports whether tier is one of FoodieBudgetTierIDs.
func ValidFoodieBudgetTier(tier string) bool { return contains(FoodieBudgetTierIDs, tier) }

// FoodieProfilePreferences is the multi-value half of FoodieProfile (budget
// is a single column on users, see UserRepository) — what
// FoodieProfileRepository reads and replaces.
type FoodieProfilePreferences struct {
	Cuisines  []string
	Diets     []string
	Allergies []string
}

// FoodieProfileRepository stores the multi-value picks (cuisines/diets/
// allergies) of a guest's foodie profile. Budget lives on users.
// foodie_budget_tier instead (a single nullable value, same shape as
// country_code/birth_date) and is read/written through UserRepository.
type FoodieProfileRepository interface {
	// Get returns the user's current picks. Empty slices, never an error,
	// for a user who never filled the wizard.
	Get(ctx context.Context, userID uuid.UUID) (FoodieProfilePreferences, error)
	// Replace deletes the user's existing picks in all three tables and
	// inserts prefs. NOT atomic on its own (delete + inserts are separate
	// statements per table) — callers MUST run it inside a
	// TxManager.WithinTx, same convention as UserCuisinePreferenceRepository.
	Replace(ctx context.Context, userID uuid.UUID, prefs FoodieProfilePreferences) error
}

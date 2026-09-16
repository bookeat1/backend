package domain

import (
	"math"

	"github.com/google/uuid"
)

// TasteProfile is the guest-side input of ScoreTasteMatch: everything known
// about one guest's taste, already resolved to the vocabularies a venue's
// own signals speak — cuisine DICTIONARY codes (not wizard tile ids),
// PriceCategory (not a wizard budget string). Assembling it is
// usecase/tastematch.Loader.LoadTasteProfile's ONE job (spec
// foodie-personalization-v1-20260916.md §5.1/§8, BE-1): every caller
// (BE-2 /restaurants/picks, BE-3 /feed, BE-4 /events?sort=for_you) uses that
// loader instead of composing this struct itself, so the three surfaces can
// never quietly disagree about what "this guest's taste" means.
type TasteProfile struct {
	// CuisineCodes are the guest's EXPLICIT wizard cuisine picks, already
	// expanded through FoodieCuisineDictionaryCodes (§5.2) into cuisine
	// dictionary codes, de-duplicated. Empty when the guest never picked a
	// cuisine tile, OR picked only tiles with no dictionary mapping
	// (korean/desserts/coffee/healthy/fastfood/spicy/meat/bbq before 🔴2) —
	// ScoreTasteMatch cannot and must not tell the two apart, both mean "no
	// explicit signal, fall back to implicit".
	CuisineCodes []string
	// Budget is the guest's wizard budget tier translated to PriceCategory
	// (FoodieBudgetTierToPriceCategory). Nil when the guest never answered
	// the budget step.
	Budget *PriceCategory
	// Diets are the guest's wizard diet ids (FoodieDietIDs), verbatim. May
	// include FoodieDietExclusiveID ("no_diet"), which ScoreTasteMatch
	// deliberately never scores (§5.2: "no_diet → сигнал не считается").
	Diets []string
	// BookedCuisineCodes are the cuisine dictionary codes of every restaurant
	// the guest has a non-cancelled booking at (§5.1: status ∉ {cancelled};
	// an auto no_show still counts — it is the no-show WORKER closing an
	// unanswered booking, not the guest declining it), de-duplicated and
	// sorted for determinism. Used both as the IMPLICIT cuisine signal
	// (cuisine_match_implicit) when CuisineCodes is empty, and as the
	// booked_similar signal's comparison set.
	BookedCuisineCodes []string
	// BookedRestaurantIDs are the same bookings' restaurant ids. Kept
	// alongside BookedCuisineCodes (rather than derived from it) because
	// booked_similar needs to tell "this candidate venue IS one the guest
	// already booked" (no bonus — that is recognition, not a similarity
	// recommendation) from "a DIFFERENT booked venue shares a cuisine with
	// this candidate" (the bonus).
	BookedRestaurantIDs []uuid.UUID
}

// VenueTasteSignals is the venue-side input of ScoreTasteMatch: only values
// that exist on the candidate venue's own read model, nothing computed from
// the guest. Assembling it is each caller's own job (BE-2/3/4) — unlike
// TasteProfile it has no shared loader, because "which venues are
// candidates" differs per surface (picks vs. feed cards vs. events).
type VenueTasteSignals struct {
	RestaurantID uuid.UUID
	// CuisineCodes are the venue's cuisine dictionary codes IN POSITION ORDER
	// (CuisineRepository.ListByRestaurants' own order) — position 0 is the
	// venue's "main" cuisine, which is also the grouping key DiversifyByKey
	// is meant to be called with for the cuisine-diversity rule (§5.5.3).
	CuisineCodes []string
	// PriceCategory is the venue's price tier. The zero value ("") is a
	// valid input meaning "not set" — checked with PriceCategory.Valid(),
	// never assumed non-empty.
	PriceCategory PriceCategory
	// FeatureCodes are the venue's venue-feature dictionary codes
	// (VenueFeatureRepository.ListByRestaurants), unordered — diet_match
	// only tests membership, never position.
	FeatureCodes []string
	// IsPopular mirrors Restaurant.IsPopular (already resolved from *bool by
	// the caller: nil and false both mean "not popular" here).
	IsPopular bool
	// EditorialPick is true when the venue is on the manual "Выбрали для
	// вас" list of the requested city, or of HomePicksAllCities (§0.1 /
	// criterion 12) — resolved by the caller, ScoreTasteMatch does not know
	// about HomePicksRepository.
	EditorialPick bool
	// Rating / ReviewCount are the venue's published-review aggregate, same
	// fields and same meaning as FeedSignals.Rating/ReviewCount. Not listed
	// in §5.3's abbreviated VenueTasteSignals field list, but required by the
	// venue_rating row of the SAME formula's points table and by criterion 4's
	// test list ("venue_rating при разных рейтингах включая 0-отзывов-кейс") —
	// omitting them would make that signal untestable. Today's callers always
	// pass ReviewCount 0 (§5.1: "Отзывов 0"), so venue_rating is always 0 in
	// production; the field exists so it is not a second migration away when
	// reviews arrive.
	Rating      float64
	ReviewCount int
}

// TasteSignalCode names one ScoreTasteMatch signal. Values are the API's own
// vocabulary (§5.6 match.reasons[].code) — a client localizes by code, so
// renaming one is a breaking change to the mobile/web apps, not a refactor.
type TasteSignalCode string

const (
	// TasteSignalCuisineMatch: the guest picked at least one explicit cuisine
	// tile (§5.2-mapped) and shares it with the venue.
	TasteSignalCuisineMatch TasteSignalCode = "cuisine_match"
	// TasteSignalCuisineMatchImplicit: the guest picked NO explicit cuisine,
	// but the venue shares a cuisine with a restaurant they booked before.
	// Mutually exclusive with TasteSignalCuisineMatch — exactly one of the
	// two is ever emitted per call, matching row 1/1' of §5.3's table.
	TasteSignalCuisineMatchImplicit TasteSignalCode = "cuisine_match_implicit"
	TasteSignalDietMatch            TasteSignalCode = "diet_match"
	TasteSignalBudgetMatch          TasteSignalCode = "budget_match"
	TasteSignalBookedSimilar        TasteSignalCode = "booked_similar"
	TasteSignalEditorialPick        TasteSignalCode = "editorial_pick"
	TasteSignalVenueRating          TasteSignalCode = "venue_rating"
	TasteSignalPopular              TasteSignalCode = "popular"
)

// Scoring constants, §5.3's points table. Kept on the SAME integer scale as
// feed_ranking.go's constants (feedRatingMinReviews/feedRatingBaseline/
// feedRatingPointsPerStar/feedRatingMaxPoints are reused directly below for
// TasteSignalVenueRating, not re-declared) — one scale, one place that
// defines what a "point" means.
const (
	tasteCuisineMatchPoints         = 400
	tasteCuisineMatchImplicitPoints = 200
	tasteDietHalalPoints            = 300
	tasteDietVeganCuisinePoints     = 300
	tasteDietVeganFeaturePoints     = 150
	tasteDietGlutenFreePoints       = 300
	tasteDietPescetarianPoints      = 150
	tasteBudgetSameTierPoints       = 200
	tasteBudgetAdjacentTierPoints   = 80
	tasteBookedSimilarPoints        = 100
	tasteEditorialPickPoints        = 150
	tastePopularPoints              = 50
)

// TasteMatchMaxCoreScore is the maximum a venue can score from the FOUR
// guest-preference signals alone — cuisine_match (400) + diet_match (300) +
// budget_match (200) + booked_similar (100) — criterion 4 of the spec.
// editorial_pick, venue_rating and popular are real ScoreTasteMatch signals
// too (§5.3's table has all seven rows, and criterion 4's own test list
// requires covering them here), but they are deliberately NOT "taste": an
// editorial pin, a review average and a popularity flag say nothing about
// whether THIS guest would like THIS venue, so they are excluded from this
// constant on purpose. A caller that needs the guest-preference sub-score
// alone (nothing else in this codebase does yet) sums just those four
// reasons' Points instead of using Total.
const TasteMatchMaxCoreScore = tasteCuisineMatchPoints + tasteDietHalalPoints + tasteBudgetSameTierPoints + tasteBookedSimilarPoints

// Venue-feature / cuisine dictionary codes the diet axes below test
// membership against (§5.2's "Диеты → сигналы заведения" paragraph).
const (
	dietVenueFeatureHalal          = "halal"
	dietVenueFeatureVegetarianMenu = "vegetarian_menu"
	dietVenueFeatureGlutenFreeMenu = "gluten_free_menu"
	dietVenueCuisineVegan          = "vegan"
	dietVenueCuisineSeafood        = "seafood"
)

// dietAxis is one row of §5.2's diet→venue-signal table: a wizard diet id
// and how to score it against a venue. score never returns a negative value
// and never panics on a venue missing every field.
type dietAxis struct {
	id    string
	score func(VenueTasteSignals) (points int, detail string)
}

// dietAxes lists every diet id that contributes to diet_match, in the FIXED
// order of §5.2's table. This order — not the caller-supplied
// TasteProfile.Diets order — is what ScoreTasteMatch scans in, and is the
// tie-break when a guest picked several diets that would score the same 0:
// see the loop in ScoreTasteMatch. FoodieDietExclusiveID ("no_diet") is
// deliberately ABSENT: §5.2 says its signal is never considered, so it must
// never be able to win the max.
var dietAxes = []dietAxis{
	{id: FoodieDietHalal, score: func(v VenueTasteSignals) (int, string) {
		if containsString(v.FeatureCodes, dietVenueFeatureHalal) {
			return tasteDietHalalPoints, "venue offers halal"
		}
		return 0, "no diet data for venue"
	}},
	{id: FoodieDietVegan, score: func(v VenueTasteSignals) (int, string) {
		if containsString(v.CuisineCodes, dietVenueCuisineVegan) {
			return tasteDietVeganCuisinePoints, "venue's cuisine is vegan"
		}
		if containsString(v.FeatureCodes, dietVenueFeatureVegetarianMenu) {
			return tasteDietVeganFeaturePoints, "venue has a vegetarian menu"
		}
		return 0, "no diet data for venue"
	}},
	{id: FoodieDietNoGluten, score: func(v VenueTasteSignals) (int, string) {
		if containsString(v.FeatureCodes, dietVenueFeatureGlutenFreeMenu) {
			return tasteDietGlutenFreePoints, "venue has a gluten-free menu"
		}
		return 0, "no diet data for venue"
	}},
	{id: FoodieDietPescetarian, score: func(v VenueTasteSignals) (int, string) {
		if containsString(v.CuisineCodes, dietVenueCuisineSeafood) {
			return tasteDietPescetarianPoints, "venue's cuisine includes seafood"
		}
		return 0, "no diet data for venue"
	}},
	// kosher/keto/low_carb/paleo/no_lactose have no venue signal to test at
	// all today (§5.2: "нет оси, 0 с detail: 'no venue axis'") — structurally
	// different from the rows above, which DO have an axis but this
	// particular venue may not match it ("no diet data for venue").
	{id: FoodieDietKosher, score: noVenueDietAxis},
	{id: FoodieDietKeto, score: noVenueDietAxis},
	{id: FoodieDietLowCarb, score: noVenueDietAxis},
	{id: FoodieDietPaleo, score: noVenueDietAxis},
	{id: FoodieDietNoLactose, score: noVenueDietAxis},
}

func noVenueDietAxis(VenueTasteSignals) (int, string) { return 0, "no venue axis" }

// FoodieCuisineDictionaryCodes maps a wizard cuisine tile id
// (FoodieCuisineIDs) to the cuisine-dictionary codes (Cuisine.Code) it
// stands for, per spec §5.2. A tile absent from this map — korean (dictionary
// code does not exist yet, 🔴2), meat/bbq/desserts/coffee/healthy/fastfood/
// spicy (no axis at all) — has NO entry, on purpose: MapFoodieCuisinesToDictionaryCodes
// and ScoreTasteMatch treat a missing key exactly like an empty slice (0
// points, never a panic or an error), which is what criterion 3 requires.
//
// This is the ONLY place the wizard-tile ↔ dictionary mapping is written
// down; changing it (e.g. once the superadmin adds `korean` to the
// dictionary) is a one-line, no-migration change (§0 answer 2).
var FoodieCuisineDictionaryCodes = map[string][]string{
	FoodieCuisineKazakh:   {"kazakh"},
	FoodieCuisineAsian:    {"pan_asian", "japanese", "indian"},
	FoodieCuisineEuropean: {"european", "french", "mediterranean", "greek"},
	FoodieCuisineJapanese: {"japanese"},
	FoodieCuisineItalian:  {"italian"},
	FoodieCuisineSeafood:  {"seafood"},
	FoodieCuisineVegan:    {"vegan"},
}

// MapFoodieCuisinesToDictionaryCodes expands wizard tile ids through
// FoodieCuisineDictionaryCodes into cuisine-dictionary codes, de-duplicated.
// A tile with no entry in the table (unmapped tile, or an unknown id)
// contributes nothing — never an error, matching criterion 3.
func MapFoodieCuisinesToDictionaryCodes(tiles []string) []string {
	out := make([]string, 0, len(tiles))
	seen := make(map[string]struct{}, len(tiles))
	for _, tile := range tiles {
		for _, code := range FoodieCuisineDictionaryCodes[tile] {
			if _, dup := seen[code]; dup {
				continue
			}
			seen[code] = struct{}{}
			out = append(out, code)
		}
	}
	return out
}

// FoodieBudgetTierToPriceCategory bridges the wizard's budget tier ids
// (FoodieBudgetTierIDs) to the venue-side PriceCategory scale, per §5.2:
// "budget ↔ ₸, mid ↔ ₸₸, premium ↔ ₸₸₸".
var FoodieBudgetTierToPriceCategory = map[string]PriceCategory{
	FoodieBudgetTierBudget:  PriceLow,
	FoodieBudgetTierMid:     PriceMid,
	FoodieBudgetTierPremium: PriceHigh,
}

// priceCategoryTier orders PriceCategory for the budget_match "adjacent
// tier" rule (§5.3 row 3): ₸=0, ₸₸=1, ₸₸₸=2, so |guest - venue| is the
// number of tiers apart.
var priceCategoryTier = map[PriceCategory]int{
	PriceLow:  0,
	PriceMid:  1,
	PriceHigh: 2,
}

// TasteMatchReason is one ScoreTasteMatch signal's contribution: the code,
// the points it added (never negative), an optional machine-readable param
// (e.g. which cuisine codes matched — §5.6's match.reasons[].params) and a
// short English detail string for debugging. Like FeedScoreReason, points
// may be 0 — every signal is always reported (criterion 2), because "this
// contributed nothing" is itself the explanation a venue needs.
type TasteMatchReason struct {
	Code   TasteSignalCode
	Points int
	Params map[string]any
	Detail string
}

// ScoreTasteMatch computes a candidate venue's taste-match score for one
// guest, exactly implementing §5.3's points table — all seven signal rows,
// summed. It is deliberately PURE: no clock, no repository, no randomness,
// no float in the total (venue_rating rounds to an int immediately, same as
// ScoreFeedItem), so the same TasteProfile+VenueTasteSignals values always
// produce the same Total and the same Reasons in the same order.
//
// The formula is ADDITIVE, not lexicographic (§5.3's own correction after
// cross-vendor review): every reason's Points is just added to Total, in the
// FIXED order below, regardless of which signals are zero. Nothing here
// decides ELIGIBILITY (is this venue a candidate at all) — that is the
// caller's read model, same separation ScoreFeedItem/FeedEligible keep.
func ScoreTasteMatch(profile TasteProfile, venue VenueTasteSignals) (total int, reasons []TasteMatchReason) {
	reasons = make([]TasteMatchReason, 0, 7)

	// 1 / 1'. Cuisine match — explicit beats implicit, never both: a guest
	// with at least one explicit cuisine tile is scored on ONLY that (row 1);
	// implicit booking history (row 1') is the fallback for a guest who never
	// picked a cuisine tile at all, not an additional bonus on top.
	if len(profile.CuisineCodes) > 0 {
		matched := intersectInOrder(venue.CuisineCodes, profile.CuisineCodes)
		points, detail := 0, "outside the guest's cuisine preferences"
		var params map[string]any
		if len(matched) > 0 {
			points, detail = tasteCuisineMatchPoints, "matches the guest's cuisine preferences"
			params = map[string]any{"cuisine_codes": matched}
		}
		reasons = append(reasons, TasteMatchReason{Code: TasteSignalCuisineMatch, Points: points, Params: params, Detail: detail})
	} else if len(profile.BookedCuisineCodes) == 0 {
		reasons = append(reasons, TasteMatchReason{Code: TasteSignalCuisineMatchImplicit, Points: 0, Detail: "guest has no cuisine preferences"})
	} else {
		matched := intersectInOrder(venue.CuisineCodes, profile.BookedCuisineCodes)
		points, detail := 0, "outside the cuisines the guest booked before"
		var params map[string]any
		if len(matched) > 0 {
			points, detail = tasteCuisineMatchImplicitPoints, "matches a cuisine the guest booked before"
			params = map[string]any{"cuisine_codes": matched}
		}
		reasons = append(reasons, TasteMatchReason{Code: TasteSignalCuisineMatchImplicit, Points: points, Params: params, Detail: detail})
	}

	// 2. Diet match — the MAXIMUM across the guest's diets, never a sum (§5.2:
	// "Берётся максимум по всем диетам гостя, не сумма"), scanned in dietAxes'
	// fixed order so a tie between two zero-scoring diets always reports the
	// same detail regardless of the order FoodieProfileRepository happened to
	// return them in.
	dietPoints, dietDetail, dietConsidered := 0, "guest has no diet", false
	for _, axis := range dietAxes {
		if !containsString(profile.Diets, axis.id) {
			continue
		}
		p, d := axis.score(venue)
		if !dietConsidered || p > dietPoints {
			dietPoints, dietDetail = p, d
		}
		dietConsidered = true
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalDietMatch, Points: dietPoints, Detail: dietDetail})

	// 3. Budget match — equal tier, adjacent tier, or too far apart, per
	// §5.3 row 3. A guest with no budget answer, or a venue with no price
	// category, scores 0 for every venue rather than guessing.
	budgetPoints, budgetDetail := 0, "guest has no budget"
	if profile.Budget != nil {
		if !venue.PriceCategory.Valid() {
			budgetDetail = "venue has no price category"
		} else {
			gTier, vTier := priceCategoryTier[*profile.Budget], priceCategoryTier[venue.PriceCategory]
			diff := gTier - vTier
			if diff < 0 {
				diff = -diff
			}
			switch diff {
			case 0:
				budgetPoints, budgetDetail = tasteBudgetSameTierPoints, "matches the guest's budget tier"
			case 1:
				budgetPoints, budgetDetail = tasteBudgetAdjacentTierPoints, "one price tier from the guest's budget"
			default:
				budgetDetail = "outside the guest's budget tier"
			}
		}
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalBudgetMatch, Points: budgetPoints, Detail: budgetDetail})

	// 4. Booked similar — only considered when the guest has EXPLICIT cuisine
	// picks (§5.3 row 4: "явные кухни есть"); a venue the guest already
	// booked never earns it for itself (recognition, not a recommendation).
	bookedPoints, bookedDetail := 0, "guest has no explicit cuisine preferences"
	if len(profile.CuisineCodes) > 0 {
		switch {
		case containsUUID(profile.BookedRestaurantIDs, venue.RestaurantID):
			bookedDetail = "guest already booked this venue"
		case len(intersectInOrder(venue.CuisineCodes, profile.BookedCuisineCodes)) > 0:
			bookedPoints, bookedDetail = tasteBookedSimilarPoints, "shares a cuisine with a venue the guest booked before"
		default:
			bookedDetail = "no shared cuisine with venues the guest booked before"
		}
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalBookedSimilar, Points: bookedPoints, Detail: bookedDetail})

	// 5. Editorial pick — resolved by the caller (HomePicksRepository, §0
	// answer 1); ScoreTasteMatch only adds the bonus.
	editorialPoints, editorialDetail := 0, "not in the manual pick list"
	if venue.EditorialPick {
		editorialPoints, editorialDetail = tasteEditorialPickPoints, "in the city's manual pick list"
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalEditorialPick, Points: editorialPoints, Detail: editorialDetail})

	// 6. Venue rating — identical rule to ScoreFeedItem's own rating signal
	// (feedRatingMinReviews/feedRatingBaseline/feedRatingPointsPerStar/
	// feedRatingMaxPoints, feed_ranking.go): "одна шкала с feed_ranking.go"
	// per §5.3. Always 0 today (§5.1: zero reviews platform-wide).
	ratingPoints, ratingDetail := 0, "fewer than 5 reviews"
	if venue.ReviewCount >= feedRatingMinReviews {
		above := venue.Rating - feedRatingBaseline
		if above < 0 {
			above = 0
		}
		ratingPoints = int(math.Round(above * feedRatingPointsPerStar))
		if ratingPoints > feedRatingMaxPoints {
			ratingPoints = feedRatingMaxPoints
		}
		ratingDetail = "rated above the credibility floor"
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalVenueRating, Points: ratingPoints, Detail: ratingDetail})

	// 7. Popular.
	popularPoints, popularDetail := 0, "not marked popular"
	if venue.IsPopular {
		popularPoints, popularDetail = tastePopularPoints, "marked popular"
	}
	reasons = append(reasons, TasteMatchReason{Code: TasteSignalPopular, Points: popularPoints, Detail: popularDetail})

	for _, r := range reasons {
		total += r.Points
	}
	return total, reasons
}

// containsString reports whether v is present in set.
func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// containsUUID reports whether v is present in set.
func containsUUID(set []uuid.UUID, v uuid.UUID) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// intersectInOrder returns the elements of pool that are also in set, in
// pool's own order (never set's) — so a caller iterating venue.CuisineCodes
// (position order) gets a deterministic params.cuisine_codes regardless of
// what order the guest's slice happens to be in.
func intersectInOrder(pool []string, set []string) []string {
	if len(pool) == 0 || len(set) == 0 {
		return nil
	}
	lookup := make(map[string]struct{}, len(set))
	for _, s := range set {
		lookup[s] = struct{}{}
	}
	out := make([]string, 0, len(pool))
	for _, p := range pool {
		if _, ok := lookup[p]; ok {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

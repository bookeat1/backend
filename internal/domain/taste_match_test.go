package domain

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func pricePtr(p PriceCategory) *PriceCategory { return &p }

// reasonPoints extracts the code→points map from reasons, mirroring
// feed_ranking_test.go's wantReason pattern: a case pins the signal it is
// about without restating the whole breakdown.
func reasonPoints(reasons []TasteMatchReason) map[TasteSignalCode]int {
	out := make(map[TasteSignalCode]int, len(reasons))
	for _, r := range reasons {
		out[r.Code] = r.Points
	}
	return out
}

func reasonDetail(reasons []TasteMatchReason, code TasteSignalCode) string {
	for _, r := range reasons {
		if r.Code == code {
			return r.Detail
		}
	}
	return ""
}

func TestScoreTasteMatch(t *testing.T) {
	venueID := uuid.New()
	otherVenueID := uuid.New()

	tests := []struct {
		name       string
		profile    TasteProfile
		venue      VenueTasteSignals
		want       int
		wantReason map[TasteSignalCode]int
		wantDetail map[TasteSignalCode]string
	}{
		// --- cuisine_match / cuisine_match_implicit (row 1 / 1') ---
		{
			name:       "explicit cuisine match scores 400",
			profile:    TasteProfile{CuisineCodes: []string{"italian", "seafood"}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"italian"}},
			want:       400,
			wantReason: map[TasteSignalCode]int{TasteSignalCuisineMatch: 400},
			wantDetail: map[TasteSignalCode]string{TasteSignalCuisineMatch: "matches the guest's cuisine preferences"},
		},
		{
			name:       "explicit cuisines present but none match scores 0, not implicit",
			profile:    TasteProfile{CuisineCodes: []string{"italian"}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"european"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalCuisineMatch: 0, TasteSignalCuisineMatchImplicit: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalCuisineMatch: "outside the guest's cuisine preferences"},
		},
		{
			name:       "no explicit cuisines and no booking history: neutral, not a mismatch",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"italian"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalCuisineMatch: 0, TasteSignalCuisineMatchImplicit: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalCuisineMatchImplicit: "guest has no cuisine preferences"},
		},
		{
			name: "implicit cuisine match from booking history scores 200",
			profile: TasteProfile{
				BookedCuisineCodes:  []string{"kazakh"},
				BookedRestaurantIDs: []uuid.UUID{otherVenueID},
			},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"kazakh"}},
			want:       200,
			wantReason: map[TasteSignalCode]int{TasteSignalCuisineMatchImplicit: 200},
			wantDetail: map[TasteSignalCode]string{TasteSignalCuisineMatchImplicit: "matches a cuisine the guest booked before"},
		},
		{
			name: "booking history present but no cuisine overlap scores 0",
			profile: TasteProfile{
				BookedCuisineCodes:  []string{"kazakh"},
				BookedRestaurantIDs: []uuid.UUID{otherVenueID},
			},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"italian"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalCuisineMatchImplicit: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalCuisineMatchImplicit: "outside the cuisines the guest booked before"},
		},

		// --- diet_match (row 2) ---
		{
			name:       "halal diet matches a halal venue for 300",
			profile:    TasteProfile{Diets: []string{FoodieDietHalal}},
			venue:      VenueTasteSignals{RestaurantID: venueID, FeatureCodes: []string{"halal"}},
			want:       300,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 300},
		},
		{
			name:       "halal diet at a venue without the feature scores 0",
			profile:    TasteProfile{Diets: []string{FoodieDietHalal}},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no diet data for venue"},
		},
		{
			name:       "vegan diet matches a vegan CUISINE for 300",
			profile:    TasteProfile{Diets: []string{FoodieDietVegan}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"vegan"}},
			want:       300,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 300},
		},
		{
			name:       "vegan diet matches a vegetarian_menu FEATURE for 150",
			profile:    TasteProfile{Diets: []string{FoodieDietVegan}},
			venue:      VenueTasteSignals{RestaurantID: venueID, FeatureCodes: []string{"vegetarian_menu"}},
			want:       150,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 150},
		},
		{
			name:       "no_gluten diet matches gluten_free_menu for 300",
			profile:    TasteProfile{Diets: []string{FoodieDietNoGluten}},
			venue:      VenueTasteSignals{RestaurantID: venueID, FeatureCodes: []string{"gluten_free_menu"}},
			want:       300,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 300},
		},
		{
			name:       "pescetarian diet matches seafood cuisine for 150",
			profile:    TasteProfile{Diets: []string{FoodieDietPescetarian}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"seafood"}},
			want:       150,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 150},
		},
		{
			name:       "diets without any venue axis always score 0 with a distinct detail",
			profile:    TasteProfile{Diets: []string{FoodieDietKosher}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"vegan"}, FeatureCodes: []string{"halal", "vegetarian_menu", "gluten_free_menu"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no venue axis"},
		},
		{
			name:       "keto diet: no venue axis",
			profile:    TasteProfile{Diets: []string{FoodieDietKeto}},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no venue axis"},
		},
		{
			name:       "low_carb diet: no venue axis",
			profile:    TasteProfile{Diets: []string{FoodieDietLowCarb}},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no venue axis"},
		},
		{
			name:       "paleo diet: no venue axis",
			profile:    TasteProfile{Diets: []string{FoodieDietPaleo}},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no venue axis"},
		},
		{
			name:       "no_lactose diet: no venue axis",
			profile:    TasteProfile{Diets: []string{FoodieDietNoLactose}},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "no venue axis"},
		},
		{
			name:       "no_diet is exclusive and is never scored: same as no diet at all",
			profile:    TasteProfile{Diets: []string{FoodieDietExclusiveID}},
			venue:      VenueTasteSignals{RestaurantID: venueID, FeatureCodes: []string{"halal"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "guest has no diet"},
		},
		{
			name:       "empty diets: guest has no diet",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, FeatureCodes: []string{"halal"}},
			want:       0,
			wantDetail: map[TasteSignalCode]string{TasteSignalDietMatch: "guest has no diet"},
		},
		{
			name:       "diet_match takes the MAX across several diets, not the sum",
			profile:    TasteProfile{Diets: []string{FoodieDietPescetarian, FoodieDietHalal}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"seafood"}, FeatureCodes: []string{"halal"}},
			want:       300, // halal(300) beats pescetarian(150), never 450
			wantReason: map[TasteSignalCode]int{TasteSignalDietMatch: 300},
		},

		// --- budget_match (row 3) ---
		{
			name:       "same price tier scores 200",
			profile:    TasteProfile{Budget: pricePtr(PriceMid)},
			venue:      VenueTasteSignals{RestaurantID: venueID, PriceCategory: PriceMid},
			want:       200,
			wantReason: map[TasteSignalCode]int{TasteSignalBudgetMatch: 200},
			wantDetail: map[TasteSignalCode]string{TasteSignalBudgetMatch: "matches the guest's budget tier"},
		},
		{
			name:       "adjacent price tier scores 80",
			profile:    TasteProfile{Budget: pricePtr(PriceLow)},
			venue:      VenueTasteSignals{RestaurantID: venueID, PriceCategory: PriceMid},
			want:       80,
			wantReason: map[TasteSignalCode]int{TasteSignalBudgetMatch: 80},
		},
		{
			name:       "two tiers apart scores 0",
			profile:    TasteProfile{Budget: pricePtr(PriceLow)},
			venue:      VenueTasteSignals{RestaurantID: venueID, PriceCategory: PriceHigh},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalBudgetMatch: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalBudgetMatch: "outside the guest's budget tier"},
		},
		{
			name:       "guest with no budget scores 0 for every venue",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, PriceCategory: PriceMid},
			want:       0,
			wantDetail: map[TasteSignalCode]string{TasteSignalBudgetMatch: "guest has no budget"},
		},
		{
			name:       "venue with no price category scores 0, never panics",
			profile:    TasteProfile{Budget: pricePtr(PriceMid)},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			want:       0,
			wantDetail: map[TasteSignalCode]string{TasteSignalBudgetMatch: "venue has no price category"},
		},

		// --- booked_similar (row 4) ---
		{
			// venue's cuisine ("georgian") deliberately does not overlap
			// BookedCuisineCodes either, so cuisine_match_implicit stays 0 too
			// and Total isolates booked_similar's own gate.
			name:       "no explicit cuisines: booked_similar never fires",
			profile:    TasteProfile{BookedCuisineCodes: []string{"italian"}, BookedRestaurantIDs: []uuid.UUID{otherVenueID}},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"georgian"}},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalBookedSimilar: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalBookedSimilar: "guest has no explicit cuisine preferences"},
		},
		{
			name: "explicit cuisines + shares a cuisine with a DIFFERENT booked venue: 100",
			profile: TasteProfile{
				CuisineCodes:        []string{"italian"},
				BookedCuisineCodes:  []string{"european"},
				BookedRestaurantIDs: []uuid.UUID{otherVenueID},
			},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"european"}},
			want:       100, // cuisine_match itself is 0 (italian vs european) — the 100 is booked_similar alone
			wantReason: map[TasteSignalCode]int{TasteSignalBookedSimilar: 100},
			wantDetail: map[TasteSignalCode]string{TasteSignalBookedSimilar: "shares a cuisine with a venue the guest booked before"},
		},
		{
			name: "the venue itself is a booked venue: no bonus for recognizing yourself",
			profile: TasteProfile{
				CuisineCodes:        []string{"italian"},
				BookedCuisineCodes:  []string{"european"},
				BookedRestaurantIDs: []uuid.UUID{venueID},
			},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"european"}},
			wantReason: map[TasteSignalCode]int{TasteSignalBookedSimilar: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalBookedSimilar: "guest already booked this venue"},
		},
		{
			name: "explicit cuisines but no shared cuisine with any booked venue: 0",
			profile: TasteProfile{
				CuisineCodes:        []string{"italian"},
				BookedCuisineCodes:  []string{"kazakh"},
				BookedRestaurantIDs: []uuid.UUID{otherVenueID},
			},
			venue:      VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"european"}},
			wantReason: map[TasteSignalCode]int{TasteSignalBookedSimilar: 0},
			wantDetail: map[TasteSignalCode]string{TasteSignalBookedSimilar: "no shared cuisine with venues the guest booked before"},
		},

		// --- editorial_pick (row 5) ---
		{
			name:       "editorial pick adds 150",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, EditorialPick: true},
			want:       150,
			wantReason: map[TasteSignalCode]int{TasteSignalEditorialPick: 150},
		},
		{
			name:       "not an editorial pick adds 0",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalEditorialPick: 0},
		},

		// --- venue_rating (row 6), identical rule to feed_ranking.go ---
		{
			name:       "fewer than 5 reviews scores 0 regardless of rating",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, Rating: 5, ReviewCount: 4},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalVenueRating: 0},
		},
		{
			name:       "zero reviews scores 0 (today's production state)",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, Rating: 0, ReviewCount: 0},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalVenueRating: 0},
		},
		{
			name:       "a 4.5 rating over enough reviews scores 150",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, Rating: 4.5, ReviewCount: 12},
			want:       150,
			wantReason: map[TasteSignalCode]int{TasteSignalVenueRating: 150},
		},
		{
			name:       "a perfect rating is capped at 200",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, Rating: 5, ReviewCount: 40},
			want:       200,
			wantReason: map[TasteSignalCode]int{TasteSignalVenueRating: 200},
		},
		{
			name:       "a below-baseline rating never goes negative",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, Rating: 1.5, ReviewCount: 40},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalVenueRating: 0},
		},

		// --- popular (row 7) ---
		{
			name:       "popular venue adds 50",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID, IsPopular: true},
			want:       50,
			wantReason: map[TasteSignalCode]int{TasteSignalPopular: 50},
		},
		{
			name:       "unpopular venue adds 0",
			profile:    TasteProfile{},
			venue:      VenueTasteSignals{RestaurantID: venueID},
			want:       0,
			wantReason: map[TasteSignalCode]int{TasteSignalPopular: 0},
		},

		// --- everything at once ---
		{
			name: "the four core taste signals sum to TasteMatchMaxCoreScore when everything maxes out",
			profile: TasteProfile{
				CuisineCodes:        []string{"italian"},
				Diets:               []string{FoodieDietHalal},
				Budget:              pricePtr(PriceMid),
				BookedCuisineCodes:  []string{"italian"},
				BookedRestaurantIDs: []uuid.UUID{otherVenueID},
			},
			venue: VenueTasteSignals{
				RestaurantID: venueID, CuisineCodes: []string{"italian"},
				FeatureCodes: []string{"halal"}, PriceCategory: PriceMid,
			},
			want: TasteMatchMaxCoreScore, // editorial/rating/popular all zero here
			wantReason: map[TasteSignalCode]int{
				TasteSignalCuisineMatch: 400, TasteSignalDietMatch: 300,
				TasteSignalBudgetMatch: 200, TasteSignalBookedSimilar: 100,
				TasteSignalEditorialPick: 0, TasteSignalVenueRating: 0, TasteSignalPopular: 0,
			},
		},
		{
			name:    "every signal at once, including the non-taste ones, is a plain sum (additive, not lexicographic)",
			profile: TasteProfile{CuisineCodes: []string{"italian"}, Budget: pricePtr(PriceMid)},
			venue: VenueTasteSignals{
				RestaurantID: venueID, CuisineCodes: []string{"italian"}, PriceCategory: PriceMid,
				EditorialPick: true, IsPopular: true, Rating: 5, ReviewCount: 40,
			},
			want: 400 + 0 + 200 + 0 + 150 + 200 + 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, reasons := ScoreTasteMatch(tt.profile, tt.venue)
			if total != tt.want {
				t.Errorf("total = %d, want %d (breakdown %+v)", total, tt.want, reasons)
			}
			for code, points := range tt.wantReason {
				if got := reasonPoints(reasons)[code]; got != points {
					t.Errorf("reason %s points = %d, want %d (breakdown %+v)", code, got, points, reasons)
				}
			}
			for code, detail := range tt.wantDetail {
				if got := reasonDetail(reasons, code); got != detail {
					t.Errorf("reason %s detail = %q, want %q", code, got, detail)
				}
			}
			// Criterion 2: never a negative point value, ever.
			for _, r := range reasons {
				if r.Points < 0 {
					t.Errorf("reason %s has negative points %d", r.Code, r.Points)
				}
			}
			// Every reason's points must actually sum to Total.
			sum := 0
			for _, r := range reasons {
				sum += r.Points
			}
			if sum != total {
				t.Errorf("reasons sum to %d, Total is %d — they must agree", sum, total)
			}
		})
	}
}

// TestScoreTasteMatchAlwaysReportsSevenReasons pins criterion 2: every call
// reports all seven signals, zero included, regardless of input.
func TestScoreTasteMatchAlwaysReportsSevenReasons(t *testing.T) {
	_, reasons := ScoreTasteMatch(TasteProfile{}, VenueTasteSignals{RestaurantID: uuid.New()})
	if len(reasons) != 7 {
		t.Fatalf("got %d reasons, want 7: %+v", len(reasons), reasons)
	}
	seen := map[TasteSignalCode]bool{}
	for _, r := range reasons {
		seen[r.Code] = true
	}
	for _, code := range []TasteSignalCode{
		TasteSignalCuisineMatchImplicit, TasteSignalDietMatch, TasteSignalBudgetMatch,
		TasteSignalBookedSimilar, TasteSignalEditorialPick, TasteSignalVenueRating, TasteSignalPopular,
	} {
		if !seen[code] {
			t.Errorf("missing reason code %s", code)
		}
	}
}

// TestScoreTasteMatchDeterministic pins criterion 1: the same input, called
// twice, produces the same Total and the same Reasons in the same order.
func TestScoreTasteMatchDeterministic(t *testing.T) {
	profile := TasteProfile{
		CuisineCodes: []string{"italian", "seafood"}, Diets: []string{FoodieDietPescetarian, FoodieDietHalal},
		Budget: pricePtr(PriceMid), BookedCuisineCodes: []string{"kazakh", "italian"},
		BookedRestaurantIDs: []uuid.UUID{uuid.New(), uuid.New()},
	}
	venue := VenueTasteSignals{
		RestaurantID: uuid.New(), CuisineCodes: []string{"seafood", "italian"},
		FeatureCodes: []string{"halal"}, PriceCategory: PriceMid,
		EditorialPick: true, IsPopular: true, Rating: 4.2, ReviewCount: 10,
	}
	total1, reasons1 := ScoreTasteMatch(profile, venue)
	total2, reasons2 := ScoreTasteMatch(profile, venue)
	if total1 != total2 {
		t.Fatalf("total differs between calls: %d vs %d", total1, total2)
	}
	if !reflect.DeepEqual(reasons1, reasons2) {
		t.Fatalf("reasons differ between calls:\n%+v\nvs\n%+v", reasons1, reasons2)
	}
}

func TestMapFoodieCuisinesToDictionaryCodes(t *testing.T) {
	tests := []struct {
		name  string
		tiles []string
		want  []string
	}{
		{name: "nil input yields nil-ish empty output", tiles: nil, want: []string{}},
		{name: "a mapped tile expands to its dictionary codes", tiles: []string{FoodieCuisineKazakh}, want: []string{"kazakh"}},
		{name: "a tile with several codes expands to all of them", tiles: []string{FoodieCuisineAsian}, want: []string{"pan_asian", "japanese", "indian"}},
		{
			name:  "an unmapped tile (no axis, e.g. desserts) contributes nothing, never an error",
			tiles: []string{FoodieCuisineDesserts, FoodieCuisineKazakh},
			want:  []string{"kazakh"},
		},
		{
			name:  "korean has no dictionary code yet (🔴2) and contributes nothing",
			tiles: []string{FoodieCuisineKorean},
			want:  []string{},
		},
		{
			name:  "an entirely unknown tile id contributes nothing, never panics",
			tiles: []string{"not-a-real-tile"},
			want:  []string{},
		},
		{
			name:  "duplicate codes across tiles are de-duplicated (italian and european both list nothing shared here, japanese appears via both asian and japanese tiles)",
			tiles: []string{FoodieCuisineAsian, FoodieCuisineJapanese},
			want:  []string{"pan_asian", "japanese", "indian"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapFoodieCuisinesToDictionaryCodes(tt.tiles)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestScoreTasteMatchCuisineCodesAreMutuallyExclusive pins §5.3 rows 1/1':
// exactly one of cuisine_match / cuisine_match_implicit is ever present in
// Reasons, never both, regardless of what data the guest has.
func TestScoreTasteMatchCuisineCodesAreMutuallyExclusive(t *testing.T) {
	venueID := uuid.New()
	cases := []struct {
		name    string
		profile TasteProfile
	}{
		{"explicit cuisines set", TasteProfile{CuisineCodes: []string{"italian"}}},
		{"only booking history", TasteProfile{BookedCuisineCodes: []string{"italian"}, BookedRestaurantIDs: []uuid.UUID{uuid.New()}}},
		{"neither", TasteProfile{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reasons := ScoreTasteMatch(tc.profile, VenueTasteSignals{RestaurantID: venueID, CuisineCodes: []string{"italian"}})
			hasExplicit, hasImplicit := false, false
			for _, r := range reasons {
				if r.Code == TasteSignalCuisineMatch {
					hasExplicit = true
				}
				if r.Code == TasteSignalCuisineMatchImplicit {
					hasImplicit = true
				}
			}
			if hasExplicit == hasImplicit {
				t.Fatalf("expected exactly one of cuisine_match/cuisine_match_implicit, got explicit=%v implicit=%v (%+v)", hasExplicit, hasImplicit, reasons)
			}
		})
	}
}

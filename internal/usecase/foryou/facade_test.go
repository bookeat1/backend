package foryou

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/homepicks"
)

type fakeLoader struct {
	profile domain.TasteProfile
	err     error
	gotUser uuid.UUID
	calls   int
}

func (f *fakeLoader) LoadTasteProfile(_ context.Context, userID uuid.UUID) (domain.TasteProfile, error) {
	f.gotUser = userID
	f.calls++
	return f.profile, f.err
}

type fakeCatalog struct {
	items     []domain.RestaurantListItem
	err       error
	gotFilter domain.RestaurantFilter
	calls     int
}

func (f *fakeCatalog) List(_ context.Context, flt domain.RestaurantFilter, _ domain.VenueStateFilter) ([]domain.RestaurantListItem, int, error) {
	f.gotFilter = flt
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.items, len(f.items), nil
}

type fakeRail struct {
	items         []domain.RestaurantListItem
	mode          domain.HomePicksMode
	err           error
	manual        map[uuid.UUID]bool
	manualErr     error
	resolvedCalls int
	manualCalls   int
}

func (f *fakeRail) GuestResolved(_ context.Context, _ string, limit int) ([]domain.RestaurantListItem, domain.HomePicksMode, error) {
	f.resolvedCalls++
	if f.err != nil {
		return nil, "", f.err
	}
	items := f.items
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, f.mode, nil
}

func (f *fakeRail) ManualPickIDs(_ context.Context, _ string) (map[uuid.UUID]bool, error) {
	f.manualCalls++
	if f.manualErr != nil {
		return nil, f.manualErr
	}
	return f.manual, nil
}

func venueItem(name string, cuisines []string, price domain.PriceCategory, popular bool, displayOrder *int) domain.RestaurantListItem {
	cs := make([]domain.Cuisine, 0, len(cuisines))
	for _, c := range cuisines {
		cs = append(cs, domain.Cuisine{Code: c})
	}
	var isPopular *bool
	if popular {
		t := true
		isPopular = &t
	}
	return domain.RestaurantListItem{
		Restaurant: domain.Restaurant{
			ID: uuid.New(), Name: name, IsActive: true,
			PriceCategory: price, IsPopular: isPopular, DisplayOrder: displayOrder,
		},
		Cuisines: cs,
	}
}

func intPtr(n int) *int { return &n }

func names(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant.Name)
	}
	return out
}

// criterion 5: an anonymous caller (userID nil) never touches the profile
// loader or the candidate catalog — it goes straight to the SAME fallback
// rail, relabelled.
func TestGuestAnonymousUsesTheFallbackRailUntouched(t *testing.T) {
	a := venueItem("А", nil, "", false, nil)
	loader := &fakeLoader{}
	catalog := &fakeCatalog{}
	rail := &fakeRail{items: []domain.RestaurantListItem{a}, mode: domain.HomePicksModePopular}
	f := NewFacade(loader, catalog, rail)

	res, err := f.Guest(context.Background(), nil, "Алматы", 0)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModePopular {
		t.Fatalf("mode = %q, want popular", res.Mode)
	}
	if len(res.Items) != 1 || res.Items[0].Match != nil {
		t.Fatalf("items = %+v, want one plain (no match) item", res.Items)
	}
	if loader.calls != 0 {
		t.Fatal("an anonymous caller must never load a taste profile")
	}
	if catalog.calls != 0 {
		t.Fatal("an anonymous caller must never read candidates")
	}
}

// criterion 5/§5.4: a signed-in guest whose profile is EMPTY (no cuisines,
// no budget, no venue-axis diet, no bookings) is not "active" — same
// fallback as an anonymous guest, mode from the rail.
func TestGuestInactiveProfileUsesTheFallbackRail(t *testing.T) {
	loader := &fakeLoader{profile: domain.TasteProfile{}}
	rail := &fakeRail{mode: domain.HomePicksModeEditorial}
	f := NewFacade(loader, &fakeCatalog{}, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 0)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModeEditorial {
		t.Fatalf("mode = %q, want editorial", res.Mode)
	}
	if loader.gotUser != userID {
		t.Fatalf("loader called with %v, want %v", loader.gotUser, userID)
	}
}

// criterion 3.4: a profile with only "uncovered" tiles (already mapped to no
// CuisineCodes by the caller of this package — that mapping is BE-1's job) is
// inactive, EXCEPT the diets-with-an-axis / budget / bookings escape hatches.
// Covered by TestGuestInactiveProfileUsesTheFallbackRail's empty case plus
// the four activity paths below.
func TestProfileActiveEachOfTheFourConditionsAloneIsEnough(t *testing.T) {
	cases := []struct {
		name    string
		profile domain.TasteProfile
		want    bool
	}{
		{"empty profile is inactive", domain.TasteProfile{}, false},
		{"explicit cuisine alone", domain.TasteProfile{CuisineCodes: []string{"italian"}}, true},
		{"budget alone", domain.TasteProfile{Budget: func() *domain.PriceCategory { p := domain.PriceMid; return &p }()}, true},
		{"venue-axis diet alone", domain.TasteProfile{Diets: []string{domain.FoodieDietHalal}}, true},
		{"axis-less diet alone is not enough", domain.TasteProfile{Diets: []string{domain.FoodieDietKosher}}, false},
		{"no_diet alone is not enough", domain.TasteProfile{Diets: []string{domain.FoodieDietExclusiveID}}, false},
		{"booking with no explicit cuisine", domain.TasteProfile{BookedRestaurantIDs: []uuid.UUID{uuid.New()}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := profileActive(tc.profile); got != tc.want {
				t.Fatalf("profileActive(%+v) = %v, want %v", tc.profile, got, tc.want)
			}
		})
	}
}

// criterion 6: an active profile with a matching candidate answers
// data.mode = for_you and attaches match.score/reasons to the card.
func TestGuestActiveProfileScoresCandidates(t *testing.T) {
	// Budget deliberately left unset: with it set, PriceHigh vs. the guest's
	// tier could land on the "adjacent tier" 80-point rule and make the
	// georgian venue a (weak) core match too — this test is about cuisine
	// matching alone, budget_match has its own coverage in domain's own
	// ScoreTasteMatch tests.
	profile := domain.TasteProfile{CuisineCodes: []string{"italian"}}
	match := venueItem("Итальянское", []string{"italian"}, domain.PriceMid, false, intPtr(1))
	noMatch := venueItem("Грузинское", []string{"georgian"}, domain.PriceHigh, false, intPtr(2))
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: []domain.RestaurantListItem{noMatch, match}}
	rail := &fakeRail{manual: map[uuid.UUID]bool{}}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	// limit=1: exactly as many candidates match as the row needs, so the
	// fallback rail must not even be consulted for padding.
	res, err := f.Guest(context.Background(), &userID, "Алматы", 1)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModeForYou {
		t.Fatalf("mode = %q, want for_you", res.Mode)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %v, want only the matching venue (the georgian one scores 0 core)", names(res.Items))
	}
	got := res.Items[0]
	if got.Restaurant.Name != "Итальянское" {
		t.Fatalf("item = %s, want Итальянское", got.Restaurant.Name)
	}
	if got.Match == nil {
		t.Fatal("a for_you card must carry a match block")
	}
	// cuisine_match(400) alone — no budget in the profile.
	if got.Match.Score != 400 {
		t.Fatalf("score = %d, want 400", got.Match.Score)
	}
	if rail.resolvedCalls != 0 {
		t.Fatal("no padding needed, the fallback rail must not be consulted")
	}
}

// criterion 9: an active profile whose every candidate scores 0 CORE taste
// points degrades to the fallback rail, not an empty for_you row.
func TestGuestAllCandidatesZeroCoreScoreDegradesToFallback(t *testing.T) {
	low := domain.PriceLow
	profile := domain.TasteProfile{Budget: &low}                                        // no explicit cuisine, no bookings
	farPrice := venueItem("Дорогое", []string{"european"}, domain.PriceHigh, true, nil) // diff=2 tiers → 0
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: []domain.RestaurantListItem{farPrice}}
	popular := venueItem("Популярное", nil, "", true, nil)
	rail := &fakeRail{manual: map[uuid.UUID]bool{}, items: []domain.RestaurantListItem{popular}, mode: domain.HomePicksModePopular}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 8)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModePopular {
		t.Fatalf("mode = %q, want popular (fallback)", res.Mode)
	}
	if !(len(res.Items) == 1 && res.Items[0].Restaurant.Name == "Популярное") {
		t.Fatalf("items = %v, want the fallback rail's own answer", names(res.Items))
	}
}

// criterion 8/§3.7: fewer matched candidates than limit → the tail is padded
// from the fallback rail, in its own order, minus anything already shown,
// each padding card carrying fallback_popular at 0 points.
func TestGuestPadsAShortMatchedRowFromTheFallback(t *testing.T) {
	profile := domain.TasteProfile{CuisineCodes: []string{"italian"}}
	match := venueItem("Совпадение", []string{"italian"}, "", false, nil)
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: []domain.RestaurantListItem{match}}

	padA := venueItem("Добор А", nil, "", true, nil)
	padDup := match // same venue as the already-matched one — must be skipped
	padB := venueItem("Добор Б", nil, "", true, nil)
	rail := &fakeRail{
		manual: map[uuid.UUID]bool{},
		items:  []domain.RestaurantListItem{padA, padDup, padB},
	}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 3)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModeForYou {
		t.Fatalf("mode = %q, want for_you", res.Mode)
	}
	want := []string{"Совпадение", "Добор А", "Добор Б"}
	if got := names(res.Items); !equalNames(got, want) {
		t.Fatalf("items:\n got %v\nwant %v", got, want)
	}
	for _, it := range res.Items[1:] {
		if it.Match == nil || it.Match.Score != 0 || len(it.Match.Reasons) != 1 ||
			it.Match.Reasons[0].Code != fallbackPopularCode || it.Match.Reasons[0].Points != 0 {
			t.Fatalf("padding item %s has match %+v, want a single fallback_popular/0 reason", it.Restaurant.Name, it.Match)
		}
	}
}

// criterion 7 worked example: 6 italian + 2 kazakh candidates, guest matches
// both cuisines equally — the diversity pass must land the kazakh cards at
// (0-indexed) position 2 and 5, i.e. 1-indexed 3 and 6, exactly as the spec's
// own example states, never both pushed to the tail.
func TestGuestDiversityWorkedExample(t *testing.T) {
	profile := domain.TasteProfile{CuisineCodes: []string{"italian", "kazakh"}}
	items := make([]domain.RestaurantListItem, 0, 8)
	for i := 0; i < 6; i++ {
		items = append(items, venueItem("Italian", []string{"italian"}, "", false, intPtr(i)))
	}
	for i := 0; i < 2; i++ {
		items = append(items, venueItem("Kazakh", []string{"kazakh"}, "", false, intPtr(10+i)))
	}
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: items}
	rail := &fakeRail{manual: map[uuid.UUID]bool{}}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 8)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if len(res.Items) != 8 {
		t.Fatalf("items = %d, want 8", len(res.Items))
	}
	var run, maxRun int
	lastKey := ""
	kazakhPositions := []int{}
	for i, it := range res.Items {
		key := it.Restaurant.Name
		if key == lastKey {
			run++
		} else {
			run = 1
			lastKey = key
		}
		if run > maxRun {
			maxRun = run
		}
		if key == "Kazakh" {
			kazakhPositions = append(kazakhPositions, i+1) // 1-indexed
		}
	}
	if maxRun >= 3 {
		t.Fatalf("three or more consecutive same-cuisine cards, order: %v", names(res.Items))
	}
	if len(kazakhPositions) != 2 || kazakhPositions[0] != 3 || kazakhPositions[1] != 6 {
		t.Fatalf("kazakh positions = %v, want [3 6] (spec's own worked example)", kazakhPositions)
	}
}

// criterion 12: a candidate on the manual pick list gets the editorial_pick
// bonus even though it is reached through for_you scoring, not the plain
// rail — the bonus is additive, never a filter.
func TestGuestEditorialPickBonusAppliesInsideForYou(t *testing.T) {
	profile := domain.TasteProfile{CuisineCodes: []string{"italian"}}
	venue := venueItem("Редакторское", []string{"italian"}, "", false, nil)
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: []domain.RestaurantListItem{venue}}
	rail := &fakeRail{manual: map[uuid.UUID]bool{venue.Restaurant.ID: true}}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 8)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	// cuisine_match(400) + editorial_pick(150) = 550.
	if len(res.Items) != 1 || res.Items[0].Match.Score != 550 {
		t.Fatalf("items = %+v, want a single 550-point card", res.Items)
	}
}

// criterion 10: any failure on the personalized path — profile, candidates,
// or the manual-pick read — degrades to the fallback rail, never an error.
func TestGuestDegradesOnEveryFailureAlongThePersonalizedPath(t *testing.T) {
	userID := uuid.New()
	activeProfile := domain.TasteProfile{CuisineCodes: []string{"italian"}}

	t.Run("profile load failure", func(t *testing.T) {
		loader := &fakeLoader{err: errors.New("db down")}
		rail := &fakeRail{mode: domain.HomePicksModePopular}
		f := NewFacade(loader, &fakeCatalog{}, rail)
		res, err := f.Guest(context.Background(), &userID, "Алматы", 0)
		if err != nil {
			t.Fatalf("guest: %v, want 200-with-fallback, never an error", err)
		}
		if res.Mode != ModePopular {
			t.Fatalf("mode = %q, want popular", res.Mode)
		}
	})

	t.Run("candidate read failure", func(t *testing.T) {
		loader := &fakeLoader{profile: activeProfile}
		catalog := &fakeCatalog{err: errors.New("db down")}
		rail := &fakeRail{mode: domain.HomePicksModeEditorial}
		f := NewFacade(loader, catalog, rail)
		res, err := f.Guest(context.Background(), &userID, "Алматы", 0)
		if err != nil {
			t.Fatalf("guest: %v, want 200-with-fallback, never an error", err)
		}
		if res.Mode != ModeEditorial {
			t.Fatalf("mode = %q, want editorial", res.Mode)
		}
	})

	t.Run("manual pick read failure", func(t *testing.T) {
		venue := venueItem("В", []string{"italian"}, "", false, nil)
		loader := &fakeLoader{profile: activeProfile}
		catalog := &fakeCatalog{items: []domain.RestaurantListItem{venue}}
		rail := &fakeRail{manualErr: errors.New("db down"), mode: domain.HomePicksModePopular}
		f := NewFacade(loader, catalog, rail)
		res, err := f.Guest(context.Background(), &userID, "Алматы", 0)
		if err != nil {
			t.Fatalf("guest: %v, want 200-with-fallback, never an error", err)
		}
		if res.Mode != ModePopular {
			t.Fatalf("mode = %q, want popular", res.Mode)
		}
	})
}

// A fallback-padding failure must not turn a good for_you rail into an
// error — it is simply served shorter than the limit.
func TestGuestPaddingFailureLeavesAShorterForYouRowInstead(t *testing.T) {
	profile := domain.TasteProfile{CuisineCodes: []string{"italian"}}
	match := venueItem("Совпадение", []string{"italian"}, "", false, nil)
	loader := &fakeLoader{profile: profile}
	catalog := &fakeCatalog{items: []domain.RestaurantListItem{match}}
	rail := &fakeRail{manual: map[uuid.UUID]bool{}, err: errors.New("db down")}
	f := NewFacade(loader, catalog, rail)

	userID := uuid.New()
	res, err := f.Guest(context.Background(), &userID, "Алматы", 5)
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	if res.Mode != ModeForYou {
		t.Fatalf("mode = %q, want for_you", res.Mode)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items = %v, want exactly the one matched card (padding failed)", names(res.Items))
	}
}

// criterion 11: limit defaults to 8 and is capped at 50, same as
// usecase/homepicks.
func TestNormalizeLimitMatchesHomepicksDefaults(t *testing.T) {
	if got := normalizeLimit(0); got != homepicks.DefaultLimit {
		t.Fatalf("normalizeLimit(0) = %d, want %d", got, homepicks.DefaultLimit)
	}
	if got := normalizeLimit(-5); got != homepicks.DefaultLimit {
		t.Fatalf("normalizeLimit(-5) = %d, want %d", got, homepicks.DefaultLimit)
	}
	if got := normalizeLimit(10_000); got != homepicks.MaxLimit {
		t.Fatalf("normalizeLimit(10000) = %d, want %d", got, homepicks.MaxLimit)
	}
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

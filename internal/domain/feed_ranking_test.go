package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// rankNow is the frozen instant every case below is anchored to. Never
// time.Now(): a bucket boundary would then flip depending on when CI runs.
var rankNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestScoreFeedItem(t *testing.T) {
	tests := []struct {
		name    string
		signals FeedSignals
		want    int
		// wantReason asserts the points attributed to ONE code, so a case can
		// pin the signal it is about without restating the whole breakdown.
		wantReason map[FeedSignalCode]int
	}{
		{
			// The floor: nothing bought, nothing fresh, nothing urgent, an
			// unrated venue, an anonymous guest.
			name: "organic old item at an unrated venue scores zero",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:    rankNow.Add(30 * 24 * time.Hour),
			},
			want: 0,
			wantReason: map[FeedSignalCode]int{
				FeedSignalPlacement:   0,
				FeedSignalFreshness:   0,
				FeedSignalEndingSoon:  0,
				FeedSignalVenueRating: 0,
			},
		},
		{
			name: "full paid placement is 1000 points",
			signals: FeedSignals{
				PlacementWeight: 100,
				CreatedAt:       rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:          rankNow.Add(30 * 24 * time.Hour),
			},
			want:       1000,
			wantReason: map[FeedSignalCode]int{FeedSignalPlacement: 1000},
		},
		{
			// A hand-edited row must not be able to buy an unbounded score.
			name: "placement weight above the maximum is clamped",
			signals: FeedSignals{
				PlacementWeight: 10000,
				CreatedAt:       rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:          rankNow.Add(30 * 24 * time.Hour),
			},
			want:       1000,
			wantReason: map[FeedSignalCode]int{FeedSignalPlacement: 1000},
		},
		{
			name: "negative placement weight is clamped to zero",
			signals: FeedSignals{
				PlacementWeight: -50,
				CreatedAt:       rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:          rankNow.Add(30 * 24 * time.Hour),
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalPlacement: 0},
		},
		{
			name: "supplied in the last 24h is the top freshness bucket",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-2 * time.Hour),
				EndsAt:    rankNow.Add(30 * 24 * time.Hour),
			},
			want:       300,
			wantReason: map[FeedSignalCode]int{FeedSignalFreshness: 300},
		},
		{
			// Buckets are half-open: exactly 24h old has already left the first
			// bucket. Pinned so a refactor cannot silently widen it.
			name: "exactly 24h old falls into the 3-day bucket",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-24 * time.Hour),
				EndsAt:    rankNow.Add(30 * 24 * time.Hour),
			},
			want:       200,
			wantReason: map[FeedSignalCode]int{FeedSignalFreshness: 200},
		},
		{
			name: "a week-old item is no longer fresh",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-7 * 24 * time.Hour),
				EndsAt:    rankNow.Add(30 * 24 * time.Hour),
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalFreshness: 0},
		},
		{
			// Clock skew on an imported row must not score negatively.
			name: "a future created_at is treated as just supplied",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(3 * time.Hour),
				EndsAt:    rankNow.Add(30 * 24 * time.Hour),
			},
			want:       300,
			wantReason: map[FeedSignalCode]int{FeedSignalFreshness: 300},
		},
		{
			name: "ending within 24h earns the urgency bonus",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:    rankNow.Add(5 * time.Hour),
			},
			want:       250,
			wantReason: map[FeedSignalCode]int{FeedSignalEndingSoon: 250},
		},
		{
			name: "ending within 3 days earns the smaller urgency bonus",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:    rankNow.Add(40 * time.Hour),
			},
			want:       120,
			wantReason: map[FeedSignalCode]int{FeedSignalEndingSoon: 120},
		},
		{
			// Should never reach the ranking (the read model filters it out),
			// but if it does, urgency is not a reason to promote it.
			name: "an already-ended item earns no urgency bonus",
			signals: FeedSignals{
				CreatedAt: rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:    rankNow.Add(-time.Minute),
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalEndingSoon: 0},
		},
		{
			name: "a great rating below the credibility floor scores nothing",
			signals: FeedSignals{
				CreatedAt:   rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:      rankNow.Add(30 * 24 * time.Hour),
				Rating:      5,
				ReviewCount: 4,
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalVenueRating: 0},
		},
		{
			name: "a 4.5 rating over enough reviews scores 150",
			signals: FeedSignals{
				CreatedAt:   rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:      rankNow.Add(30 * 24 * time.Hour),
				Rating:      4.5,
				ReviewCount: 12,
			},
			want:       150,
			wantReason: map[FeedSignalCode]int{FeedSignalVenueRating: 150},
		},
		{
			name: "a 5.0 rating is capped at the rating maximum",
			signals: FeedSignals{
				CreatedAt:   rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:      rankNow.Add(30 * 24 * time.Hour),
				Rating:      5,
				ReviewCount: 40,
			},
			want:       200,
			wantReason: map[FeedSignalCode]int{FeedSignalVenueRating: 200},
		},
		{
			// A weak venue is demoted by not being boosted, never punished.
			name: "a below-baseline rating never goes negative",
			signals: FeedSignals{
				CreatedAt:   rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:      rankNow.Add(30 * 24 * time.Hour),
				Rating:      1.5,
				ReviewCount: 40,
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalVenueRating: 0},
		},
		{
			// Everything at once: the sum is the point of an additive score.
			name: "signals add up",
			signals: FeedSignals{
				PlacementWeight: 30,
				CreatedAt:       rankNow.Add(-time.Hour),
				EndsAt:          rankNow.Add(2 * time.Hour),
				Rating:          4.5,
				ReviewCount:     20,
			},
			want: 300 + 300 + 250 + 150,
		},
		{
			// ScoreFeedItem's own four signals, all maxed at once, still fall
			// short of a full paid placement (1000): 300+250+200 = 750. The
			// balance that CAN outweigh a maxed placement (freshness+ending+
			// rating+taste, §5.3) is ScoreFeedCard's property, not this pure
			// four-signal function's — see TestScoreFeedCard_TasteCanOutweighAMaxedPlacement.
			name: "every ScoreFeedItem signal maxed still falls short of a full placement",
			signals: FeedSignals{
				CreatedAt:   rankNow.Add(-time.Hour),
				EndsAt:      rankNow.Add(2 * time.Hour),
				Rating:      5,
				ReviewCount: 50,
			},
			want: 750,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScoreFeedItem(tt.signals, rankNow)
			if got.Total != tt.want {
				t.Fatalf("total = %d, want %d (breakdown %+v)", got.Total, tt.want, got.Reasons)
			}
			// The breakdown must always explain the whole total, in a fixed
			// order — that is the contract the API exposes.
			wantOrder := []FeedSignalCode{
				FeedSignalPlacement, FeedSignalFreshness, FeedSignalEndingSoon,
				FeedSignalVenueRating,
			}
			if len(got.Reasons) != len(wantOrder) {
				t.Fatalf("breakdown must report every signal, got %d reasons", len(got.Reasons))
			}
			sum := 0
			for i, r := range got.Reasons {
				if r.Code != wantOrder[i] {
					t.Fatalf("reason %d = %s, want %s", i, r.Code, wantOrder[i])
				}
				if r.Detail == "" {
					t.Fatalf("reason %s carries no human-readable detail", r.Code)
				}
				sum += r.Points
			}
			if sum != got.Total {
				t.Fatalf("reasons sum to %d but total is %d", sum, got.Total)
			}
			for code, want := range tt.wantReason {
				found := false
				for _, r := range got.Reasons {
					if r.Code == code {
						found = true
						if r.Points != want {
							t.Fatalf("signal %s = %d points, want %d", code, r.Points, want)
						}
					}
				}
				if !found {
					t.Fatalf("signal %s missing from the breakdown", code)
				}
			}
		})
	}
}

// feedItemFixture builds a candidate with everything neutral, so a test can set
// exactly the one field it is about.
func feedItemFixture(kind FeedItemKind, id uuid.UUID) FeedItem {
	return FeedItem{
		Kind:      kind,
		ID:        id,
		CreatedAt: rankNow.Add(-30 * 24 * time.Hour),
		EndsAt:    rankNow.Add(30 * 24 * time.Hour),
	}
}

// tasteVenueFixture is a venue-bound card carrying its own taste signals, so a
// ScoreFeedCard case can set exactly the axis it is about.
func tasteVenueFixture(cuisines []string, price PriceCategory, features []string) FeedItem {
	it := feedItemFixture(FeedItemPromo, uuid.New())
	rid := uuid.New()
	it.RestaurantID = &rid
	it.RestaurantCuisineCodes = cuisines
	it.RestaurantPriceCategory = price
	it.RestaurantFeatureCodes = features
	return it
}

// TestScoreFeedCard_TasteBlockReplacesDeadCuisineMatch is criterion 15: the
// taste block's codes and points come from domain.ScoreTasteMatch fed by a
// domain.TasteProfile, not from FeedItem.RestaurantID/user_cuisine_preferences
// (which no longer exist on FeedItem at all — see feed.go).
func TestScoreFeedCard_TasteBlockReplacesDeadCuisineMatch(t *testing.T) {
	it := tasteVenueFixture([]string{"italian"}, PriceMid, nil)
	profile := TasteProfile{CuisineCodes: []string{"italian"}, Budget: ptrPrice(PriceMid)}

	got := ScoreFeedCard(it, profile, rankNow)

	want := map[FeedSignalCode]int{
		FeedSignalCode(TasteSignalCuisineMatch):  tasteCuisineMatchPoints,
		FeedSignalCode(TasteSignalBudgetMatch):   tasteBudgetSameTierPoints,
		FeedSignalCode(TasteSignalDietMatch):     0,
		FeedSignalCode(TasteSignalBookedSimilar): 0,
	}
	seen := map[FeedSignalCode]bool{}
	counts := map[FeedSignalCode]int{}
	for _, r := range got.Reasons {
		seen[r.Code] = true
		counts[r.Code]++
		if wantPts, ok := want[r.Code]; ok && r.Points != wantPts {
			t.Fatalf("signal %s = %d points, want %d", r.Code, r.Points, wantPts)
		}
	}
	for code := range want {
		if !seen[code] {
			t.Fatalf("taste signal %s missing from the feed card's breakdown", code)
		}
	}
	// editorial_pick/popular are /restaurants/picks-only ScoreTasteMatch
	// signals and must never appear on a feed card at all.
	for _, forbidden := range []TasteSignalCode{TasteSignalEditorialPick, TasteSignalPopular} {
		if seen[FeedSignalCode(forbidden)] {
			t.Fatalf("ScoreFeedCard must never report %s (a /restaurants/picks-only signal)", forbidden)
		}
	}
	// venue_rating IS a legitimate base ScoreFeedItem signal (FeedSignalVenueRating
	// shares the exact string "venue_rating" with TasteSignalVenueRating) — it
	// must appear EXACTLY ONCE, never twice (that would be the double-count
	// feedTasteReasonCodes exists to prevent).
	if counts[FeedSignalCode(TasteSignalVenueRating)] != 1 {
		t.Fatalf("venue_rating must appear exactly once (from ScoreFeedItem, never duplicated by the taste block), got %d", counts[FeedSignalCode(TasteSignalVenueRating)])
	}
	wantTotal := tasteCuisineMatchPoints + tasteBudgetSameTierPoints
	if got.Total != wantTotal {
		t.Fatalf("total = %d, want %d (breakdown %+v)", got.Total, wantTotal, got.Reasons)
	}
}

// TestScoreFeedCard_TasteCanOutweighAMaxedPlacement is the balance
// TestScoreFeedItem's old cuisine-inclusive case used to prove: money buys
// reach, never immunity from a card that is ALSO relevant to this guest.
func TestScoreFeedCard_TasteCanOutweighAMaxedPlacement(t *testing.T) {
	maxedPlacement := feedItemFixture(FeedItemPromo, uuid.New())
	maxedPlacement.Placement.PlacementWeight = 100 // 1000 points

	relevant := tasteVenueFixture([]string{"italian"}, PriceMid, nil)
	relevant.CreatedAt = rankNow.Add(-time.Hour)                                           // freshness 300
	relevant.EndsAt = rankNow.Add(2 * time.Hour)                                           // ending soon 250
	profile := TasteProfile{CuisineCodes: []string{"italian"}, Budget: ptrPrice(PriceMid)} // 400 + 200

	maxedScore := ScoreFeedCard(maxedPlacement, TasteProfile{}, rankNow)
	relevantScore := ScoreFeedCard(relevant, profile, rankNow)

	if maxedScore.Total != 1000 {
		t.Fatalf("maxed placement = %d, want 1000", maxedScore.Total)
	}
	wantRelevant := 300 + 250 + 400 + 200
	if relevantScore.Total != wantRelevant {
		t.Fatalf("relevant card = %d, want %d (breakdown %+v)", relevantScore.Total, wantRelevant, relevantScore.Reasons)
	}
	if relevantScore.Total <= maxedScore.Total {
		t.Fatalf("a fresh, urgent, personally relevant card (%d) must be able to outrank a maxed placement (%d)",
			relevantScore.Total, maxedScore.Total)
	}
}

// TestScoreFeedCard_PlatformItemScoresZeroTasteWithItsOwnDetail is criterion
// 16: a card with no venue (RestaurantID nil) never disappears from the feed
// and scores 0 on every taste signal with the "platform item" detail, not
// ScoreTasteMatch's own per-signal wording (which assumes a venue exists).
func TestScoreFeedCard_PlatformItemScoresZeroTasteWithItsOwnDetail(t *testing.T) {
	it := feedItemFixture(FeedItemPromo, uuid.New())
	it.RestaurantID = nil // platform item, migration 0085

	profile := TasteProfile{CuisineCodes: []string{"italian"}, Budget: ptrPrice(PriceMid),
		Diets: []string{FoodieDietHalal}, BookedRestaurantIDs: []uuid.UUID{uuid.New()}}

	got := ScoreFeedCard(it, profile, rankNow)

	tasteCodes := map[TasteSignalCode]bool{
		TasteSignalCuisineMatch: true, TasteSignalCuisineMatchImplicit: true,
		TasteSignalDietMatch: true, TasteSignalBudgetMatch: true, TasteSignalBookedSimilar: true,
	}
	found := 0
	for _, r := range got.Reasons {
		if !tasteCodes[TasteSignalCode(r.Code)] {
			continue
		}
		found++
		if r.Points != 0 {
			t.Fatalf("platform item must score 0 on %s, got %d", r.Code, r.Points)
		}
		if r.Detail != feedPlatformTasteDetail {
			t.Fatalf("platform item's %s detail = %q, want %q", r.Code, r.Detail, feedPlatformTasteDetail)
		}
	}
	if found == 0 {
		t.Fatal("a platform card must still report the taste signals, all at 0 — not omit them")
	}
	// The card is never dropped: it still contributes its own organic score.
	if got.Total < 0 {
		t.Fatalf("a platform card's total must never go negative, got %d", got.Total)
	}
}

// ptrPrice is the fixture helper for domain.TasteProfile.Budget.
func ptrPrice(p PriceCategory) *PriceCategory { return &p }

func TestRankFeedItems_OrdersByScoreThenDeadline(t *testing.T) {
	paid := feedItemFixture(FeedItemPromo, uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	paid.Placement.PlacementWeight = 50

	urgent := feedItemFixture(FeedItemPromo, uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	urgent.EndsAt = rankNow.Add(3 * time.Hour)

	dull := feedItemFixture(FeedItemEvent, uuid.MustParse("33333333-3333-3333-3333-333333333333"))

	got := RankFeedItems([]FeedItem{dull, urgent, paid}, TasteProfile{}, rankNow)
	want := []uuid.UUID{paid.ID, urgent.ID, dull.ID}
	for i, id := range want {
		if got[i].Item.ID != id {
			t.Fatalf("position %d = %s, want %s (scores %d/%d/%d)",
				i, got[i].Item.ID, id, got[0].Score.Total, got[1].Score.Total, got[2].Score.Total)
		}
	}
}

func TestRankFeedItems_TieBreakIsStableAcrossInputOrder(t *testing.T) {
	// Three cards that score IDENTICALLY and end at the same instant: only the
	// kind/id tie-break can separate them. Shuffling the input must not move a
	// single card — this is the "two calls never return a different order"
	// guarantee that pagination depends on.
	a := feedItemFixture(FeedItemPromo, uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001"))
	b := feedItemFixture(FeedItemPromo, uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002"))
	e := feedItemFixture(FeedItemEvent, uuid.MustParse("cccccccc-0000-0000-0000-000000000003"))

	permutations := [][]FeedItem{
		{a, b, e},
		{e, b, a},
		{b, e, a},
		{b, a, e},
		{e, a, b},
		{a, e, b},
	}
	// "event" sorts before "promo", then ids ascend.
	want := []uuid.UUID{e.ID, a.ID, b.ID}

	for i, perm := range permutations {
		got := RankFeedItems(perm, TasteProfile{}, rankNow)
		for pos, id := range want {
			if got[pos].Item.ID != id {
				t.Fatalf("permutation %d: position %d = %s, want %s", i, pos, got[pos].Item.ID, id)
			}
		}
	}
}

func TestRankFeedItems_EmptyAndSingle(t *testing.T) {
	if got := RankFeedItems(nil, TasteProfile{}, rankNow); len(got) != 0 {
		t.Fatalf("ranking nothing must yield nothing, got %d", len(got))
	}
	one := feedItemFixture(FeedItemPromo, uuid.New())
	if got := RankFeedItems([]FeedItem{one}, TasteProfile{}, rankNow); len(got) != 1 || got[0].Item.ID != one.ID {
		t.Fatalf("ranking one item must yield that item, got %+v", got)
	}
}

// TestRankFeedItems_NoMoreThanTwoConsecutiveCardsOfOneRestaurant is criterion
// 17: three same-restaurant cards outscoring everything else must not run
// three (or more) in a row on the ranked order — the pass runs over the WHOLE
// order, before any paging.
func TestRankFeedItems_NoMoreThanTwoConsecutiveCardsOfOneRestaurant(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()

	// Four cards of the SAME restaurant, all scoring higher (paid placement)
	// than one card of a DIFFERENT restaurant with no placement at all — so
	// the priority order alone would run all four of rid's cards first.
	mk := func(r uuid.UUID, weight int) FeedItem {
		it := feedItemFixture(FeedItemPromo, uuid.New())
		it.RestaurantID = &r
		it.Placement.PlacementWeight = weight
		return it
	}
	a := mk(rid, 90)
	b := mk(rid, 80)
	c := mk(rid, 70)
	d := mk(rid, 60)
	e := mk(other, 0)

	got := RankFeedItems([]FeedItem{a, b, c, d, e}, TasteProfile{}, rankNow)
	if len(got) != 5 {
		t.Fatalf("expected 5 ranked cards, got %d", len(got))
	}
	run := 0
	var lastID uuid.UUID
	var haveLast bool
	for _, r := range got {
		rest := r.Item.RestaurantID
		if rest != nil && haveLast && *rest == lastID {
			run++
			if run >= 2 {
				t.Fatalf("three or more consecutive cards of restaurant %s: %+v", *rest, got)
			}
		} else {
			run = 0
		}
		if rest != nil {
			lastID, haveLast = *rest, true
		} else {
			haveLast = false
		}
	}
	// e (the other restaurant) must have been pulled forward to break the run.
	if got[0].Item.RestaurantID == nil || *got[0].Item.RestaurantID != rid {
		t.Fatalf("the priority order must still lead with %s's highest card, got %+v", rid, got[0].Item)
	}
	foundOther := false
	for i := 0; i < 3; i++ {
		if got[i].Item.RestaurantID != nil && *got[i].Item.RestaurantID == other {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("the other restaurant's card must be pulled forward within the first 3 positions, got %+v", got)
	}
}

// TestRankFeedItems_PlatformCardsAreNeverGroupedWithEachOther guards the
// feedDiversityKey choice: two UNRELATED platform cards (both RestaurantID
// nil) must not be treated as "the same venue" and reshuffled away from each
// other, the way two real cards of the same restaurant would be.
func TestRankFeedItems_PlatformCardsAreNeverGroupedWithEachOther(t *testing.T) {
	mk := func(weight int) FeedItem {
		it := feedItemFixture(FeedItemPromo, uuid.New())
		it.Placement.PlacementWeight = weight
		return it
	}
	a, b, c := mk(90), mk(80), mk(70)

	got := RankFeedItems([]FeedItem{a, b, c}, TasteProfile{}, rankNow)
	want := []uuid.UUID{a.ID, b.ID, c.ID}
	for i, id := range want {
		if got[i].Item.ID != id {
			t.Fatalf("platform cards must keep their priority order untouched by the venue-diversity pass, got %+v", got)
		}
	}
}

// eligibleFixture is a card that passes every gate, so a case can break
// exactly one condition and assert that one condition alone hides it.
func eligibleFixture() FeedItem {
	it := feedItemFixture(FeedItemPromo, uuid.New())
	// A VENUE-BOUND card: the venue flags below only mean anything when there
	// is a venue (since migration 0085 an item may have none), so the fixture
	// has to say which of the two shapes it is.
	rid := uuid.New()
	it.RestaurantID = &rid
	almaty := CityAlmaty
	it.City = &almaty
	it.VenueIsActive = true
	it.ItemStatus = string(PromoPublished)
	it.Placement.Status = FeedApproved
	it.StartsAt = rankNow.Add(-time.Hour)
	return it
}

func TestFeedEligible(t *testing.T) {
	tests := []struct {
		name  string
		mutFn func(*FeedItem)
		want  bool
	}{
		{name: "published, approved, in window, active venue", mutFn: func(*FeedItem) {}, want: true},
		{name: "unapproved item never appears", mutFn: func(i *FeedItem) { i.Placement.Status = FeedNotSubmitted }},
		{name: "an item still awaiting review never appears", mutFn: func(i *FeedItem) { i.Placement.Status = FeedPendingReview }},
		{name: "a rejected item never appears", mutFn: func(i *FeedItem) { i.Placement.Status = FeedRejected }},
		{name: "an approved but unpublished draft never appears", mutFn: func(i *FeedItem) { i.ItemStatus = string(PromoDraft) }},
		{name: "an approved item the venue hid never appears", mutFn: func(i *FeedItem) { i.ItemStatus = string(PromoHidden) }},
		{name: "an expired item never appears", mutFn: func(i *FeedItem) { i.EndsAt = rankNow.Add(-time.Minute) }},
		{name: "an item ending exactly now never appears", mutFn: func(i *FeedItem) { i.EndsAt = rankNow }},
		{name: "a promo whose window has not opened never appears", mutFn: func(i *FeedItem) { i.StartsAt = rankNow.Add(time.Hour) }},
		{name: "another city's item never appears", mutFn: func(i *FeedItem) { astana := CityAstana; i.City = &astana }},
		{name: "a deactivated venue takes its content with it", mutFn: func(i *FeedItem) { i.VenueIsActive = false }},
		{name: "a venue hidden from home is hidden from the feed", mutFn: func(i *FeedItem) { i.VenueHiddenFromHome = true }},
		{
			// The one asymmetry with promos, pinned so it is not "fixed" away.
			name: "an upcoming event IS promoted before it starts",
			mutFn: func(i *FeedItem) {
				i.Kind = FeedItemEvent
				i.StartsAt = rankNow.Add(48 * time.Hour)
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := eligibleFixture()
			tt.mutFn(&item)
			if got := FeedEligible(item, CityAlmaty, rankNow); got != tt.want {
				t.Fatalf("eligible = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFeedLifecycleOf(t *testing.T) {
	approvedPromo := func(status string, startsAt time.Time) FeedItem {
		it := feedItemFixture(FeedItemPromo, uuid.New())
		it.ItemStatus = status
		it.StartsAt = startsAt
		it.Placement.Status = FeedApproved
		return it
	}

	tests := []struct {
		name string
		item func() FeedItem
		want FeedLifecycle
	}{
		{
			name: "never submitted",
			item: func() FeedItem { return feedItemFixture(FeedItemPromo, uuid.New()) },
			want: FeedLifecycleNotSubmitted,
		},
		{
			name: "waiting for the platform",
			item: func() FeedItem {
				it := feedItemFixture(FeedItemPromo, uuid.New())
				it.Placement.Status = FeedPendingReview
				return it
			},
			want: FeedLifecycleSubmitted,
		},
		{
			name: "rejected by the platform",
			item: func() FeedItem {
				it := feedItemFixture(FeedItemPromo, uuid.New())
				it.Placement.Status = FeedRejected
				return it
			},
			want: FeedLifecycleRejected,
		},
		{
			name: "approved but the venue has not published it",
			item: func() FeedItem { return approvedPromo("draft", rankNow.Add(-time.Hour)) },
			want: FeedLifecycleApproved,
		},
		{
			name: "approved but the venue hid it again",
			item: func() FeedItem { return approvedPromo("hidden", rankNow.Add(-time.Hour)) },
			want: FeedLifecycleApproved,
		},
		{
			name: "approved promo whose window has not opened yet",
			item: func() FeedItem { return approvedPromo("published", rankNow.Add(time.Hour)) },
			want: FeedLifecycleApproved,
		},
		{
			name: "approved published promo inside its window is live",
			item: func() FeedItem { return approvedPromo("published", rankNow.Add(-time.Hour)) },
			want: FeedLifecycleLive,
		},
		{
			// An event is promoted BEFORE it starts — that is the whole point of
			// announcing it — so a future start does not hold it back.
			name: "approved published upcoming event is live",
			item: func() FeedItem {
				it := feedItemFixture(FeedItemEvent, uuid.New())
				it.ItemStatus = "published"
				it.StartsAt = rankNow.Add(48 * time.Hour)
				it.Placement.Status = FeedApproved
				return it
			},
			want: FeedLifecycleLive,
		},
		{
			// Expiry beats every moderation state.
			name: "an ended item pending review is over, not waiting",
			item: func() FeedItem {
				it := feedItemFixture(FeedItemPromo, uuid.New())
				it.EndsAt = rankNow.Add(-time.Minute)
				it.Placement.Status = FeedPendingReview
				return it
			},
			want: FeedLifecycleExpired,
		},
		{
			name: "an item ending exactly now is already expired",
			item: func() FeedItem {
				it := approvedPromo("published", rankNow.Add(-time.Hour))
				it.EndsAt = rankNow
				return it
			},
			want: FeedLifecycleExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FeedLifecycleOf(tt.item(), rankNow); got != tt.want {
				t.Fatalf("lifecycle = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFeedStatusAfterContentEdit(t *testing.T) {
	tests := []struct {
		cur  FeedStatus
		want FeedStatus
	}{
		// A decision was made about specific words: editing them invalidates it.
		{FeedApproved, FeedPendingReview},
		{FeedRejected, FeedPendingReview},
		// Nothing was decided yet — editing changes nothing.
		{FeedPendingReview, FeedPendingReview},
		{FeedNotSubmitted, FeedNotSubmitted},
	}
	for _, tt := range tests {
		t.Run(string(tt.cur), func(t *testing.T) {
			if got := FeedStatusAfterContentEdit(tt.cur); got != tt.want {
				t.Fatalf("after edit %s = %s, want %s", tt.cur, got, tt.want)
			}
		})
	}
}

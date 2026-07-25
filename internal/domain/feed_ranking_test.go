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
				FeedSignalPlacement:    0,
				FeedSignalFreshness:    0,
				FeedSignalEndingSoon:   0,
				FeedSignalVenueRating:  0,
				FeedSignalCuisineMatch: 0,
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
			name: "a cuisine match is the strongest organic signal",
			signals: FeedSignals{
				CreatedAt:                rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:                   rankNow.Add(30 * 24 * time.Hour),
				HasCuisinePreferences:    true,
				MatchesCuisinePreference: true,
			},
			want:       400,
			wantReason: map[FeedSignalCode]int{FeedSignalCuisineMatch: 400},
		},
		{
			name: "a guest with preferences gets nothing for a non-matching item",
			signals: FeedSignals{
				CreatedAt:             rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:                rankNow.Add(30 * 24 * time.Hour),
				HasCuisinePreferences: true,
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalCuisineMatch: 0},
		},
		{
			// The "preferences absent" path: an anonymous guest, or one who
			// never picked a cuisine. Every item scores 0 here, so the rest of
			// the signals keep their relative order untouched.
			name: "no preferences at all is neutral, not a mismatch",
			signals: FeedSignals{
				CreatedAt:                rankNow.Add(-30 * 24 * time.Hour),
				EndsAt:                   rankNow.Add(30 * 24 * time.Hour),
				MatchesCuisinePreference: true, // stale flag without preferences
			},
			want:       0,
			wantReason: map[FeedSignalCode]int{FeedSignalCuisineMatch: 0},
		},
		{
			// Everything at once: the sum is the point of an additive score.
			name: "signals add up",
			signals: FeedSignals{
				PlacementWeight:          30,
				CreatedAt:                rankNow.Add(-time.Hour),
				EndsAt:                   rankNow.Add(2 * time.Hour),
				Rating:                   4.5,
				ReviewCount:              20,
				HasCuisinePreferences:    true,
				MatchesCuisinePreference: true,
			},
			want: 300 + 300 + 250 + 150 + 400,
		},
		{
			// The intended balance: a maxed-out paid placement loses to a card
			// that is fresh, urgent, well-rated AND personally relevant. Money
			// buys reach, not immunity.
			name: "a maxed paid placement does not outrank every organic signal at once",
			signals: FeedSignals{
				CreatedAt:                rankNow.Add(-time.Hour),
				EndsAt:                   rankNow.Add(2 * time.Hour),
				Rating:                   5,
				ReviewCount:              50,
				HasCuisinePreferences:    true,
				MatchesCuisinePreference: true,
			},
			want: 1150,
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
				FeedSignalVenueRating, FeedSignalCuisineMatch,
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

func TestRankFeedItems_OrdersByScoreThenDeadline(t *testing.T) {
	paid := feedItemFixture(FeedItemPromo, uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	paid.Placement.PlacementWeight = 50

	urgent := feedItemFixture(FeedItemPromo, uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	urgent.EndsAt = rankNow.Add(3 * time.Hour)

	dull := feedItemFixture(FeedItemEvent, uuid.MustParse("33333333-3333-3333-3333-333333333333"))

	got := RankFeedItems([]FeedItem{dull, urgent, paid}, rankNow)
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
		got := RankFeedItems(perm, rankNow)
		for pos, id := range want {
			if got[pos].Item.ID != id {
				t.Fatalf("permutation %d: position %d = %s, want %s", i, pos, got[pos].Item.ID, id)
			}
		}
	}
}

func TestRankFeedItems_EmptyAndSingle(t *testing.T) {
	if got := RankFeedItems(nil, rankNow); len(got) != 0 {
		t.Fatalf("ranking nothing must yield nothing, got %d", len(got))
	}
	one := feedItemFixture(FeedItemPromo, uuid.New())
	if got := RankFeedItems([]FeedItem{one}, rankNow); len(got) != 1 || got[0].Item.ID != one.ID {
		t.Fatalf("ranking one item must yield that item, got %+v", got)
	}
}

// eligibleFixture is a card that passes every gate, so a case can break
// exactly one condition and assert that one condition alone hides it.
func eligibleFixture() FeedItem {
	it := feedItemFixture(FeedItemPromo, uuid.New())
	it.City = CityAlmaty
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
		{name: "another city's item never appears", mutFn: func(i *FeedItem) { i.City = CityAstana }},
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

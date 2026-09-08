package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// platformFixture is a main-screen card the PLATFORM supplied: no venue, no
// city of its own. Everything else is the same as any other eligible card.
func platformFixture() FeedItem {
	it := feedItemFixture(FeedItemPromo, uuid.New())
	it.RestaurantID = nil
	it.City = nil
	it.ItemStatus = string(PromoPublished)
	it.Placement.Status = FeedApproved
	it.StartsAt = rankNow.Add(-time.Hour)
	return it
}

// A platform card with no city belongs on EVERY city's main screen. Without the
// nil-city branch it belongs on none, because the pre-0085 rule compared a
// value against a value and an absent city equalled nothing.
func TestFeedEligible_PlatformCardRunsInEveryCity(t *testing.T) {
	it := platformFixture()
	for _, city := range []City{CityAlmaty, CityAstana} {
		if !FeedEligible(it, city, rankNow) {
			t.Fatalf("a platform card must be eligible in %s", city)
		}
	}
}

// A platform card pinned to one city runs there and nowhere else — the same
// rule a venue-bound card follows, just sourced from the item itself.
func TestFeedEligible_PlatformCardHonoursItsOwnCity(t *testing.T) {
	it := platformFixture()
	almaty := CityAlmaty
	it.City = &almaty

	if !FeedEligible(it, CityAlmaty, rankNow) {
		t.Fatal("a platform card pinned to Almaty must appear in Almaty")
	}
	if FeedEligible(it, CityAstana, rankNow) {
		t.Fatal("a platform card pinned to Almaty must NOT appear in Astana")
	}
}

// The venue flags are meaningless without a venue and must not be able to hide
// the platform's own card. This is the case that would silently delete the
// whole feature if the eligibility rule kept reading them unconditionally: the
// read model has no venue row to take `is_active` from.
func TestFeedEligible_PlatformCardIgnoresVenueFlags(t *testing.T) {
	it := platformFixture()
	it.VenueIsActive = false
	it.VenueHiddenFromHome = true

	if !FeedEligible(it, CityAlmaty, rankNow) {
		t.Fatal("a card with no venue cannot be hidden by a venue's flags")
	}
}

// ...while a venue-bound card is affected by them exactly as before. Stated
// here next to the case above so the two cannot drift apart.
func TestFeedEligible_VenueBoundCardStillObeysVenueFlags(t *testing.T) {
	it := eligibleFixture()
	if !FeedEligible(it, CityAlmaty, rankNow) {
		t.Fatal("the fixture must be eligible to begin with")
	}
	deactivated := it
	deactivated.VenueIsActive = false
	if FeedEligible(deactivated, CityAlmaty, rankNow) {
		t.Fatal("a deactivated venue must still take its content with it")
	}
	hidden := it
	hidden.VenueHiddenFromHome = true
	if FeedEligible(hidden, CityAlmaty, rankNow) {
		t.Fatal("a venue hidden from home must still be hidden from the feed")
	}
}

// Ranking a card with no venue must not panic and must simply score a neutral
// rating: the venue-rating signal has no venue to read, and "unrated" is
// already a modelled case (below the credibility floor).
func TestScoreFeedItem_PlatformCardScoresNeutralVenueRating(t *testing.T) {
	it := platformFixture()
	score := ScoreFeedItem(FeedSignalsOf(it), rankNow)
	for _, r := range score.Reasons {
		if r.Code == FeedSignalVenueRating && r.Points != 0 {
			t.Fatalf("a card with no venue must score 0 for venue rating, got %d", r.Points)
		}
	}
	if len(RankFeedItems([]FeedItem{it}, rankNow)) != 1 {
		t.Fatal("ranking a platform card must yield that card")
	}
}

// The demotion rule reads owner, not status: venue content loses its approval
// when it is edited, platform content does not (there is no second party to
// re-review it). Both halves stated here because the SQL is written against
// this function.
func TestFeedDemotableAfterContentEdit(t *testing.T) {
	rid := uuid.New()
	if !FeedDemotableAfterContentEdit(&rid) {
		t.Fatal("a venue's item must still be demoted when its content changes")
	}
	if FeedDemotableAfterContentEdit(nil) {
		t.Fatal("platform content must not be demoted: its editor is its reviewer")
	}
}

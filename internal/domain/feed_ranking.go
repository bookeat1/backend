package domain

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

// FeedSignalCode names one ranking signal. The codes are part of the API: the
// feed returns the breakdown that produced each card's position, so a venue
// asking "why am I third?" gets an answer, and so a bad ordering can be
// debugged from a response body instead of from the database.
type FeedSignalCode string

const (
	// FeedSignalPlacement is the paid placement the platform sold and set.
	FeedSignalPlacement FeedSignalCode = "paid_placement"
	// FeedSignalFreshness rewards recently supplied content — the rail must
	// feel alive, and a venue that keeps posting should see it pay off.
	FeedSignalFreshness FeedSignalCode = "freshness"
	// FeedSignalEndingSoon rewards urgency: an offer expiring tonight is worth
	// more to a guest deciding where to eat tonight than one running all month.
	FeedSignalEndingSoon FeedSignalCode = "ending_soon"
	// FeedSignalVenueRating rewards venues guests actually rate well, so paid
	// placement cannot be the only way up.
	FeedSignalVenueRating FeedSignalCode = "venue_rating"
)

// Scoring constants. They are plain integers on ONE scale (points) rather than
// normalized weights: an explainable score is one a human can add up in their
// head. The relative sizes encode the product intent —
//   - a full paid placement (1000) can outrank any single organic signal but
//     NOT the sum of them (300+250+200 = 750, plus the taste block below), so
//     money buys reach and never buys immunity from being beaten by genuinely
//     relevant content;
//   - the guest's own taste (ScoreTasteMatch, up to 1000 via ScoreFeedCard) is
//     the strongest organic signal, because relevance is what makes the rail
//     worth opening — see ScoreFeedCard, not this function, for that block.
const (
	// feedPlacementPointsPerWeight maps the 0..100 weight onto 0..1000 points.
	feedPlacementPointsPerWeight = 10

	feedFreshWithinDayPoints    = 300
	feedFreshWithin3DaysPoints  = 200
	feedFreshWithinWeekPoints   = 100
	feedEndingWithinDayPoints   = 250
	feedEndingWithin3DaysPoints = 120

	// feedRatingMinReviews is the credibility floor: one 5★ review from the
	// owner's cousin must not outrank a venue with fifty 4.6★ reviews, so a
	// venue below the floor scores a neutral 0 rather than a guessed value.
	feedRatingMinReviews = 5
	// feedRatingBaseline is the "average" rating that earns nothing; only the
	// part above it scores, and a below-average venue is never pushed NEGATIVE
	// (the feed demotes it by not boosting it, it does not punish it).
	feedRatingBaseline      = 3.0
	feedRatingPointsPerStar = 100
	feedRatingMaxPoints     = 200
)

// Freshness / urgency thresholds, named so the buckets read as product rules.
const (
	feedOneDay   = 24 * time.Hour
	feedThreeDay = 72 * time.Hour
	feedOneWeek  = 7 * 24 * time.Hour
)

// FeedScoreReason is one signal's contribution: the code, the points it added,
// and a short human-readable detail. Points may be 0 — every signal is always
// reported, because "this contributed nothing" is itself the explanation a
// venue needs.
type FeedScoreReason struct {
	Code   FeedSignalCode
	Points int
	Detail string
}

// FeedScore is a card's total plus the full breakdown that produced it. Reasons
// are always emitted in the same fixed order (placement, freshness, ending
// soon, rating — ScoreFeedCard appends the taste block after these four in
// ScoreTasteMatch's own order), so two identical inputs render identically.
type FeedScore struct {
	Total   int
	Reasons []FeedScoreReason
}

// FeedSignals is the pure input of the ranking: only values that actually exist
// in this schema. Nothing here is a model output or a remembered aggregate —
// every field comes straight off the feed read model.
type FeedSignals struct {
	// PlacementWeight is FeedPlacement.PlacementWeight (0..MaxFeedPlacementWeight).
	PlacementWeight int
	// CreatedAt is when the venue supplied the item — the freshness anchor.
	CreatedAt time.Time
	// EndsAt is the item's deadline — the urgency anchor.
	EndsAt time.Time
	// Rating / ReviewCount is the venue's published-review aggregate.
	Rating      float64
	ReviewCount int
}

// FeedSignalsOf extracts ScoreFeedItem's OWN four signals from a feed item —
// placement, freshness, ending soon, venue rating. It deliberately does NOT
// carry the taste block any more (§5.3's dead cuisine_match signal, replaced
// by ScoreFeedCard's own domain.ScoreTasteMatch call); kept next to the scorer
// so the mapping is visible in one place rather than hidden in a usecase.
func FeedSignalsOf(item FeedItem) FeedSignals {
	return FeedSignals{
		PlacementWeight: item.Placement.PlacementWeight,
		CreatedAt:       item.CreatedAt,
		EndsAt:          item.EndsAt,
		Rating:          item.RestaurantRating,
		ReviewCount:     item.RestaurantReviewCount,
	}
}

// ScoreFeedItem computes a card's merchandising score as of now. It is
// deliberately PURE — no repository, no clock, no actor, no randomness and no
// floating-point accumulation: the total is an integer sum, so the same inputs
// always produce the same number on any machine, and the ordering below can be
// trusted to be stable.
//
// Nothing here decides ELIGIBILITY. Whether an item may appear at all
// (published, approved, inside its window, active venue) is a hard filter in
// the read model; the score only orders what already passed it. Keeping the two
// apart is what makes it impossible for a high score to smuggle an unapproved
// item onto the main screen.
func ScoreFeedItem(s FeedSignals, now time.Time) FeedScore {
	reasons := make([]FeedScoreReason, 0, 4)

	// 1. Paid placement. Clamped rather than trusted: a hand-edited row must not
	// be able to buy an unbounded score.
	weight := s.PlacementWeight
	if weight < 0 {
		weight = 0
	}
	if weight > MaxFeedPlacementWeight {
		weight = MaxFeedPlacementWeight
	}
	reasons = append(reasons, FeedScoreReason{
		Code:   FeedSignalPlacement,
		Points: weight * feedPlacementPointsPerWeight,
		Detail: fmt.Sprintf("placement weight %d/%d", weight, MaxFeedPlacementWeight),
	})

	// 2. Freshness, in buckets rather than a decay curve — a bucket is
	// explainable to a venue ("your promo is in its first day"), a curve is not.
	// A CreatedAt in the future (clock skew on an imported row) is treated as
	// "just now" instead of scoring negatively.
	age := now.Sub(s.CreatedAt)
	if age < 0 {
		age = 0
	}
	freshPoints, freshDetail := 0, "supplied more than a week ago"
	switch {
	case age < feedOneDay:
		freshPoints, freshDetail = feedFreshWithinDayPoints, "supplied within 24h"
	case age < feedThreeDay:
		freshPoints, freshDetail = feedFreshWithin3DaysPoints, "supplied within 3 days"
	case age < feedOneWeek:
		freshPoints, freshDetail = feedFreshWithinWeekPoints, "supplied within a week"
	}
	reasons = append(reasons, FeedScoreReason{Code: FeedSignalFreshness, Points: freshPoints, Detail: freshDetail})

	// 3. Ending soon. An already-expired item scores nothing here instead of a
	// huge negative-time bonus — it should never have reached the ranking at
	// all, and if it somehow did, urgency is not the reason to promote it.
	remaining := s.EndsAt.Sub(now)
	endPoints, endDetail := 0, "ends in more than 3 days"
	switch {
	case remaining <= 0:
		endDetail = "already ended"
	case remaining < feedOneDay:
		endPoints, endDetail = feedEndingWithinDayPoints, "ends within 24h"
	case remaining < feedThreeDay:
		endPoints, endDetail = feedEndingWithin3DaysPoints, "ends within 3 days"
	}
	reasons = append(reasons, FeedScoreReason{Code: FeedSignalEndingSoon, Points: endPoints, Detail: endDetail})

	// 4. Venue rating, only above the credibility floor and only for the part
	// above the baseline. Rounded to an int immediately so no float ever enters
	// the total.
	ratingPoints, ratingDetail := 0, fmt.Sprintf("fewer than %d reviews", feedRatingMinReviews)
	if s.ReviewCount >= feedRatingMinReviews {
		above := s.Rating - feedRatingBaseline
		if above < 0 {
			above = 0
		}
		ratingPoints = int(math.Round(above * feedRatingPointsPerStar))
		if ratingPoints > feedRatingMaxPoints {
			ratingPoints = feedRatingMaxPoints
		}
		ratingDetail = fmt.Sprintf("rated %.2f over %d reviews", s.Rating, s.ReviewCount)
	}
	reasons = append(reasons, FeedScoreReason{Code: FeedSignalVenueRating, Points: ratingPoints, Detail: ratingDetail})

	total := 0
	for _, r := range reasons {
		total += r.Points
	}
	return FeedScore{Total: total, Reasons: reasons}
}

// feedTasteReasonCodes are the ONLY ScoreTasteMatch signals a feed card's
// score includes — spec foodie-personalization-v1-20260916.md §5.3's last
// paragraph / §4 criterion 15: "Total = ScoreFeedItem-сигналы + ScoreTasteMatch
// (заведение карточки)". editorial_pick, venue_rating and popular are real
// ScoreTasteMatch rows too, but are deliberately EXCLUDED here:
//   - venue_rating would double-count FeedSignalVenueRating above (the same
//     review aggregate, scored twice under two different codes);
//   - editorial_pick and popular are /restaurants/picks-only concepts (a
//     city's manual pick list, is_popular) that §5.3's feed paragraph never
//     mentions — only "the taste block" (cuisine/diet/budget/booking-history)
//     replaces the dead cuisine_match signal, not the whole ScoreTasteMatch
//     formula.
//
// ScoreFeedCard passes IsPopular/EditorialPick/Rating/ReviewCount as their
// zero value into VenueTasteSignals for exactly this reason: with those left
// zero, ScoreTasteMatch's own editorial_pick/venue_rating/popular rows always
// score 0 regardless of this filter, so filtering is a belt-and-braces
// guarantee against ever reading points from a signal this feed does not use,
// not the only thing preventing double-counting.
var feedTasteReasonCodes = map[TasteSignalCode]bool{
	TasteSignalCuisineMatch:         true,
	TasteSignalCuisineMatchImplicit: true,
	TasteSignalDietMatch:            true,
	TasteSignalBudgetMatch:          true,
	TasteSignalBookedSimilar:        true,
}

// feedPlatformTasteDetail is criterion 16's detail string for a PLATFORM
// card's taste reasons: "не исчезает из ленты" with 0 on every taste signal,
// but WITHOUT the misleading per-signal ScoreTasteMatch details ("guest has no
// cuisine preferences" etc.) that assume a venue exists to be evaluated
// against.
const feedPlatformTasteDetail = "platform item"

// VenueTasteSignalsOf builds the taste-matching input for item's OWN venue —
// the venue-side half of ScoreFeedCard's ScoreTasteMatch call. Deliberately
// leaves IsPopular, EditorialPick, Rating and ReviewCount at their zero value:
// see feedTasteReasonCodes for why a feed card never scores those three
// ScoreTasteMatch signals. Meaningless (returns the zero VenueTasteSignals) on
// a PLATFORM item — callers must check item.RestaurantID first, same as every
// other venue-only field on FeedItem.
func VenueTasteSignalsOf(item FeedItem) VenueTasteSignals {
	if item.RestaurantID == nil {
		return VenueTasteSignals{}
	}
	return VenueTasteSignals{
		RestaurantID:  *item.RestaurantID,
		CuisineCodes:  item.RestaurantCuisineCodes,
		PriceCategory: item.RestaurantPriceCategory,
		FeatureCodes:  item.RestaurantFeatureCodes,
	}
}

// platformTasteReasons is criterion 16's answer for a card with no venue:
// zero on every taste signal a REAL venue could have scored, each carrying
// feedPlatformTasteDetail instead of ScoreTasteMatch's own per-signal detail.
// The cuisine row's code still picks cuisine_match vs. cuisine_match_implicit
// by the SAME rule ScoreTasteMatch itself uses (does the guest have an
// explicit cuisine pick) — a client that already localizes both codes must not
// learn a THIRD meaning of either one for the platform-item case.
func platformTasteReasons(taste TasteProfile) []TasteMatchReason {
	cuisineCode := TasteSignalCuisineMatchImplicit
	if len(taste.CuisineCodes) > 0 {
		cuisineCode = TasteSignalCuisineMatch
	}
	return []TasteMatchReason{
		{Code: cuisineCode, Detail: feedPlatformTasteDetail},
		{Code: TasteSignalDietMatch, Detail: feedPlatformTasteDetail},
		{Code: TasteSignalBudgetMatch, Detail: feedPlatformTasteDetail},
		{Code: TasteSignalBookedSimilar, Detail: feedPlatformTasteDetail},
	}
}

// ScoreFeedCard is a feed card's full score: ScoreFeedItem's own four organic
// signals (placement, freshness, ending soon, venue rating) plus the taste
// block that replaces the dead cuisine_match signal — spec
// foodie-personalization-v1-20260916.md §5.3 last paragraph / §8 BE-3.
//
// taste is the guest's domain.TasteProfile (usecase/tastematch.Loader's own
// read — the SAME assembler BE-2/BE-4 use, so two surfaces scoring the same
// guest against the same venue never disagree, criterion 15). Its zero value
// (anonymous guest, or one with no foodie profile) scores 0 on every taste
// signal, which is exactly criterion 18's "today's order" requirement — no
// branch here treats "no profile" specially, ScoreTasteMatch already does.
//
// item.RestaurantID nil (a PLATFORM card) short-circuits to
// platformTasteReasons instead of calling ScoreTasteMatch with a zero-value
// venue: criterion 16 wants a "platform item" detail, not ScoreTasteMatch's
// own per-signal wording for a venue that structurally cannot exist.
func ScoreFeedCard(item FeedItem, taste TasteProfile, now time.Time) FeedScore {
	base := ScoreFeedItem(FeedSignalsOf(item), now)

	var tasteReasons []TasteMatchReason
	if item.RestaurantID == nil {
		tasteReasons = platformTasteReasons(taste)
	} else {
		_, all := ScoreTasteMatch(taste, VenueTasteSignalsOf(item))
		for _, r := range all {
			if feedTasteReasonCodes[r.Code] {
				tasteReasons = append(tasteReasons, r)
			}
		}
	}

	total := base.Total
	reasons := make([]FeedScoreReason, 0, len(base.Reasons)+len(tasteReasons))
	reasons = append(reasons, base.Reasons...)
	for _, r := range tasteReasons {
		reasons = append(reasons, FeedScoreReason{Code: FeedSignalCode(r.Code), Points: r.Points, Detail: r.Detail})
		total += r.Points
	}
	return FeedScore{Total: total, Reasons: reasons}
}

// FeedEligible reports whether item may appear on city's main screen at now.
// It is the WHOLE visibility rule in one place, and it is a hard gate: nothing
// in the ranking can override it.
//
// The Postgres read model (FeedRepository.ListCandidates) enforces exactly
// these conditions in SQL, because filtering a whole city's content in Go would
// be the N+1 this feed exists to avoid. This function is the canonical written
// statement of the rule — the SQL must agree with it, and an integration test
// against a real database is what proves it does.
func FeedEligible(item FeedItem, city City, now time.Time) bool {
	// The venue itself must be visible: a deactivated venue, or one the
	// platform pulled off the home screen, takes its content with it. A
	// PLATFORM item has no venue, so there is nothing here to hide it —
	// checking the flags anyway would mean the whole feature depends on the
	// read model remembering to project a neutral `true`.
	if item.RestaurantID != nil && (!item.VenueIsActive || item.VenueHiddenFromHome) {
		return false
	}
	// A nil city is the platform item that belongs on EVERY city's main screen
	// (see FeedItem.City). A venue-bound item always has one — its venue's —
	// so this branch cannot loosen anything that exists today.
	if item.City != nil && *item.City != city {
		return false
	}
	// The venue published it AND the platform approved it — two independent
	// yeses, both required.
	if item.ItemStatus != string(PromoPublished) {
		return false
	}
	if item.Placement.Status != FeedApproved {
		return false
	}
	// The window must still be open. A promo additionally has to have STARTED
	// (an offer that is not yet valid is not an offer); an upcoming event is
	// promoted before it starts, which is the point of announcing it.
	if !item.EndsAt.After(now) {
		return false
	}
	if item.Kind == FeedItemPromo && item.StartsAt.After(now) {
		return false
	}
	return true
}

// RankedFeedItem is a card with the score that placed it.
type RankedFeedItem struct {
	Item  FeedItem
	Score FeedScore
}

// RankFeedItems scores every candidate and returns them in the order the main
// screen shows them. Pure: it neither reads a clock nor mutates its input
// slice's order in place beyond the copy it builds.
//
// taste is the guest's domain.TasteProfile, scored into every card via
// ScoreFeedCard (§8 BE-3) — its zero value is the anonymous/no-profile case
// (criterion 18).
//
// The score order is a TOTAL one — score desc, then the soonest deadline, then
// kind, then id — so two calls over the same data can never disagree, and
// paginating by slicing this list can never show or skip a card twice. Every
// tie-break after the score is a value that is unique-per-card in the limit
// (id is), so no pair of distinct cards ever compares equal.
//
// A DIVERSITY pass then runs over the WHOLE scored order (before any paging —
// criterion 17: "перестановка ДО нарезки страниц"), via the same
// domain.DiversifyByKey BE-2 uses for its own cuisine rule: no more than two
// consecutive cards of the same restaurant. The diversity key is the
// restaurant id for a venue-bound card; a platform card (no restaurant) gets
// its OWN unique key per card (kind+id) rather than sharing one group with
// every other platform card, so two unrelated platform cards next to each
// other are never treated as "the same venue" and reshuffled.
func RankFeedItems(items []FeedItem, taste TasteProfile, now time.Time) []RankedFeedItem {
	ranked := make([]RankedFeedItem, 0, len(items))
	for _, it := range items {
		ranked = append(ranked, RankedFeedItem{Item: it, Score: ScoreFeedCard(it, taste, now)})
	}
	sort.Slice(ranked, func(i, j int) bool { return lessRankedFeedItem(ranked[i], ranked[j]) })
	return DiversifyByKey(ranked, feedDiversityKey)
}

// feedDiversityKey is RankFeedItems' own grouping key for DiversifyByKey —
// see the diversity paragraph on RankFeedItems for why a platform card gets a
// key unique to itself instead of sharing "no restaurant" with every other
// platform card.
func feedDiversityKey(r RankedFeedItem) string {
	if r.Item.RestaurantID != nil {
		return r.Item.RestaurantID.String()
	}
	return "platform:" + string(r.Item.Kind) + ":" + r.Item.ID.String()
}

// lessRankedFeedItem is the total order described on RankFeedItems.
func lessRankedFeedItem(a, b RankedFeedItem) bool {
	if a.Score.Total != b.Score.Total {
		return a.Score.Total > b.Score.Total
	}
	// Equal scores: the one that disappears sooner goes first — showing it later
	// may mean not showing it at all.
	if !a.Item.EndsAt.Equal(b.Item.EndsAt) {
		return a.Item.EndsAt.Before(b.Item.EndsAt)
	}
	// Kind before id so a promo and an event that are otherwise identical still
	// have a defined order ("event" < "promo" lexicographically).
	if a.Item.Kind != b.Item.Kind {
		return a.Item.Kind < b.Item.Kind
	}
	// The final arbiter: ids are unique within a kind, so this always decides.
	return a.Item.ID.String() < b.Item.ID.String()
}

// FeedLifecycle is the state a VENUE sees for its own item — the answer to
// "where is my submission". It is derived, never stored: deriving it means the
// venue's view can never drift out of sync with the moderation columns and the
// item's window.
type FeedLifecycle string

const (
	// FeedLifecycleNotSubmitted — the venue never asked for the main screen.
	FeedLifecycleNotSubmitted FeedLifecycle = "not_submitted"
	// FeedLifecycleSubmitted — waiting for the platform's decision.
	FeedLifecycleSubmitted FeedLifecycle = "submitted"
	// FeedLifecycleRejected — the platform said no (see RejectionReason).
	FeedLifecycleRejected FeedLifecycle = "rejected"
	// FeedLifecycleApproved — the platform said yes, but the card is not on the
	// screen yet: the item is still a draft/hidden, or its window has not opened.
	FeedLifecycleApproved FeedLifecycle = "approved"
	// FeedLifecycleLive — the card is on the main screen right now.
	FeedLifecycleLive FeedLifecycle = "live"
	// FeedLifecycleExpired — the item's window has closed; whatever the
	// moderation state, it can never appear again.
	FeedLifecycleExpired FeedLifecycle = "expired"
)

// FeedLifecycleOf derives what the venue sees for item as of now. Expiry is
// checked FIRST and overrides every moderation state: an ended promo pending
// review is not "waiting", it is over.
func FeedLifecycleOf(item FeedItem, now time.Time) FeedLifecycle {
	if !item.EndsAt.After(now) {
		return FeedLifecycleExpired
	}
	switch item.Placement.Status {
	case FeedPendingReview:
		return FeedLifecycleSubmitted
	case FeedRejected:
		return FeedLifecycleRejected
	case FeedApproved:
		// Approved is not the same as visible: the read model additionally
		// requires the venue's own published status and, for a promo, a window
		// that has already opened (an event is promoted before it starts).
		if item.ItemStatus != string(PromoPublished) {
			return FeedLifecycleApproved
		}
		if item.Kind == FeedItemPromo && item.StartsAt.After(now) {
			return FeedLifecycleApproved
		}
		return FeedLifecycleLive
	default:
		return FeedLifecycleNotSubmitted
	}
}

// FeedStatusAfterContentEdit is the moderation status an item falls back to
// when the venue edits its content. A decision (approved or rejected) was made
// about specific words and dates; changing them invalidates it, so the item
// goes back into the queue. An item nobody decided on yet is left alone.
//
// The Postgres implementation of DemoteAfterContentEdit must agree with this
// function — it is the single written statement of the rule.
func FeedStatusAfterContentEdit(cur FeedStatus) FeedStatus {
	switch cur {
	case FeedApproved, FeedRejected:
		return FeedPendingReview
	default:
		return cur
	}
}

// FeedDemotableAfterContentEdit reports whether editing an item's content may
// invalidate the platform's moderation decision, given who owns the item.
//
// It does for VENUE content: the platform approved specific words, and the
// venue changing them is exactly the substitution moderation exists to catch.
// It does NOT for PLATFORM content (restaurantID nil, migration 0085) — the
// editor and the reviewer are the same superadmin, there is no second party to
// ask, and a demotion would take the platform's own card off the home screen
// until the platform re-approved itself. That is a review round trip with
// nobody on the other side, which is the same reason platform content is
// approved at creation in the first place.
//
// The Postgres implementation of FeedRepository.DemoteAfterContentEdit must
// agree with this function — it is the single written statement of the rule.
func FeedDemotableAfterContentEdit(restaurantID *uuid.UUID) bool { return restaurantID != nil }

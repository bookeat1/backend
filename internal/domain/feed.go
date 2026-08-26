package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// FeedItemKind says which venue-supplied entity a feed card is made of. There
// is deliberately no third "feed item" entity: the merchandising feed is a READ
// MODEL over the promos and events a venue already created (migration 0050), so
// a card is always one of these two rows.
type FeedItemKind string

const (
	// FeedItemPromo is a card made from a row of `promos`.
	FeedItemPromo FeedItemKind = "promo"
	// FeedItemEvent is a card made from a row of `events`.
	FeedItemEvent FeedItemKind = "event"
)

// Valid reports whether k is a known feed item kind.
func (k FeedItemKind) Valid() bool { return k == FeedItemPromo || k == FeedItemEvent }

// FeedStatus is the PLATFORM's moderation decision about showing one item on
// the app's main screen. It is a second axis on top of the venue's own
// publication status (PromoStatus/EventStatus): the venue decides whether the
// item exists publicly at all, the platform decides whether it gets the main
// screen. Both must be green for a card to appear.
//
// The vocabulary mirrors DraftStatus on purpose — this is the same
// human-in-the-loop shape the content-draft queue already established (propose
// → a human decides → terminal), only the decider is the platform superadmin
// rather than the venue's own staff.
type FeedStatus string

const (
	// FeedNotSubmitted is the default: the venue never asked for the main
	// screen. The item still shows on its own restaurant page as before.
	FeedNotSubmitted FeedStatus = "not_submitted"
	// FeedPendingReview means the venue submitted the item and the platform has
	// not decided yet. Never visible on the main screen.
	FeedPendingReview FeedStatus = "pending_review"
	// FeedApproved is the platform's yes. Necessary but not sufficient — the
	// item must also be published and still inside its window.
	FeedApproved FeedStatus = "approved"
	// FeedRejected is the platform's no; FeedPlacement.RejectionReason carries
	// the explanation. The venue may fix the item and submit it again.
	FeedRejected FeedStatus = "rejected"
)

// Valid reports whether s is a known feed status.
func (s FeedStatus) Valid() bool {
	switch s {
	case FeedNotSubmitted, FeedPendingReview, FeedApproved, FeedRejected:
		return true
	}
	return false
}

// MaxFeedPlacementWeight is the upper bound of the paid-placement lever. The
// scale is intentionally coarse (0..100, 0 = organic) because a human sells and
// sets it; a wider range would only create false precision. A DB CHECK enforces
// the same bounds.
const MaxFeedPlacementWeight = 100

// FeedPlacement is the platform-controlled merchandising state of ONE promo or
// event: the moderation decision plus the paid-placement weight. It is stored
// as columns on the item's own row (migration 0050), not as a separate entity —
// one item holds exactly one placement.
type FeedPlacement struct {
	Status FeedStatus
	// SubmittedAt is when the venue last asked for the main screen, nil while
	// the item was never submitted.
	SubmittedAt *time.Time
	// ReviewedBy/ReviewedAt record the platform superadmin who decided and when.
	// Both nil until a decision exists; cleared on a re-submission.
	ReviewedBy *uuid.UUID
	ReviewedAt *time.Time
	// RejectionReason is mandatory on a rejection and nil otherwise — the venue
	// must be told what to fix.
	RejectionReason *string
	// PlacementWeight is the paid-placement lever, 0..MaxFeedPlacementWeight,
	// settable ONLY by the platform superadmin. It survives status changes so a
	// sold placement is not lost when a venue re-submits an edited item.
	PlacementWeight int
}

// FeedItem is one card of the merchandising feed: the venue-supplied content
// (promo or event) denormalized together with the venue attributes the ranking
// and the card need, so the guest read is a single query with no per-card
// follow-up.
//
// The same struct serves the guest feed and the venue/platform state views;
// the ranking-only fields (RestaurantRating, ReviewCount,
// MatchesCuisinePreference, HasCuisinePreferences) are zero on the state views,
// which never rank anything.
type FeedItem struct {
	Kind FeedItemKind
	// ID is the underlying promo/event id. It is unique only WITHIN a kind —
	// (Kind, ID) is the card's identity.
	ID uuid.UUID

	// RestaurantID is the venue that supplied the item, nil when the PLATFORM
	// itself did (Promo.RestaurantID / Event.RestaurantID nil, migration 0085).
	// RestaurantName is then empty and the card draws no venue line.
	RestaurantID       *uuid.UUID
	RestaurantName     string
	RestaurantNameI18n I18n
	// City is the item's EFFECTIVE city — its own override when it has one,
	// otherwise the venue's (COALESCE(i.city, r.city)). nil means "no city at
	// all", which only a platform item with no override can be, and which
	// FeedEligible reads as "every city's main screen".
	City *City
	// CategoryID is the venue's cuisine/category (restaurant_categories), the
	// same dictionary user_cuisine_preferences points at. nil when untagged.
	CategoryID *uuid.UUID
	// VenueIsActive / VenueHiddenFromHome are the venue's own visibility flags.
	// They are carried (not just filtered on in SQL) so FeedEligible can state
	// the WHOLE eligibility rule in one readable place. For a PLATFORM item
	// there is no venue and they are meaningless: the read model projects the
	// neutral pair (true, false) and FeedEligible skips them explicitly, so
	// neither layer can accidentally hide the platform's own card.
	VenueIsActive       bool
	VenueHiddenFromHome bool

	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	// StartsAt/EndsAt is the item's own window. A promo is feed-eligible only
	// while StartsAt <= now < EndsAt; an event is eligible while EndsAt > now
	// (an upcoming event is worth promoting BEFORE it starts). This mirrors the
	// existing public listings exactly (PromoRepository.ListActive vs
	// EventRepository.ListPublishedUpcoming) rather than inventing a third rule.
	StartsAt time.Time
	EndsAt   time.Time
	// CoverImageURL is the card's picture, for both kinds (promos got their own
	// column in migration 0060). Nil means the item has no picture — never a
	// placeholder URL.
	CoverImageURL *string
	// Images is the item's gallery WITHOUT the cover, in the editor's order
	// (migration 0070). Empty for an item that has no extra photos — the card
	// then draws the cover alone, never a placeholder.
	Images []string
	// Terms is set for promos only (the fine print).
	Terms string
	// DiscountPercent is the promo card's «−30%» badge value, 0..100. Set for
	// PROMOS only — events have no discount, so the union projects NULL for them
	// and this stays nil. Nil (either kind) means "no badge".
	DiscountPercent *int
	// ItemStatus is the venue's own publication status — "draft"/"published"/
	// "hidden", the shared vocabulary of PromoStatus and EventStatus. Carried as
	// a plain string because a card does not care which of the two types it came
	// from; only "published" is ever feed-eligible.
	ItemStatus string

	Placement FeedPlacement

	// RestaurantRating / RestaurantReviewCount are the venue's published-review
	// aggregate (0/0 when unrated — an unrated venue is never penalized, see
	// ScoreFeedItem).
	RestaurantRating      float64
	RestaurantReviewCount int
	// MatchesCuisinePreference is true when the signed-in guest listed this
	// venue's category among their cuisine preferences (migration 0021).
	MatchesCuisinePreference bool
	// HasCuisinePreferences distinguishes "the guest has preferences and this
	// item does not match" from "the guest has no preferences at all". Without
	// it every anonymous feed would look like a universal mismatch.
	HasCuisinePreferences bool

	CreatedAt time.Time
}

// FeedQuery selects the candidate set for one main-screen read.
type FeedQuery struct {
	// City is the hard filter — the feed is city-scoped, which is the only
	// "distance" this schema can express (restaurants carry lat/lng but the
	// guest's position is not part of any request today).
	City City
	// UserID is the signed-in guest, nil for an anonymous one. Its only effect
	// is the cuisine-preference signal.
	UserID *uuid.UUID
	// Now is the instant the eligibility windows are evaluated against, passed
	// in so the read is reproducible in tests.
	Now time.Time
	// Limit caps the candidate window. The feed is a merchandising rail, not an
	// archive: the repository returns at most Limit items in a deterministic SQL
	// pre-order and the ranking then re-orders exactly those.
	Limit int
}

// FeedPlacementUpdate is one moderation write. Status is always applied; every
// pointer field is applied as given (nil = write NULL) except PlacementWeight,
// where nil means "leave the current weight alone" — a moderation decision must
// not silently reset a paid placement.
type FeedPlacementUpdate struct {
	Status          FeedStatus
	SubmittedAt     *time.Time
	ReviewedBy      *uuid.UUID
	ReviewedAt      *time.Time
	RejectionReason *string
	PlacementWeight *int
}

// FeedRepository is the merchandising feed's read model plus the moderation
// writes. It spans both promos and events on purpose — that union IS the read
// model, and splitting it per entity would force the guest read into two
// queries plus a merge.
type FeedRepository interface {
	// ListCandidates returns, in ONE query, every item eligible for q.City's
	// main screen at q.Now — published AND feed-approved AND inside its window,
	// at an active, not-hidden-from-home venue — already carrying the venue's
	// rating aggregate and the guest's cuisine-preference match. At most q.Limit
	// rows, in a deterministic SQL pre-order (paid weight desc, ends_at asc, id
	// asc) so the truncation itself is reproducible.
	ListCandidates(ctx context.Context, q FeedQuery) ([]FeedItem, error)
	// GetItem returns one item with its placement, regardless of status, so the
	// usecase can resolve the owning restaurant BEFORE authorizing. ErrNotFound
	// when absent. Ranking-only fields are not populated.
	GetItem(ctx context.Context, kind FeedItemKind, id uuid.UUID) (*FeedItem, error)
	// ListByRestaurant returns all of a restaurant's promos and events with
	// their feed placement — the venue's "where do my submissions stand" view.
	// Newest-created first with (kind, id) as the stable tie-break, paginated,
	// plus the total count.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]FeedItem, int, error)
	// ListByFeedStatus returns platform-wide items in the given feed status —
	// the superadmin's moderation queue. Oldest submission first (FIFO) with
	// (kind, id) as the stable tie-break, paginated, plus the total count.
	ListByFeedStatus(ctx context.Context, status FeedStatus, page, perPage int) ([]FeedItem, int, error)
	// TransitionFeedStatus is a compare-and-set on the item's feed_status: it
	// applies upd only when the current status is one of from. ErrNotFound when
	// the id is absent, ErrInvalidStatus when the current status is not in from
	// (a concurrent decision won). from must be non-empty.
	TransitionFeedStatus(ctx context.Context, kind FeedItemKind, id uuid.UUID, from []FeedStatus, upd FeedPlacementUpdate) error
	// SetPlacementWeight writes the paid-placement weight without touching the
	// moderation status. ErrNotFound when the id is absent.
	SetPlacementWeight(ctx context.Context, kind FeedItemKind, id uuid.UUID, weight int) error
	// DemoteAfterContentEdit pulls an item off the main screen when its content
	// changed after a decision: approved/rejected → pending_review, clearing the
	// previous decision. A no-op for not_submitted/pending_review items, and it
	// NEVER touches the placement weight (the venue did not buy or lose it by
	// editing). This is what stops "get an innocuous promo approved, then edit
	// the title" — see the ordering note in usecase/promos.
	DemoteAfterContentEdit(ctx context.Context, kind FeedItemKind, id uuid.UUID) error
}

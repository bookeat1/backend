// Package feed is the application logic for the app's main-screen
// merchandising rail (the "Акции" section): venues supply the content — promos
// and events they already create through usecase/promos and usecase/events —
// and the PLATFORM decides what is shown and in which order.
//
// Three actors, three postures:
//   - the GUEST reads Main. No auth required; a signed-in guest additionally
//     gets their cuisine preferences folded into the ranking. One repository
//     call, no per-card follow-up query.
//   - the VENUE submits one of its own items and watches its state. Gated by
//     PermRestaurantManage at the item's OWN restaurant (superadmin bypasses) —
//     the same posture as every other admin endpoint here. A venue can neither
//     approve itself nor touch its own placement weight.
//   - the PLATFORM superadmin approves/rejects and sets the paid-placement
//     weight. domain.RoleAdmin only, checked here as defense-in-depth behind
//     the router's RequireRole gate.
//
// The moderation gate is a hard filter in the read model, never a ranking
// penalty: an unapproved, hidden, draft or expired item is simply not in the
// candidate set, so no score can promote it onto the screen.
package feed

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// maxFeedCandidates caps the candidate window the ranking sorts. The feed is a
// merchandising rail, not an archive — a guest never scrolls past a few dozen
// cards, and an unbounded set would mean scoring a whole city's content on
// every main-screen open. Deep pages beyond the cap return empty rather than a
// second, differently-ordered batch.
const maxFeedCandidates = 500

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Actor is the authenticated caller for the venue and platform actions.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant".
// Bound to restaurants.ManagerUseCase in bootstrap.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// MainInput is one main-screen read. UserID is nil for an anonymous guest.
type MainInput struct {
	City    domain.City
	UserID  *uuid.UUID
	Page    int
	PerPage int
}

// MainResult is the ranked page plus the totals the client paginates with.
// Total is the size of the ranked candidate set (capped at maxFeedCandidates),
// not the number of eligible rows in the database — the two differ only for a
// city with more live content than the rail can ever show.
type MainResult struct {
	Items   []domain.RankedFeedItem
	Total   int
	Page    int
	PerPage int
}

// ReviewInput is the platform's decision. PlacementWeight is optional and lets
// the superadmin approve and price a placement in one call; nil leaves the
// current weight untouched.
type ReviewInput struct {
	Approve         bool
	RejectionReason string
	PlacementWeight *int
}

// Facade exposes the guest, venue and platform operations of the feed.
type Facade interface {
	// Main returns the ranked main-screen feed for a city. No authorization.
	Main(ctx context.Context, in MainInput) (*MainResult, error)

	// Submit asks the platform to put one of the venue's items on the main
	// screen. Requires PermRestaurantManage at the item's own restaurant.
	// ErrInvalidStatus if the item already expired or is already submitted /
	// approved (re-submitting an approved item would silently pull it off the
	// screen). A rejected item may be submitted again after a fix.
	Submit(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*domain.FeedItem, error)

	// Withdraw takes an item back out of the feed (or out of the queue). Same
	// authorization as Submit; the venue always keeps the right to stop
	// advertising. ErrInvalidStatus when the item was never submitted.
	Withdraw(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*domain.FeedItem, error)

	// GetState returns one item with its placement and derived lifecycle.
	// Requires PermRestaurantManage at the item's own restaurant.
	GetState(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*ItemState, error)

	// ListVenue returns all of a restaurant's items with their feed state.
	// Requires PermRestaurantManage at restaurantID.
	ListVenue(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]ItemState, int, error)

	// ListReviewQueue returns the platform-wide pending_review queue, oldest
	// submission first. Superadmin only.
	ListReviewQueue(ctx context.Context, actor Actor, page, perPage int) ([]ItemState, int, error)

	// Review approves or rejects a pending item, optionally setting the paid
	// placement weight in the same call. Superadmin only. A rejection must
	// carry a reason. ErrInvalidStatus when the item is not pending review.
	Review(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID, in ReviewInput) (*ItemState, error)

	// SetPlacementWeight sets the paid-placement lever (0..MaxFeedPlacementWeight)
	// without changing the moderation status. Superadmin ONLY — this is the
	// commercial dial, and a venue must never be able to turn its own.
	SetPlacementWeight(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID, weight int) (*ItemState, error)
}

// ItemState is an item plus the venue-facing lifecycle derived from it, so the
// caller never has to recompute "submitted / approved / live / expired" itself.
type ItemState struct {
	Item      domain.FeedItem
	Lifecycle domain.FeedLifecycle
}

type facade struct {
	repo  domain.FeedRepository
	perms permissionChecker
	clock func() time.Time
}

// NewFacade constructs the feed Facade.
func NewFacade(repo domain.FeedRepository, perms permissionChecker) Facade {
	return &facade{repo: repo, perms: perms, clock: time.Now}
}

func (f *facade) Main(ctx context.Context, in MainInput) (*MainResult, error) {
	if !in.City.Valid() {
		return nil, fmt.Errorf("%w: unknown city %q", domain.ErrValidation, in.City)
	}
	page, perPage := normalizePage(in.Page, in.PerPage)
	now := f.clock()

	// ONE query: the repository returns the eligible candidates already carrying
	// the venue's rating aggregate and the guest's preference match, so no card
	// triggers a follow-up read.
	candidates, err := f.repo.ListCandidates(ctx, domain.FeedQuery{
		City:   in.City,
		UserID: in.UserID,
		Now:    now,
		Limit:  maxFeedCandidates,
	})
	if err != nil {
		return nil, err
	}

	// Ranking is a pure domain function over the whole candidate set, and the
	// page is a slice of its total order. Paginating BEFORE ranking would let a
	// card appear on two pages (or on none) as scores shift between requests.
	ranked := domain.RankFeedItems(candidates, now)
	return &MainResult{
		Items:   pageOf(ranked, page, perPage),
		Total:   len(ranked),
		Page:    page,
		PerPage: perPage,
	}, nil
}

func (f *facade) Submit(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*domain.FeedItem, error) {
	item, err := f.resolveAndAuthorize(ctx, actor, kind, itemID)
	if err != nil {
		return nil, err
	}
	now := f.clock()
	// Submitting something that is already over would only pollute the queue.
	if !item.EndsAt.After(now) {
		return nil, fmt.Errorf("%w: this %s has already ended", domain.ErrInvalidStatus, kind)
	}
	// A CAS, not a read-then-write: the from-set is the arbiter, so two parallel
	// submissions cannot both "win" and produce two queue entries.
	err = f.repo.TransitionFeedStatus(ctx, kind, itemID,
		[]domain.FeedStatus{domain.FeedNotSubmitted, domain.FeedRejected},
		domain.FeedPlacementUpdate{
			Status:      domain.FeedPendingReview,
			SubmittedAt: &now,
			// A new submission wipes the previous decision — the platform is
			// being asked again, and a stale rejection reason on a pending item
			// would read as if it had already been refused.
			ReviewedBy:      nil,
			ReviewedAt:      nil,
			RejectionReason: nil,
		})
	if err != nil {
		return nil, err
	}
	return f.repo.GetItem(ctx, kind, itemID)
}

func (f *facade) Withdraw(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*domain.FeedItem, error) {
	if _, err := f.resolveAndAuthorize(ctx, actor, kind, itemID); err != nil {
		return nil, err
	}
	// Withdrawing keeps the rejection reason nil and the weight untouched: the
	// venue may submit again later and a sold placement is not lost by a
	// temporary withdrawal.
	err := f.repo.TransitionFeedStatus(ctx, kind, itemID,
		[]domain.FeedStatus{domain.FeedPendingReview, domain.FeedApproved},
		domain.FeedPlacementUpdate{Status: domain.FeedNotSubmitted})
	if err != nil {
		return nil, err
	}
	return f.repo.GetItem(ctx, kind, itemID)
}

func (f *facade) GetState(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*ItemState, error) {
	item, err := f.resolveAndAuthorize(ctx, actor, kind, itemID)
	if err != nil {
		return nil, err
	}
	st := f.stateOf(*item)
	return &st, nil
}

func (f *facade) ListVenue(ctx context.Context, actor Actor, restaurantID uuid.UUID, page, perPage int) ([]ItemState, int, error) {
	if err := f.authorizeVenue(ctx, actor, &restaurantID); err != nil {
		return nil, 0, err
	}
	page, perPage = normalizePage(page, perPage)
	items, total, err := f.repo.ListByRestaurant(ctx, restaurantID, page, perPage)
	if err != nil {
		return nil, 0, err
	}
	return f.statesOf(items), total, nil
}

func (f *facade) ListReviewQueue(ctx context.Context, actor Actor, page, perPage int) ([]ItemState, int, error) {
	if err := authorizePlatform(actor); err != nil {
		return nil, 0, err
	}
	page, perPage = normalizePage(page, perPage)
	items, total, err := f.repo.ListByFeedStatus(ctx, domain.FeedPendingReview, page, perPage)
	if err != nil {
		return nil, 0, err
	}
	return f.statesOf(items), total, nil
}

func (f *facade) Review(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID, in ReviewInput) (*ItemState, error) {
	if err := authorizePlatform(actor); err != nil {
		return nil, err
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: unknown feed item kind %q", domain.ErrValidation, kind)
	}
	reason := strings.TrimSpace(in.RejectionReason)
	// A rejection the venue cannot act on is worse than no rejection at all.
	if !in.Approve && reason == "" {
		return nil, fmt.Errorf("%w: a rejection must carry a reason", domain.ErrValidation)
	}
	if in.PlacementWeight != nil {
		if err := validateWeight(*in.PlacementWeight); err != nil {
			return nil, err
		}
	}

	now := f.clock()
	upd := domain.FeedPlacementUpdate{
		Status:          domain.FeedRejected,
		ReviewedBy:      &actor.UserID,
		ReviewedAt:      &now,
		RejectionReason: &reason,
		PlacementWeight: in.PlacementWeight,
	}
	if in.Approve {
		upd.Status = domain.FeedApproved
		upd.RejectionReason = nil
	}
	// Only a pending item may be decided on. This is also what makes the
	// decision idempotency-safe: a duplicated approve click gets
	// ErrInvalidStatus rather than silently re-stamping the reviewer.
	if err := f.repo.TransitionFeedStatus(ctx, kind, itemID,
		[]domain.FeedStatus{domain.FeedPendingReview}, upd); err != nil {
		return nil, err
	}
	item, err := f.repo.GetItem(ctx, kind, itemID)
	if err != nil {
		return nil, err
	}
	st := f.stateOf(*item)
	return &st, nil
}

func (f *facade) SetPlacementWeight(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID, weight int) (*ItemState, error) {
	if err := authorizePlatform(actor); err != nil {
		return nil, err
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: unknown feed item kind %q", domain.ErrValidation, kind)
	}
	if err := validateWeight(weight); err != nil {
		return nil, err
	}
	if err := f.repo.SetPlacementWeight(ctx, kind, itemID, weight); err != nil {
		return nil, err
	}
	item, err := f.repo.GetItem(ctx, kind, itemID)
	if err != nil {
		return nil, err
	}
	st := f.stateOf(*item)
	return &st, nil
}

// --- helpers ---

// resolveAndAuthorize loads the item FIRST and authorizes against the
// restaurant it actually belongs to — never against a restaurant id supplied by
// the caller. That ordering is what stops a venue from acting on another
// venue's item, and it is the same shape usecase/promos and usecase/events use.
func (f *facade) resolveAndAuthorize(ctx context.Context, actor Actor, kind domain.FeedItemKind, itemID uuid.UUID) (*domain.FeedItem, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: unknown feed item kind %q", domain.ErrValidation, kind)
	}
	item, err := f.repo.GetItem(ctx, kind, itemID)
	if err != nil {
		return nil, err
	}
	if err := f.authorizeVenue(ctx, actor, item.RestaurantID); err != nil {
		return nil, err
	}
	return item, nil
}

// authorizeVenue gates an action on ONE item. restaurantID nil is a PLATFORM
// item (migration 0085): it has no venue whose staff could submit or withdraw
// it, so the global platform-content policy decides — today the superadmin,
// who would have passed the RoleAdmin bypass below anyway. Stating it
// explicitly is what keeps the seam in one place: when a marketer role is
// granted domain.PlatformContentRoles, it gains the feed submission of the
// platform's own cards with it, and not a single venue's.
func (f *facade) authorizeVenue(ctx context.Context, actor Actor, restaurantID *uuid.UUID) error {
	if restaurantID == nil {
		if !domain.CanManagePlatformContent(actor.Role) {
			return fmt.Errorf("%w: only the platform may manage feed placement of content that belongs to no venue", domain.ErrForbidden)
		}
		return nil
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, *restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's feed submissions", domain.ErrForbidden)
	}
	return nil
}

// authorizePlatform gates the moderation and the commercial dial on the global
// superadmin. Defense-in-depth behind the router's RequireRole(RoleAdmin), for
// the same reason usecase/dashboard re-checks it: a route mounted on the wrong
// group must not become an authorization hole.
func authorizePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform superadmin decides what reaches the main screen", domain.ErrForbidden)
	}
	return nil
}

func validateWeight(w int) error {
	if w < 0 || w > domain.MaxFeedPlacementWeight {
		return fmt.Errorf("%w: placement weight must be between 0 and %d", domain.ErrValidation, domain.MaxFeedPlacementWeight)
	}
	return nil
}

func (f *facade) stateOf(item domain.FeedItem) ItemState {
	return ItemState{Item: item, Lifecycle: domain.FeedLifecycleOf(item, f.clock())}
}

func (f *facade) statesOf(items []domain.FeedItem) []ItemState {
	now := f.clock()
	out := make([]ItemState, 0, len(items))
	for _, it := range items {
		out = append(out, ItemState{Item: it, Lifecycle: domain.FeedLifecycleOf(it, now)})
	}
	return out
}

func normalizePage(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// pageOf slices the ranked total order. An out-of-range page yields an empty
// list, never an error — a client scrolling past the end sees the rail end.
func pageOf(ranked []domain.RankedFeedItem, page, perPage int) []domain.RankedFeedItem {
	start := (page - 1) * perPage
	if start >= len(ranked) {
		return nil
	}
	end := start + perPage
	if end > len(ranked) {
		end = len(ranked)
	}
	return ranked[start:end]
}

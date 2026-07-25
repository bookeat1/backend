package feed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// testNow is the frozen clock every case is anchored to; the facade's clock is
// overridden with it so a window boundary never depends on when CI runs.
var testNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// fakeFeedRepo is an in-memory stand-in for the Postgres read model. Its
// ListCandidates deliberately applies domain.FeedEligible — the SAME predicate
// the SQL implements — so a test that asserts "an unapproved item never
// appears" is asserting against the written rule. Agreement between this
// predicate and the SQL is what an integration test against a real Postgres
// has to prove; a unit test cannot.
type fakeFeedRepo struct {
	items map[key]*domain.FeedItem
	// order fixes the iteration order of a Go map so the fake itself never
	// introduces the nondeterminism the ranking is supposed to remove.
	order []key

	transitions []transitionCall
	weights     []weightCall
	err         error
}

type key struct {
	kind domain.FeedItemKind
	id   uuid.UUID
}

type transitionCall struct {
	kind domain.FeedItemKind
	id   uuid.UUID
	from []domain.FeedStatus
	upd  domain.FeedPlacementUpdate
}

type weightCall struct {
	kind   domain.FeedItemKind
	id     uuid.UUID
	weight int
}

func newFakeRepo() *fakeFeedRepo {
	return &fakeFeedRepo{items: map[key]*domain.FeedItem{}}
}

func (f *fakeFeedRepo) put(it domain.FeedItem) *domain.FeedItem {
	k := key{it.Kind, it.ID}
	cp := it
	f.items[k] = &cp
	f.order = append(f.order, k)
	return &cp
}

func (f *fakeFeedRepo) ListCandidates(_ context.Context, q domain.FeedQuery) ([]domain.FeedItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.FeedItem
	for _, k := range f.order {
		it := *f.items[k]
		if !domain.FeedEligible(it, q.City, q.Now) {
			continue
		}
		// The repository resolves the preference match in SQL; the fake mirrors
		// the contract: no signed-in guest means no preferences at all.
		if q.UserID == nil {
			it.HasCuisinePreferences = false
			it.MatchesCuisinePreference = false
		}
		out = append(out, it)
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeFeedRepo) GetItem(_ context.Context, kind domain.FeedItemKind, id uuid.UUID) (*domain.FeedItem, error) {
	if f.err != nil {
		return nil, f.err
	}
	it, ok := f.items[key{kind, id}]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *it
	return &cp, nil
}

func (f *fakeFeedRepo) ListByRestaurant(_ context.Context, restaurantID uuid.UUID, _, _ int) ([]domain.FeedItem, int, error) {
	var out []domain.FeedItem
	for _, k := range f.order {
		if it := f.items[k]; it.RestaurantID == restaurantID {
			out = append(out, *it)
		}
	}
	return out, len(out), nil
}

func (f *fakeFeedRepo) ListByFeedStatus(_ context.Context, status domain.FeedStatus, _, _ int) ([]domain.FeedItem, int, error) {
	var out []domain.FeedItem
	for _, k := range f.order {
		if it := f.items[k]; it.Placement.Status == status {
			out = append(out, *it)
		}
	}
	return out, len(out), nil
}

func (f *fakeFeedRepo) TransitionFeedStatus(_ context.Context, kind domain.FeedItemKind, id uuid.UUID, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) error {
	it, ok := f.items[key{kind, id}]
	if !ok {
		return domain.ErrNotFound
	}
	matched := false
	for _, s := range from {
		if it.Placement.Status == s {
			matched = true
		}
	}
	if !matched {
		return domain.ErrInvalidStatus
	}
	f.transitions = append(f.transitions, transitionCall{kind: kind, id: id, from: from, upd: upd})
	it.Placement.Status = upd.Status
	it.Placement.SubmittedAt = upd.SubmittedAt
	it.Placement.ReviewedBy = upd.ReviewedBy
	it.Placement.ReviewedAt = upd.ReviewedAt
	it.Placement.RejectionReason = upd.RejectionReason
	if upd.PlacementWeight != nil {
		it.Placement.PlacementWeight = *upd.PlacementWeight
	}
	return nil
}

func (f *fakeFeedRepo) SetPlacementWeight(_ context.Context, kind domain.FeedItemKind, id uuid.UUID, weight int) error {
	it, ok := f.items[key{kind, id}]
	if !ok {
		return domain.ErrNotFound
	}
	f.weights = append(f.weights, weightCall{kind: kind, id: id, weight: weight})
	it.Placement.PlacementWeight = weight
	return nil
}

func (f *fakeFeedRepo) DemoteAfterContentEdit(_ context.Context, kind domain.FeedItemKind, id uuid.UUID) error {
	it, ok := f.items[key{kind, id}]
	if !ok {
		return nil
	}
	it.Placement.Status = domain.FeedStatusAfterContentEdit(it.Placement.Status)
	return nil
}

type fakePerms struct {
	roles map[[2]uuid.UUID]domain.StaffRole
	err   error
}

func (f *fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	role, ok := f.roles[[2]uuid.UUID{userID, restaurantID}]
	if !ok {
		return false, nil
	}
	return role.HasPermission(perm), nil
}

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

// newFacadeAt builds the facade with the frozen clock.
func newFacadeAt(repo domain.FeedRepository, perms permissionChecker) *facade {
	f := NewFacade(repo, perms).(*facade)
	f.clock = func() time.Time { return testNow }
	return f
}

// livePromo is an item that passes every eligibility gate, so a test can break
// exactly one condition.
func livePromo(rid uuid.UUID) domain.FeedItem {
	return domain.FeedItem{
		Kind:          domain.FeedItemPromo,
		ID:            uuid.New(),
		RestaurantID:  rid,
		City:          domain.CityAlmaty,
		VenueIsActive: true,
		ItemStatus:    string(domain.PromoPublished),
		StartsAt:      testNow.Add(-time.Hour),
		EndsAt:        testNow.Add(30 * 24 * time.Hour),
		CreatedAt:     testNow.Add(-30 * 24 * time.Hour),
		Placement:     domain.FeedPlacement{Status: domain.FeedApproved},
	}
}

// --- guest feed: moderation is a hard gate ---

func TestMain_OnlyApprovedItemsReachTheScreen(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()

	approved := repo.put(livePromo(rid))

	pending := livePromo(rid)
	pending.Placement.Status = domain.FeedPendingReview
	repo.put(pending)

	rejected := livePromo(rid)
	rejected.Placement.Status = domain.FeedRejected
	repo.put(rejected)

	never := livePromo(rid)
	never.Placement.Status = domain.FeedNotSubmitted
	repo.put(never)

	hidden := livePromo(rid)
	hidden.Placement.Status = domain.FeedApproved
	hidden.ItemStatus = string(domain.PromoHidden)
	repo.put(hidden)

	expired := livePromo(rid)
	expired.EndsAt = testNow.Add(-time.Minute)
	repo.put(expired)

	otherCity := livePromo(rid)
	otherCity.City = domain.CityAstana
	repo.put(otherCity)

	f := newFacadeAt(repo, &fakePerms{})
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty})
	if err != nil {
		t.Fatalf("the public feed must not error: %v", err)
	}
	if res.Total != 1 || len(res.Items) != 1 {
		t.Fatalf("only the approved, published, in-window item may appear, got %d", res.Total)
	}
	if res.Items[0].Item.ID != approved.ID {
		t.Fatalf("wrong item on the screen: %s", res.Items[0].Item.ID)
	}
}

func TestMain_UnknownCityIsRejected(t *testing.T) {
	f := newFacadeAt(newFakeRepo(), &fakePerms{})
	if _, err := f.Main(context.Background(), MainInput{City: "Атлантида"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown city must be ErrValidation, got %v", err)
	}
}

func TestMain_PaidPlacementOutranksAnOrganicItem(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	organic := repo.put(livePromo(rid))
	paidItem := livePromo(rid)
	paidItem.Placement.PlacementWeight = 40
	paid := repo.put(paidItem)

	f := newFacadeAt(repo, &fakePerms{})
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Items[0].Item.ID != paid.ID || res.Items[1].Item.ID != organic.ID {
		t.Fatalf("the paid placement must lead, got %s then %s", res.Items[0].Item.ID, res.Items[1].Item.ID)
	}
	// The rail is explainable: the leading card must say WHY it leads.
	var placementPoints int
	for _, r := range res.Items[0].Score.Reasons {
		if r.Code == domain.FeedSignalPlacement {
			placementPoints = r.Points
		}
	}
	if placementPoints != 400 {
		t.Fatalf("the score breakdown must attribute 400 points to the paid placement, got %d", placementPoints)
	}
}

func TestMain_AnonymousGuestGetsNoPreferenceBoost(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.HasCuisinePreferences = true
	it.MatchesCuisinePreference = true
	repo.put(it)

	f := newFacadeAt(repo, &fakePerms{})
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Items[0].Score.Total != 0 {
		t.Fatalf("an anonymous guest must get no personalization, scored %d", res.Items[0].Score.Total)
	}
}

func TestMain_PaginationSlicesOneTotalOrder(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	for i := 0; i < 5; i++ {
		it := livePromo(rid)
		// Descending weights so the expected order is unambiguous.
		it.Placement.PlacementWeight = 50 - i*10
		repo.put(it)
	}
	f := newFacadeAt(repo, &fakePerms{})

	all, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty, PerPage: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var paged []uuid.UUID
	for page := 1; page <= 3; page++ {
		res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty, Page: page, PerPage: 2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Total != 5 {
			t.Fatalf("total must be the whole ranked set, got %d", res.Total)
		}
		for _, r := range res.Items {
			paged = append(paged, r.Item.ID)
		}
	}
	if len(paged) != 5 {
		t.Fatalf("paging through must yield every card exactly once, got %d", len(paged))
	}
	for i, id := range paged {
		if all.Items[i].Item.ID != id {
			t.Fatalf("page %d position mismatch: paged %s, unpaged %s", i/2+1, id, all.Items[i].Item.ID)
		}
	}
	// Past the end is an empty rail, not an error.
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty, Page: 99, PerPage: 2})
	if err != nil || len(res.Items) != 0 {
		t.Fatalf("scrolling past the end must return an empty page, got %d items / %v", len(res.Items), err)
	}
}

// --- venue side: RBAC ---

func TestSubmit_ManagerOfTheOwningVenueMay(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedNotSubmitted
	item := repo.put(it)

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	got, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if err != nil {
		t.Fatalf("a manager of the owning venue must be able to submit: %v", err)
	}
	if got.Placement.Status != domain.FeedPendingReview {
		t.Fatalf("a submission must land in pending_review, got %s", got.Placement.Status)
	}
	if got.Placement.SubmittedAt == nil || !got.Placement.SubmittedAt.Equal(testNow) {
		t.Fatalf("the submission must be timestamped, got %v", got.Placement.SubmittedAt)
	}
}

func TestSubmit_AnotherVenuesItemIsForbidden(t *testing.T) {
	rid := uuid.New()
	otherRid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedNotSubmitted
	item := repo.put(it)

	// The actor owns a DIFFERENT restaurant.
	f := newFacadeAt(repo, permsWith(actorID, otherRid, domain.StaffRoleOwner))
	_, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must not submit another venue's item, got %v", err)
	}
	if len(repo.transitions) != 0 {
		t.Fatal("no state must change on a cross-tenant denial")
	}
}

func TestSubmit_HostessIsForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedNotSubmitted
	item := repo.put(it)

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleHostess))
	_, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not submit to the feed, got %v", err)
	}
}

func TestSubmit_ExpiredItemIsRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedNotSubmitted
	it.EndsAt = testNow.Add(-time.Minute)
	item := repo.put(it)

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	_, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("an ended item must not enter the queue, got %v", err)
	}
}

func TestSubmit_AlreadyApprovedIsRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid)) // approved

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	_, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("re-submitting an approved item must fail instead of silently unpublishing it, got %v", err)
	}
}

func TestSubmit_RejectedItemMayBeResubmittedAndLosesTheOldVerdict(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	reason := "too aggressive wording"
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedRejected
	it.Placement.RejectionReason = &reason
	item := repo.put(it)

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	got, err := f.Submit(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if err != nil {
		t.Fatalf("a fixed, previously rejected item must be re-submittable: %v", err)
	}
	if got.Placement.RejectionReason != nil {
		t.Fatalf("a re-submission must clear the old verdict, still %q", *got.Placement.RejectionReason)
	}
}

func TestGetState_AnotherVenueIsForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid))

	f := newFacadeAt(repo, permsWith(actorID, uuid.New(), domain.StaffRoleOwner))
	if _, err := f.GetState(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must not read another venue's submission, got %v", err)
	}
}

func TestListVenue_AnotherRestaurantIsForbidden(t *testing.T) {
	actorID := uuid.New()
	repo := newFakeRepo()
	f := newFacadeAt(repo, permsWith(actorID, uuid.New(), domain.StaffRoleOwner))
	if _, _, err := f.ListVenue(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, uuid.New(), 1, 20); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must not list another restaurant's submissions, got %v", err)
	}
}

// --- platform side: only the superadmin steers ---

func TestSetPlacementWeight_VenueOwnerIsForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid))

	// The owner of the item's OWN restaurant — the strongest venue role there is.
	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleOwner))
	_, err := f.SetPlacementWeight(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID, 100)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must not price its own placement, got %v", err)
	}
	if len(repo.weights) != 0 {
		t.Fatal("no weight must be written when the caller is not the platform")
	}
}

func TestReview_VenueOwnerCannotApproveItself(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedPendingReview
	item := repo.put(it)

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleOwner))
	_, err := f.Review(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant},
		domain.FeedItemPromo, item.ID, ReviewInput{Approve: true})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a venue must not approve its own submission, got %v", err)
	}
	if len(repo.transitions) != 0 {
		t.Fatal("no moderation state must change when the caller is not the platform")
	}
}

func TestReview_SuperadminApprovesAndPrices(t *testing.T) {
	rid := uuid.New()
	adminID := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedPendingReview
	item := repo.put(it)

	weight := 25
	f := newFacadeAt(repo, &fakePerms{err: errors.New("the permission checker must not be consulted for a superadmin")})
	st, err := f.Review(context.Background(), Actor{UserID: adminID, Role: domain.RoleAdmin},
		domain.FeedItemPromo, item.ID, ReviewInput{Approve: true, PlacementWeight: &weight})
	if err != nil {
		t.Fatalf("the superadmin must be able to approve: %v", err)
	}
	if st.Item.Placement.Status != domain.FeedApproved || st.Item.Placement.PlacementWeight != 25 {
		t.Fatalf("approval must set the status and the weight, got %+v", st.Item.Placement)
	}
	if st.Item.Placement.ReviewedBy == nil || *st.Item.Placement.ReviewedBy != adminID {
		t.Fatalf("the decision must record who made it, got %v", st.Item.Placement.ReviewedBy)
	}
	if st.Lifecycle != domain.FeedLifecycleLive {
		t.Fatalf("an approved, published, in-window promo must read as live, got %s", st.Lifecycle)
	}
}

func TestReview_RejectionRequiresAReason(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedPendingReview
	item := repo.put(it)

	f := newFacadeAt(repo, &fakePerms{})
	_, err := f.Review(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		domain.FeedItemPromo, item.ID, ReviewInput{Approve: false, RejectionReason: "   "})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a rejection the venue cannot act on must be refused, got %v", err)
	}
}

func TestReview_OnlyPendingItemsCanBeDecided(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid)) // already approved

	f := newFacadeAt(repo, &fakePerms{})
	_, err := f.Review(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		domain.FeedItemPromo, item.ID, ReviewInput{Approve: true})
	if !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("a duplicated decision must be refused, got %v", err)
	}
}

func TestSetPlacementWeight_OutOfRangeIsRejected(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid))
	f := newFacadeAt(repo, &fakePerms{})
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}

	for _, w := range []int{-1, domain.MaxFeedPlacementWeight + 1} {
		if _, err := f.SetPlacementWeight(context.Background(), admin, domain.FeedItemPromo, item.ID, w); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("weight %d must be ErrValidation, got %v", w, err)
		}
	}
	if len(repo.weights) != 0 {
		t.Fatal("an invalid weight must never reach the repository")
	}
}

func TestListReviewQueue_VenueOwnerIsForbidden(t *testing.T) {
	actorID := uuid.New()
	f := newFacadeAt(newFakeRepo(), permsWith(actorID, uuid.New(), domain.StaffRoleOwner))
	if _, _, err := f.ListReviewQueue(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, 1, 20); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the platform queue must be superadmin-only, got %v", err)
	}
}

// A superadmin bypasses the venue gate entirely — the same bypass usecase/promos
// and usecase/events keep, so platform support can act on any venue's item.
func TestSubmit_SuperadminBypassesTheVenueGate(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	it := livePromo(rid)
	it.Placement.Status = domain.FeedNotSubmitted
	item := repo.put(it)

	f := newFacadeAt(repo, &fakePerms{err: errors.New("must not be consulted for a superadmin")})
	if _, err := f.Submit(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, domain.FeedItemPromo, item.ID); err != nil {
		t.Fatalf("a superadmin must bypass the venue permission gate: %v", err)
	}
}

func TestSubmit_UnknownKindIsRejected(t *testing.T) {
	f := newFacadeAt(newFakeRepo(), &fakePerms{})
	_, err := f.Submit(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, "banner", uuid.New())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown kind must be ErrValidation, got %v", err)
	}
}

func TestWithdraw_TakesTheCardOffTheScreen(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid)) // approved and live

	f := newFacadeAt(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	got, err := f.Withdraw(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, domain.FeedItemPromo, item.ID)
	if err != nil {
		t.Fatalf("a venue must always be able to stop advertising: %v", err)
	}
	if got.Placement.Status != domain.FeedNotSubmitted {
		t.Fatalf("withdrawing must leave the item off the feed, got %s", got.Placement.Status)
	}
	// And it is really gone from the rail.
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("a withdrawn card must not be on the main screen, got %d", res.Total)
	}
}

// The moderation hole this closes: get something bland approved, then rewrite
// it. The edit path (usecase/promos, usecase/events) calls the same repository
// method the fake implements here.
func TestDemoteAfterContentEdit_PullsAnApprovedCardOffTheScreen(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	item := repo.put(livePromo(rid))

	if err := repo.DemoteAfterContentEdit(context.Background(), domain.FeedItemPromo, item.ID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f := newFacadeAt(repo, &fakePerms{})
	res, err := f.Main(context.Background(), MainInput{City: domain.CityAlmaty})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("an edited card must leave the main screen until it is reviewed again, got %d", res.Total)
	}
}

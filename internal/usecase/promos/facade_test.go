package promos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakePromoRepo struct {
	byID    map[uuid.UUID]*domain.Promo
	created *domain.Promo
	updated *domain.Promo
	active  []domain.Promo
}

func newFakeRepo() *fakePromoRepo { return &fakePromoRepo{byID: map[uuid.UUID]*domain.Promo{}} }

func (f *fakePromoRepo) Create(_ context.Context, p *domain.Promo) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	f.created = p
	f.byID[p.ID] = p
	return nil
}

func (f *fakePromoRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Promo, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakePromoRepo) Update(_ context.Context, p *domain.Promo) error {
	if _, ok := f.byID[p.ID]; !ok {
		return domain.ErrNotFound
	}
	f.updated = p
	f.byID[p.ID] = p
	return nil
}

func (f *fakePromoRepo) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.byID[id]; !ok {
		return domain.ErrNotFound
	}
	delete(f.byID, id)
	return nil
}

func (f *fakePromoRepo) ListByRestaurant(_ context.Context, _ uuid.UUID, _ []domain.PromoStatus, _, _ int) ([]domain.Promo, int, error) {
	return nil, 0, nil
}

func (f *fakePromoRepo) ListActive(_ context.Context, _ uuid.UUID, _ time.Time, _, _ int) ([]domain.Promo, int, error) {
	return f.active, len(f.active), nil
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

// fakeFeed records the demotions the facade asks for when content changes, and
// can fail on demand so the "demote first" ordering can be asserted.
type fakeFeed struct {
	demoted []uuid.UUID
	err     error
}

func (f *fakeFeed) DemoteAfterContentEdit(_ context.Context, kind domain.FeedItemKind, itemID uuid.UUID) error {
	if kind != domain.FeedItemPromo {
		return errors.New("promos must demote a promo, not " + string(kind))
	}
	if f.err != nil {
		return f.err
	}
	f.demoted = append(f.demoted, itemID)
	return nil
}

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

func validCreate(rid uuid.UUID) CreateInput {
	return CreateInput{
		RestaurantID: rid,
		Title:        "Happy Hour",
		StartsAt:     time.Now().Add(-time.Hour),
		EndsAt:       time.Now().Add(time.Hour),
	}
}

func TestCreate_HostessForbidden(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess), &fakeFeed{})

	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not create a promo, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no promo must be written when a hostess is denied")
	}
}

func TestCreate_ManagerAllowed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	p, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validCreate(rid))
	if err != nil {
		t.Fatalf("a manager must be able to create a promo: %v", err)
	}
	if p.Status != domain.PromoDraft {
		t.Fatalf("a new promo must default to draft, got %s", p.Status)
	}
}

func TestUpdate_CrossTenantForbidden(t *testing.T) {
	rid := uuid.New()
	other := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	pr := &domain.Promo{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.PromoDraft}
	repo.byID[pr.ID] = pr
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleOwner), &fakeFeed{})

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.PromoPublished}
	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, pr.ID, in)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an owner of another restaurant must not edit this promo, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a cross-tenant denial")
	}
}

func TestCreate_InvalidWindowRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	in := validCreate(rid)
	in.EndsAt = in.StartsAt // empty window
	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty window must be ErrValidation, got %v", err)
	}
}

// A venue that edits an approved promo must lose the approval: otherwise
// "get something innocuous approved, then rewrite it" is an open door onto the
// main screen.
func TestUpdate_DemotesFeedApprovalBeforeWriting(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	pr := &domain.Promo{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.PromoPublished}
	repo.byID[pr.ID] = pr
	fd := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), fd)

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.PromoPublished}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, pr.ID, in); err != nil {
		t.Fatalf("a manager must be able to edit their promo: %v", err)
	}
	if len(fd.demoted) != 1 || fd.demoted[0] != pr.ID {
		t.Fatalf("editing a promo must demote its feed placement, got %v", fd.demoted)
	}
}

// The ordering guarantee: if the demotion cannot be recorded, the edit must NOT
// happen — the reverse would leave unreviewed text live on the main screen.
func TestUpdate_AbortsWhenTheDemotionFails(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	pr := &domain.Promo{ID: uuid.New(), RestaurantID: rid, Title: "x", Status: domain.PromoPublished}
	repo.byID[pr.ID] = pr
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{err: errors.New("db down")})

	in := UpdateInput{Title: "y", StartsAt: time.Now(), EndsAt: time.Now().Add(time.Hour), Status: domain.PromoPublished}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, pr.ID, in); err == nil {
		t.Fatal("the edit must fail when the feed demotion fails")
	}
	if repo.updated != nil {
		t.Fatal("no content must be written when the feed placement could not be demoted")
	}
}

// A venue hiding and re-publishing its own approved promo must NOT be sent back
// to the moderation queue: Status is its own lever, and nothing a moderator read
// has changed. The text-edit case (which MUST demote) is covered above.
func TestUpdate_StatusOnlyChangeKeepsTheApproval(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	now := time.Now()
	pr := &domain.Promo{
		ID: uuid.New(), RestaurantID: rid, Title: "Кофе за полцены",
		Description: "до конца недели", Terms: "только в зале",
		StartsAt: now, EndsAt: now.Add(48 * time.Hour), Status: domain.PromoPublished,
	}
	repo.byID[pr.ID] = pr
	fd := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), fd)

	in := UpdateInput{
		Title: pr.Title, Description: pr.Description, Terms: pr.Terms,
		StartsAt: pr.StartsAt, EndsAt: pr.EndsAt,
		Status: domain.PromoDraft, // hiding it — the only change
	}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, pr.ID, in); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(fd.demoted) != 0 {
		t.Fatalf("a status-only change demoted the feed approval: %v", fd.demoted)
	}
}

// The card's picture is content a moderator approved: swapping it must send the
// promo back to the review queue, exactly like rewriting its title does.
func TestUpdate_ChangingTheCoverDemotesFromTheFeed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), feed)
	actor := Actor{UserID: actorID, Role: domain.RoleRestaurant}

	old := "https://pub-41b6f06fc8e74b6e959cdd6def081e22.r2.dev/promos/old.jpg"
	in := validCreate(rid)
	in.CoverImageURL = &old
	p, err := f.Create(context.Background(), actor, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.CoverImageURL == nil || *p.CoverImageURL != old {
		t.Fatalf("cover was not stored: %v", p.CoverImageURL)
	}

	unchanged := UpdateInput{
		Title: p.Title, StartsAt: p.StartsAt, EndsAt: p.EndsAt,
		CoverImageURL: &old, Status: domain.PromoPublished,
	}
	if _, err := f.Update(context.Background(), actor, p.ID, unchanged); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("republishing with the same cover must not demote: %v", feed.demoted)
	}

	newCover := "https://pub-41b6f06fc8e74b6e959cdd6def081e22.r2.dev/promos/new.jpg"
	swapped := unchanged
	swapped.CoverImageURL = &newCover
	updated, err := f.Update(context.Background(), actor, p.ID, swapped)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.CoverImageURL == nil || *updated.CoverImageURL != newCover {
		t.Fatalf("cover = %v, want the new one", updated.CoverImageURL)
	}
	if len(feed.demoted) != 1 || feed.demoted[0] != p.ID {
		t.Fatalf("changing the cover must demote the promo, got %v", feed.demoted)
	}

	// Removing the picture altogether is an edit too.
	removed := swapped
	removed.CoverImageURL = nil
	if _, err := f.Update(context.Background(), actor, p.ID, removed); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 2 {
		t.Fatalf("removing the cover must demote as well, got %v", feed.demoted)
	}
}

// A discount outside 0..100 is refused before anything is written — the usecase
// turns what would otherwise be a raw DB CHECK violation into a clean 422.
func TestCreate_DiscountOutOfRangeRejected(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})

	over := 150
	in := validCreate(rid)
	in.DiscountPercent = &over
	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a discount over 100 must be ErrValidation, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no promo must be written when the discount is out of range")
	}
}

// The «−N%» badge is content a moderator approved: changing the discount must
// send the promo back to the review queue, exactly like swapping the cover.
func TestUpdate_ChangingTheDiscountDemotesFromTheFeed(t *testing.T) {
	rid := uuid.New()
	actorID := uuid.New()
	repo := newFakeRepo()
	feed := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), feed)
	actor := Actor{UserID: actorID, Role: domain.RoleRestaurant}

	thirty := 30
	in := validCreate(rid)
	in.DiscountPercent = &thirty
	p, err := f.Create(context.Background(), actor, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.DiscountPercent == nil || *p.DiscountPercent != thirty {
		t.Fatalf("discount was not stored: %v", p.DiscountPercent)
	}

	// Republishing with the same discount is not an edit.
	unchanged := UpdateInput{
		Title: p.Title, StartsAt: p.StartsAt, EndsAt: p.EndsAt,
		DiscountPercent: &thirty, Status: domain.PromoPublished,
	}
	if _, err := f.Update(context.Background(), actor, p.ID, unchanged); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 0 {
		t.Fatalf("republishing with the same discount must not demote: %v", feed.demoted)
	}

	// Changing the badge value demotes.
	fifty := 50
	swapped := unchanged
	swapped.DiscountPercent = &fifty
	if _, err := f.Update(context.Background(), actor, p.ID, swapped); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 1 || feed.demoted[0] != p.ID {
		t.Fatalf("changing the discount must demote the promo, got %v", feed.demoted)
	}

	// Removing the badge altogether is an edit too.
	removed := swapped
	removed.DiscountPercent = nil
	if _, err := f.Update(context.Background(), actor, p.ID, removed); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(feed.demoted) != 2 {
		t.Fatalf("removing the discount must demote as well, got %v", feed.demoted)
	}
}

// См. комментарий у фейка событий: галерея в этих тестах не проверяется.
func (f *fakePromoRepo) ReplaceImages(_ context.Context, _ uuid.UUID, _ []string) error { return nil }

func (f *fakePromoRepo) ImagesByPromo(_ context.Context, _ []uuid.UUID) (map[uuid.UUID][]string, error) {
	return map[uuid.UUID][]string{}, nil
}

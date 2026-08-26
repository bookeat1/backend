package stories

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeStoryRepo is an in-memory domain.StoryRepository. Delete and Update are
// scoped to a restaurant id exactly like the Postgres repo, so the usecase's
// cross-tenant guarantees are actually exercised, not assumed.
type fakeStoryRepo struct {
	byID    map[uuid.UUID]*domain.Story
	created *domain.Story
	updated *domain.Story
	deleted *[2]uuid.UUID // {id, restaurantID}
	listErr error
}

func newFakeRepo() *fakeStoryRepo {
	return &fakeStoryRepo{byID: map[uuid.UUID]*domain.Story{}}
}

func (f *fakeStoryRepo) ListActiveByRestaurant(_ context.Context, rid uuid.UUID) ([]domain.Story, error) {
	var out []domain.Story
	for _, s := range f.byID {
		if s.RestaurantID == rid && s.IsActive {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

func (f *fakeStoryRepo) ListByRestaurant(_ context.Context, rid uuid.UUID) ([]domain.Story, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []domain.Story
	for _, s := range f.byID {
		if s.RestaurantID == rid {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

func (f *fakeStoryRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Story, error) {
	s, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStoryRepo) Create(_ context.Context, s *domain.Story) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	f.created = s
	cp := *s
	f.byID[s.ID] = &cp
	return nil
}

func (f *fakeStoryRepo) Update(_ context.Context, s *domain.Story) error {
	cur, ok := f.byID[s.ID]
	if !ok || cur.RestaurantID != s.RestaurantID {
		return domain.ErrNotFound
	}
	f.updated = s
	cp := *s
	f.byID[s.ID] = &cp
	return nil
}

func (f *fakeStoryRepo) Delete(_ context.Context, id, restaurantID uuid.UUID) error {
	cur, ok := f.byID[id]
	if !ok || cur.RestaurantID != restaurantID {
		return domain.ErrNotFound
	}
	f.deleted = &[2]uuid.UUID{id, restaurantID}
	delete(f.byID, id)
	return nil
}

func (f *fakeStoryRepo) Reorder(_ context.Context, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error {
	for i, id := range orderedIDs {
		s, ok := f.byID[id]
		if !ok || s.RestaurantID != restaurantID {
			continue // foreign / unknown id: ignored, exactly like the SQL join
		}
		s.SortOrder = i
	}
	return nil
}

// fakePerms mirrors the promos test double: a role granted per (user, restaurant)
// pair, resolved through the shared RBAC permission matrix.
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

func managerActor(id uuid.UUID) Actor { return Actor{UserID: id, Role: domain.RoleRestaurant} }

func strptr(s string) *string { return &s }

func seedStory(repo *fakeStoryRepo, rid uuid.UUID, sortOrder int, active bool) *domain.Story {
	s := &domain.Story{ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/x.jpg", SortOrder: sortOrder, IsActive: active}
	repo.byID[s.ID] = s
	return s
}

// --- Create ---

func TestCreateStory_ManagerHappyPath(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	cap := "  Летнее меню  "
	s, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid,
		ImageURL:     "https://cdn.book-eat.com/stories/a.jpg",
		Caption:      &cap,
	})
	if err != nil {
		t.Fatalf("a manager must be able to create a story: %v", err)
	}
	if s.Caption == nil || *s.Caption != "Летнее меню" {
		t.Fatalf("caption must be trimmed, got %v", s.Caption)
	}
	if !s.IsActive {
		t.Fatalf("a new story must default to active")
	}
	if s.SortOrder != 0 {
		t.Fatalf("first story must default to sort_order 0, got %d", s.SortOrder)
	}
}

func TestCreateStory_HostessForbidden(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess))

	_, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid, ImageURL: "https://cdn/a.jpg",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not create a story, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no story must be written when a hostess is denied")
	}
}

func TestCreateStory_EmptyImageURLRejected(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	_, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{RestaurantID: rid, ImageURL: "   "})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an empty image_url must be ErrValidation, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("no story must be written on a validation failure")
	}
}

func TestCreateStory_NonHTTPImageURLRejected(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	_, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid, ImageURL: "javascript:alert(1)",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a non-http(s) image_url must be ErrValidation, got %v", err)
	}
}

func TestCreateStory_DefaultsSortOrderToEndOfList(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedStory(repo, rid, 0, true)
	seedStory(repo, rid, 5, false) // highest existing, even though inactive
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid, ImageURL: "https://cdn/a.jpg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.SortOrder != 6 {
		t.Fatalf("new story must land at end of list (max 5 + 1), got %d", s.SortOrder)
	}
}

func TestCreateStory_SuperadminBypassesRBAC(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	// perms grants nobody; only the admin-role bypass can let this through.
	f := NewFacade(repo, &fakePerms{})

	_, err := f.CreateStory(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, CreateInput{
		RestaurantID: rid, ImageURL: "https://cdn/a.jpg",
	})
	if err != nil {
		t.Fatalf("a superadmin must be able to create a story anywhere: %v", err)
	}
}

// --- Update ---

func TestUpdateStory_PartialLeavesUnsetFields(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cap := "old"
	cur := seedStory(repo, rid, 3, true)
	cur.Caption = &cap
	cur.ImageURL = "https://cdn/old.jpg"
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	newURL := "https://cdn/new.jpg"
	s, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{ImageURL: &newURL})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.ImageURL != newURL {
		t.Fatalf("image_url must change, got %q", s.ImageURL)
	}
	if s.Caption == nil || *s.Caption != "old" {
		t.Fatalf("an omitted caption must be left unchanged, got %v", s.Caption)
	}
	if s.SortOrder != 3 || !s.IsActive {
		t.Fatalf("omitted sort_order/is_active must be left unchanged, got %d/%v", s.SortOrder, s.IsActive)
	}
}

func TestUpdateStory_EmptyCaptionClearsIt(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cap := "old"
	cur := seedStory(repo, rid, 0, true)
	cur.Caption = &cap
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{Caption: strptr("   ")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.Caption != nil {
		t.Fatalf("an empty caption must clear to nil, got %v", *s.Caption)
	}
}

func TestUpdateStory_CrossTenantForbidden(t *testing.T) {
	rid, other, actorID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	// actor manages a DIFFERENT restaurant.
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleOwner))

	newURL := "https://cdn/new.jpg"
	_, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{ImageURL: &newURL})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an owner of another restaurant must not edit this story, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a cross-tenant denial")
	}
}

func TestUpdateStory_InvalidImageURLRejected(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	_, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{ImageURL: strptr("")})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("clearing image_url to empty must be ErrValidation, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("no update must happen on a validation failure")
	}
}

func TestUpdateStory_NotFound(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	_, err := f.UpdateStory(context.Background(), managerActor(actorID), uuid.New(), UpdateInput{})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("updating an absent story must be ErrNotFound, got %v", err)
	}
}

// --- Delete ---

func TestDeleteStory_HappyPathScopedToRestaurant(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	if err := f.DeleteStory(context.Background(), managerActor(actorID), cur.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if repo.deleted == nil || repo.deleted[0] != cur.ID || repo.deleted[1] != rid {
		t.Fatalf("delete must be scoped to the story's restaurant, got %v", repo.deleted)
	}
	if _, ok := repo.byID[cur.ID]; ok {
		t.Fatal("story must be gone")
	}
}

func TestDeleteStory_CrossTenantForbidden(t *testing.T) {
	rid, other, actorID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	f := NewFacade(repo, permsWith(actorID, other, domain.StaffRoleOwner))

	err := f.DeleteStory(context.Background(), managerActor(actorID), cur.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an owner of another restaurant must not delete this story, got %v", err)
	}
	if repo.deleted != nil {
		t.Fatal("no delete must happen on a cross-tenant denial")
	}
}

// --- Reorder ---

func TestReorderStories_HappyPath(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	a := seedStory(repo, rid, 0, true)
	b := seedStory(repo, rid, 1, true)
	c := seedStory(repo, rid, 2, true)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	// New order: c, a, b.
	if err := f.ReorderStories(context.Background(), managerActor(actorID), rid, []uuid.UUID{c.ID, a.ID, b.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if repo.byID[c.ID].SortOrder != 0 || repo.byID[a.ID].SortOrder != 1 || repo.byID[b.ID].SortOrder != 2 {
		t.Fatalf("reorder mismatch: c=%d a=%d b=%d",
			repo.byID[c.ID].SortOrder, repo.byID[a.ID].SortOrder, repo.byID[b.ID].SortOrder)
	}
}

func TestReorderStories_IgnoresForeignIDs(t *testing.T) {
	rid, other, actorID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	mine := seedStory(repo, rid, 0, true)
	foreign := seedStory(repo, other, 7, true) // another venue's card
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	// The foreign id is slipped into the list at position 0; it must be ignored,
	// and my card must still be renumbered by its own position (1).
	if err := f.ReorderStories(context.Background(), managerActor(actorID), rid, []uuid.UUID{foreign.ID, mine.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if repo.byID[foreign.ID].SortOrder != 7 {
		t.Fatalf("a foreign card must be untouched, got sort_order %d", repo.byID[foreign.ID].SortOrder)
	}
	if repo.byID[mine.ID].SortOrder != 1 {
		t.Fatalf("my card must be renumbered to its list position 1, got %d", repo.byID[mine.ID].SortOrder)
	}
}

func TestReorderStories_HostessForbidden(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	s := seedStory(repo, rid, 0, true)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess))

	err := f.ReorderStories(context.Background(), managerActor(actorID), rid, []uuid.UUID{s.ID})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not reorder stories, got %v", err)
	}
	if repo.byID[s.ID].SortOrder != 0 {
		t.Fatal("no reorder must happen on denial")
	}
}

// --- ListForAdmin ---

func TestListForAdmin_ReturnsInactiveToo(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	seedStory(repo, rid, 0, true)
	seedStory(repo, rid, 1, false)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	got, err := f.ListForAdmin(context.Background(), managerActor(actorID), rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("admin list must include inactive cards, got %d", len(got))
	}
}

func TestListForAdmin_Forbidden(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}) // grants nothing

	_, err := f.ListForAdmin(context.Background(), managerActor(actorID), rid)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a non-managing caller must be denied the admin list, got %v", err)
	}
}

// --- action_url: the EXTERNAL link a tap on the story follows ---
//
// These guard the field that is NOT image_url. image_url is where the picture
// lives; action_url is where the guest goes. Confusing the two is the whole
// hazard this feature carries, so every test below names the distinction.

func TestCreateStory_StoresActionURL(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid,
		ImageURL:     "https://cdn.book-eat.com/stories/a.jpg",
		ActionURL:    strptr("  https://book-eat.com/promo  "),
	})
	if err != nil {
		t.Fatalf("a manager must be able to attach an external link: %v", err)
	}
	if s.ActionURL == nil || *s.ActionURL != "https://book-eat.com/promo" {
		t.Fatalf("action_url must be trimmed and stored, got %v", s.ActionURL)
	}
	if s.ImageURL != "https://cdn.book-eat.com/stories/a.jpg" {
		t.Fatalf("the link must not touch image_url, got %q", s.ImageURL)
	}
	if repo.created == nil || repo.created.ActionURL == nil {
		t.Fatal("the link must reach the repository, not just the returned struct")
	}
}

func TestCreateStory_NoActionURLIsNil(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
		RestaurantID: rid, ImageURL: "https://cdn/a.jpg", ActionURL: strptr("   "),
	})
	if err != nil {
		t.Fatalf("a story without a link is valid: %v", err)
	}
	if s.ActionURL != nil {
		t.Fatalf("a blank link must be stored as NULL, got %v", *s.ActionURL)
	}
}

// TestCreateStory_DangerousActionURLRejected: the link is OPENED on the guest's
// phone, so a hostile scheme is code execution in a webview. Control characters
// must be stripped BEFORE the URL is parsed — "java\nscript:" is the classic
// way past a naive check — which is exactly what domain.ValidateExternalActionURL
// does, and why stories reuse it instead of growing a second, softer validator.
func TestCreateStory_DangerousActionURLRejected(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"java\nscript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"book-eat.com/promo",
		"https://user:pass@book-eat.com/promo",
		"https:///promo",
	} {
		t.Run(raw, func(t *testing.T) {
			rid, actorID := uuid.New(), uuid.New()
			repo := newFakeRepo()
			f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

			_, err := f.CreateStory(context.Background(), managerActor(actorID), CreateInput{
				RestaurantID: rid, ImageURL: "https://cdn/a.jpg", ActionURL: strptr(raw),
			})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%q must be refused as ErrValidation, got %v", raw, err)
			}
			if repo.created != nil {
				t.Fatal("nothing must be written when the link is refused")
			}
		})
	}
}

func TestUpdateStory_ActionURLLeftAloneWhenOmitted(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	cur.ActionURL = strptr("https://book-eat.com/promo")
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{
		ImageURL: strptr("https://cdn/new.jpg"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.ActionURL == nil || *s.ActionURL != "https://book-eat.com/promo" {
		t.Fatalf("changing the picture must not drop the link, got %v", s.ActionURL)
	}
	if s.ImageURL != "https://cdn/new.jpg" {
		t.Fatalf("image_url must be the field that changed, got %q", s.ImageURL)
	}
}

func TestUpdateStory_EmptyActionURLClearsIt(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	cur.ActionURL = strptr("https://book-eat.com/promo")
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	s, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{
		ActionURL: strptr(""),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.ActionURL != nil {
		t.Fatalf("an empty link must un-link the story, got %v", *s.ActionURL)
	}
}

func TestUpdateStory_DangerousActionURLRejected(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	cur := seedStory(repo, rid, 0, true)
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	_, err := f.UpdateStory(context.Background(), managerActor(actorID), cur.ID, UpdateInput{
		ActionURL: strptr("java\nscript:alert(1)"),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a smuggled javascript: link must be ErrValidation, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("nothing must be written when the link is refused")
	}
}

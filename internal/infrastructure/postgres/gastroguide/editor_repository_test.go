package gastroguide

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// The editor's guarantees are properties of SQL — an ordering written in one
// transaction under a deferred unique constraint, a gap closed on detach, a slug
// refused by a unique index. None of them can be checked against a fake, so all
// of these run on a real Postgres.

func editorSetup(t *testing.T) (*pgxpool.Pool, *EditorRepository, context.Context) {
	t.Helper()
	pool, _, ctx := setup(t)
	return pool, NewEditor(pool, sqltx.NewManager(pool)), ctx
}

// timePtrPast is a published_at safely in the past, for rows that must count as
// live wherever the guest predicate is involved.
func timePtrPast() *time.Time { return ptrTime(time.Now().Add(-time.Hour)) }

// positionsOf reads the raw (position, restaurant_id) pairs straight from the
// table, bypassing the repository: the point of these tests is what is STORED,
// and a read that sorts could hide a duplicate number.
func positionsOf(t *testing.T, pool *pgxpool.Pool, ctx context.Context, collectionID uuid.UUID) map[uuid.UUID]int {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT restaurant_id, position FROM gastroguide_collection_venues WHERE collection_id = $1`,
		collectionID)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]int{}
	for rows.Next() {
		var id uuid.UUID
		var pos int
		if err := rows.Scan(&id, &pos); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		out[id] = pos
	}
	return out
}

func assertNoDuplicatePositions(t *testing.T, positions map[uuid.UUID]int) {
	t.Helper()
	seen := map[int]uuid.UUID{}
	for id, pos := range positions {
		if other, dup := seen[pos]; dup {
			t.Fatalf("position %d is held by both %s and %s", pos, other, id)
		}
		seen[pos] = id
	}
}

func seedEditableCollection(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug string) uuid.UUID {
	t.Helper()
	return seedCollection(t, pool, ctx, collectionSeed{slug: slug, status: domain.GuideCollectionDraft})
}

// A full reversal is the hardest reorder there is: every venue's number changes
// and, applied row by row, the sequence passes through states where two rows
// claim the same slot. It works because all the numbers are written by ONE
// statement in ONE transaction and the unique (collection_id, position) is
// DEFERRABLE INITIALLY DEFERRED, so it is checked once at COMMIT.
func TestEditorReorderVenues_ReversalLeavesNoDuplicatePositions(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "kids")

	ids := make([]uuid.UUID, 0, 5)
	for i := 0; i < 5; i++ {
		id := seedVenue(t, pool, ctx, fmt.Sprintf("Заведение %d", i), true)
		if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: id}); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	reversed := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		reversed[len(ids)-1-i] = id
	}
	if err := repo.ReorderVenues(ctx, col, reversed); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	positions := positionsOf(t, pool, ctx, col)
	assertNoDuplicatePositions(t, positions)
	for i, id := range reversed {
		if got := positions[id]; got != i+1 {
			t.Errorf("venue %s is at %d, want %d", id, got, i+1)
		}
	}

	got, err := repo.ListCollectionVenueIDs(ctx, col)
	if err != nil {
		t.Fatalf("list ids: %v", err)
	}
	for i := range reversed {
		if got[i] != reversed[i] {
			t.Fatalf("read-back order = %v, want %v", got, reversed)
		}
	}
}

// A rotation by one (each venue moves up a slot, the first goes last) is the
// case that a naive per-row UPDATE cannot do at all under a non-deferred
// constraint. Included separately from the reversal because it is the shape a
// drag-and-drop actually produces.
func TestEditorReorderVenues_RotationIsAtomic(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "breakfasts")

	var ids []uuid.UUID
	for i := 0; i < 4; i++ {
		id := seedVenue(t, pool, ctx, fmt.Sprintf("Кафе %d", i), true)
		if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: id}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		ids = append(ids, id)
	}

	rotated := append(append([]uuid.UUID{}, ids[1:]...), ids[0])
	if err := repo.ReorderVenues(ctx, col, rotated); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	positions := positionsOf(t, pool, ctx, col)
	assertNoDuplicatePositions(t, positions)
	for i, id := range rotated {
		if positions[id] != i+1 {
			t.Errorf("venue %s at %d, want %d", id, positions[id], i+1)
		}
	}
}

// A reorder that does not name exactly the current members is refused whole:
// the client's screen is stale, and guessing what they meant would silently
// rewrite somebody's curation. Nothing at all is written — verified by comparing
// the stored positions before and after.
func TestEditorReorderVenues_StalePayloadIsRefusedAndWritesNothing(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "romantic")

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		id := seedVenue(t, pool, ctx, fmt.Sprintf("Ресторан %d", i), true)
		if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: id}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		ids = append(ids, id)
	}
	stranger := seedVenue(t, pool, ctx, "Чужое", true)
	before := positionsOf(t, pool, ctx, col)

	cases := map[string][]uuid.UUID{
		"a venue is missing":        {ids[0], ids[1]},
		"a venue is listed twice":   {ids[0], ids[0], ids[1]},
		"a stranger is listed":      {ids[0], ids[1], stranger},
		"an extra id is appended":   {ids[0], ids[1], ids[2], stranger},
		"the list is empty":         {},
		"only the stranger is sent": {stranger},
	}
	for name, order := range cases {
		err := repo.ReorderVenues(ctx, col, order)
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: got %v, want ErrValidation", name, err)
			continue
		}
		if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideOrderMismatch {
			t.Errorf("%s: code %q ok=%v, want %q", name, code, ok, domain.CodeGuideOrderMismatch)
		}
		after := positionsOf(t, pool, ctx, col)
		if len(after) != len(before) {
			t.Errorf("%s: membership changed", name)
		}
		for id, pos := range before {
			if after[id] != pos {
				t.Errorf("%s: venue %s moved from %d to %d despite the refusal", name, id, pos, after[id])
			}
		}
	}
}

// Reordering the same way twice is a no-op: the order is the intended FINAL
// sequence, not a move, so a client that retries after a lost response cannot
// scramble anything.
func TestEditorReorderVenues_IsIdempotent(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "brunch")

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		id := seedVenue(t, pool, ctx, fmt.Sprintf("Место %d", i), true)
		if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: id}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		ids = append(ids, id)
	}
	order := []uuid.UUID{ids[2], ids[0], ids[1]}
	for i := 0; i < 3; i++ {
		if err := repo.ReorderVenues(ctx, col, order); err != nil {
			t.Fatalf("reorder #%d: %v", i, err)
		}
	}
	positions := positionsOf(t, pool, ctx, col)
	assertNoDuplicatePositions(t, positions)
	for i, id := range order {
		if positions[id] != i+1 {
			t.Errorf("venue %s at %d, want %d", id, positions[id], i+1)
		}
	}
}

// Detaching closes the gap. Left open, the next attach (max+1) would drift the
// numbers away from the visible order until a reorder payload in a bug report
// could no longer be reasoned about.
func TestEditorDetachVenue_ClosesTheGap(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "gap")

	var ids []uuid.UUID
	for i := 0; i < 4; i++ {
		id := seedVenue(t, pool, ctx, fmt.Sprintf("Точка %d", i), true)
		if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: id}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		ids = append(ids, id)
	}
	if err := repo.DetachVenue(ctx, col, ids[1]); err != nil {
		t.Fatalf("detach: %v", err)
	}

	positions := positionsOf(t, pool, ctx, col)
	assertNoDuplicatePositions(t, positions)
	want := map[uuid.UUID]int{ids[0]: 1, ids[2]: 2, ids[3]: 3}
	if len(positions) != len(want) {
		t.Fatalf("positions = %v, want %v", positions, want)
	}
	for id, pos := range want {
		if positions[id] != pos {
			t.Errorf("venue %s at %d, want %d", id, positions[id], pos)
		}
	}

	// And the next attach lands right after the last one, not in a hole.
	next := seedVenue(t, pool, ctx, "Новое", true)
	if err := repo.AttachVenue(ctx, col, domain.GuideVenueAttachment{RestaurantID: next}); err != nil {
		t.Fatalf("attach after detach: %v", err)
	}
	if got := positionsOf(t, pool, ctx, col)[next]; got != 4 {
		t.Errorf("the new venue landed at %d, want 4", got)
	}

	if err := repo.DetachVenue(ctx, col, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("detach of a non-member: got %v, want ErrNotFound", err)
	}
}

// The same venue twice in ONE collection is refused with a code the panel can
// act on; the same venue in several DIFFERENT collections is the whole point of
// the guide and must keep working.
func TestEditorAttachVenue_SameCollectionTwiceRefusedManyCollectionsFine(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	first := seedEditableCollection(t, pool, ctx, "kids")
	second := seedEditableCollection(t, pool, ctx, "breakfasts")
	venue := seedVenue(t, pool, ctx, "Общее", true)

	if err := repo.AttachVenue(ctx, first, domain.GuideVenueAttachment{RestaurantID: venue}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	err := repo.AttachVenue(ctx, first, domain.GuideVenueAttachment{RestaurantID: venue})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("second attach: got %v, want ErrAlreadyExists", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideVenueAlreadyAttached {
		t.Errorf("second attach: code %q ok=%v, want %q", code, ok, domain.CodeGuideVenueAlreadyAttached)
	}
	if err := repo.AttachVenue(ctx, second, domain.GuideVenueAttachment{RestaurantID: venue}); err != nil {
		t.Errorf("attach to another collection: %v", err)
	}

	if err := repo.AttachVenue(ctx, uuid.New(), domain.GuideVenueAttachment{RestaurantID: venue}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("attach to an unknown collection: got %v, want ErrNotFound", err)
	}
	if err := repo.AttachVenue(ctx, first, domain.GuideVenueAttachment{RestaurantID: uuid.New()}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("attach of an unknown venue: got %v, want ErrNotFound", err)
	}
}

// A slug collision is a thing the editor fixes by typing a different slug, so it
// comes back as a coded 409 and not as a 500 with a Postgres string in it.
func TestEditorCreate_DuplicateSlugIsCoded(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	_ = pool

	in := domain.GuideCollectionWrite{Slug: "kids", Title: "С детьми"}
	if _, err := repo.CreateCollection(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := repo.CreateCollection(ctx, in)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate slug: got %v, want ErrAlreadyExists", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideSlugTaken {
		t.Errorf("duplicate slug: code %q ok=%v, want %q", code, ok, domain.CodeGuideSlugTaken)
	}

	cat := domain.GuideCategoryWrite{Slug: "breakfasts", Title: "Завтраки", IsActive: true}
	if _, err := repo.CreateCategory(ctx, cat); err != nil {
		t.Fatalf("create category: %v", err)
	}
	_, err = repo.CreateCategory(ctx, cat)
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideSlugTaken {
		t.Errorf("duplicate category slug: code %q ok=%v, want %q", code, ok, domain.CodeGuideSlugTaken)
	}
}

// The cabinet must see drafts and archived collections — that is now the ONLY
// thing that distinguishes it from the guest listing, which shows empty
// collections too. It must also keep reporting the guest-visible venue count,
// so the cabinet and the app never disagree about a number.
func TestEditorListCollectionsAdmin_ShowsDraftsAndArchivedToo(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	seedEditableCollection(t, pool, ctx, "draft-empty")
	published := seedCollection(t, pool, ctx, collectionSeed{
		slug: "live", status: domain.GuideCollectionPublished, publishedAt: timePtrPast(), position: 2})
	seedCollection(t, pool, ctx, collectionSeed{
		slug: "gone", status: domain.GuideCollectionArchived, publishedAt: timePtrPast(), position: 3})
	dark := seedVenue(t, pool, ctx, "Закрыто", false)
	if err := repo.AttachVenue(ctx, published, domain.GuideVenueAttachment{RestaurantID: dark}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	items, total, err := repo.ListCollectionsAdmin(ctx, domain.GuideCollectionAdminFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("total = %d, items = %d, want 3 and 3", total, len(items))
	}

	// venue_count is the GUEST-visible count, so a collection holding one
	// deactivated venue reports zero — the same number the app shows.
	for _, it := range items {
		if it.Slug == "live" && it.VenueCount != 0 {
			t.Errorf("live venue_count = %d, want 0 (its only venue is deactivated)", it.VenueCount)
		}
	}

	// …and the detail still shows that venue, flagged, so the editor can see WHY.
	detail, err := repo.GetCollectionAdmin(ctx, published)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(detail.Venues) != 1 {
		t.Fatalf("detail venues = %d, want 1", len(detail.Venues))
	}
	if detail.Venues[0].IsActive {
		t.Error("the deactivated venue is reported as active")
	}

	only, _, err := repo.ListCollectionsAdmin(ctx, domain.GuideCollectionAdminFilter{
		Statuses: []domain.GuideCollectionStatus{domain.GuideCollectionDraft}})
	if err != nil {
		t.Fatalf("list drafts: %v", err)
	}
	if len(only) != 1 || only[0].Slug != "draft-empty" {
		t.Errorf("draft filter returned %d rows, want just draft-empty", len(only))
	}
}

// Replacing a collection's rubric set is one atomic edit, and the order the
// editor gave is the order the collection holds inside each rubric.
func TestEditorSetCollectionCategories_ReplacesTheWholeSet(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedEditableCollection(t, pool, ctx, "kids")

	first, err := repo.CreateCategory(ctx, domain.GuideCategoryWrite{Slug: "breakfasts", Title: "Завтраки", IsActive: true})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	second, err := repo.CreateCategory(ctx, domain.GuideCategoryWrite{Slug: "morning", Title: "Утро", IsActive: true})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}

	if err := repo.SetCollectionCategories(ctx, col, []uuid.UUID{second.ID, first.ID}); err != nil {
		t.Fatalf("set categories: %v", err)
	}
	detail, err := repo.GetCollectionAdmin(ctx, col)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(detail.Categories) != 2 || detail.Categories[0].Slug != "morning" {
		t.Fatalf("categories = %v, want [morning breakfasts]", detail.Categories)
	}

	if err := repo.SetCollectionCategories(ctx, col, nil); err != nil {
		t.Fatalf("clear categories: %v", err)
	}
	detail, err = repo.GetCollectionAdmin(ctx, col)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(detail.Categories) != 0 {
		t.Errorf("categories = %v, want none", detail.Categories)
	}

	if err := repo.SetCollectionCategories(ctx, col, []uuid.UUID{uuid.New()}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown category: got %v, want ErrNotFound", err)
	}
}

// Editing a collection's text must not move it in or out of the guest's view:
// status and published_at are changed only by publish/unpublish/archive.
func TestEditorUpdateCollection_LeavesPublicationAlone(t *testing.T) {
	pool, repo, ctx := editorSetup(t)
	col := seedCollection(t, pool, ctx, collectionSeed{
		slug: "live", status: domain.GuideCollectionPublished, publishedAt: timePtrPast()})

	before, err := repo.GetCollectionAdmin(ctx, col)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := repo.UpdateCollection(ctx, col, domain.GuideCollectionWrite{
		Slug: "live", Title: "Переписанный заголовок", Subtitle: "И подзаголовок",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := repo.GetCollectionAdmin(ctx, col)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != before.Status {
		t.Errorf("status changed from %q to %q", before.Status, after.Status)
	}
	if after.PublishedAt == nil || !after.PublishedAt.Equal(*before.PublishedAt) {
		t.Errorf("published_at changed from %v to %v", before.PublishedAt, after.PublishedAt)
	}
	if after.Title != "Переписанный заголовок" {
		t.Errorf("title = %q, want the new one", after.Title)
	}
}

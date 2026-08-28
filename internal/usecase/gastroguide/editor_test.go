package gastroguide

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeEditorRepo is a hand-written double (no mock framework, per the repo's
// convention). It records what it was asked and answers from fields set by the
// test — the SQL-level guarantees (atomic reorder, gap closing, slug
// uniqueness) belong to the integration tests, not here.
type fakeEditorRepo struct {
	detail       *domain.GuideCollectionAdminDetail
	activeVenues int
	memberIDs    []uuid.UUID
	err          error

	gotStatus      domain.GuideCollectionStatus
	gotPublishedAt *time.Time
	statusCalls    int
	gotOrder       []uuid.UUID
	reorderCalls   int
	writes         int
	gotWrite       domain.GuideCollectionWrite
	gotAdminFilter domain.GuideCollectionAdminFilter
	gotCategoryIDs []uuid.UUID
}

func (f *fakeEditorRepo) ListAllCategories(context.Context) ([]domain.GuideCategory, error) {
	f.writes++
	return nil, f.err
}

func (f *fakeEditorRepo) CreateCategory(context.Context, domain.GuideCategoryWrite) (*domain.GuideCategory, error) {
	f.writes++
	return &domain.GuideCategory{ID: uuid.New()}, f.err
}

func (f *fakeEditorRepo) UpdateCategory(context.Context, uuid.UUID, domain.GuideCategoryWrite) (*domain.GuideCategory, error) {
	f.writes++
	return &domain.GuideCategory{ID: uuid.New()}, f.err
}

func (f *fakeEditorRepo) ListCollectionsAdmin(_ context.Context, filter domain.GuideCollectionAdminFilter) ([]domain.GuideCollection, int, error) {
	f.writes++
	f.gotAdminFilter = filter
	return nil, 0, f.err
}

func (f *fakeEditorRepo) GetCollectionAdmin(context.Context, uuid.UUID) (*domain.GuideCollectionAdminDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.detail == nil {
		return nil, domain.ErrNotFound
	}
	return f.detail, nil
}

func (f *fakeEditorRepo) CreateCollection(_ context.Context, in domain.GuideCollectionWrite) (*domain.GuideCollection, error) {
	f.writes++
	f.gotWrite = in
	return &domain.GuideCollection{ID: uuid.New()}, f.err
}

func (f *fakeEditorRepo) UpdateCollection(_ context.Context, _ uuid.UUID, in domain.GuideCollectionWrite) (*domain.GuideCollection, error) {
	f.writes++
	f.gotWrite = in
	return &domain.GuideCollection{ID: uuid.New()}, f.err
}

func (f *fakeEditorRepo) SetCollectionStatus(_ context.Context, _ uuid.UUID, status domain.GuideCollectionStatus, at *time.Time) (*domain.GuideCollection, error) {
	f.writes++
	f.statusCalls++
	f.gotStatus, f.gotPublishedAt = status, at
	if f.err != nil {
		return nil, f.err
	}
	return &domain.GuideCollection{Status: status, PublishedAt: at}, nil
}

func (f *fakeEditorRepo) CountActiveVenues(context.Context, uuid.UUID) (int, error) {
	return f.activeVenues, f.err
}

func (f *fakeEditorRepo) SetCollectionCategories(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	f.writes++
	f.gotCategoryIDs = ids
	return f.err
}

func (f *fakeEditorRepo) AttachVenue(context.Context, uuid.UUID, domain.GuideVenueAttachment) error {
	f.writes++
	return f.err
}

func (f *fakeEditorRepo) DetachVenue(context.Context, uuid.UUID, uuid.UUID) error {
	f.writes++
	return f.err
}

func (f *fakeEditorRepo) UpdateVenueNote(context.Context, uuid.UUID, uuid.UUID, string, domain.I18n) error {
	f.writes++
	return f.err
}

func (f *fakeEditorRepo) ReorderVenues(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	f.writes++
	f.reorderCalls++
	f.gotOrder = ids
	return f.err
}

func (f *fakeEditorRepo) ListCollectionVenueIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return f.memberIDs, f.err
}

var _ domain.GastroguideEditorRepository = (*fakeEditorRepo)(nil)

func superadmin() EditorActor {
	return EditorActor{UserID: uuid.New(), Role: domain.RoleAdmin}
}

// Every editor operation is superadmin-only. This walks the WHOLE surface with
// each non-admin role rather than spot-checking one method: a new endpoint that
// forgets the gate is exactly the kind of thing a spot check misses, and the
// guide is platform editorial content — a restaurant owner who could reach it
// could put their own venue into "лучшие завтраки".
func TestEditor_EveryOperationIsSuperadminOnly(t *testing.T) {
	roles := []domain.Role{domain.RoleUser, domain.RoleRestaurant, domain.Role("hostess")}
	ops := map[string]func(Editor, context.Context, EditorActor) error{
		"ListCategories": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.ListCategories(ctx, a)
			return err
		},
		"CreateCategory": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.CreateCategory(ctx, a, CategoryInput{Slug: "breakfasts", Title: "Завтраки"})
			return err
		},
		"UpdateCategory": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.UpdateCategory(ctx, a, uuid.New(), CategoryInput{Slug: "breakfasts", Title: "Завтраки"})
			return err
		},
		"ListCollections": func(e Editor, ctx context.Context, a EditorActor) error {
			_, _, err := e.ListCollections(ctx, a, AdminListInput{})
			return err
		},
		"GetCollection": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.GetCollection(ctx, a, uuid.New())
			return err
		},
		"CreateCollection": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.CreateCollection(ctx, a, CollectionInput{Slug: "kids", Title: "С детьми"})
			return err
		},
		"UpdateCollection": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.UpdateCollection(ctx, a, uuid.New(), CollectionInput{Slug: "kids", Title: "С детьми"})
			return err
		},
		"Publish": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.Publish(ctx, a, uuid.New(), nil)
			return err
		},
		"Unpublish": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.Unpublish(ctx, a, uuid.New())
			return err
		},
		"Archive": func(e Editor, ctx context.Context, a EditorActor) error {
			_, err := e.Archive(ctx, a, uuid.New())
			return err
		},
		"SetCategories": func(e Editor, ctx context.Context, a EditorActor) error {
			return e.SetCategories(ctx, a, uuid.New(), []uuid.UUID{uuid.New()})
		},
		"AttachVenue": func(e Editor, ctx context.Context, a EditorActor) error {
			return e.AttachVenue(ctx, a, uuid.New(), AttachVenueInput{RestaurantID: uuid.New()})
		},
		"DetachVenue": func(e Editor, ctx context.Context, a EditorActor) error {
			return e.DetachVenue(ctx, a, uuid.New(), uuid.New())
		},
		"SetVenueNote": func(e Editor, ctx context.Context, a EditorActor) error {
			return e.SetVenueNote(ctx, a, uuid.New(), uuid.New(), "note", nil)
		},
		"ReorderVenues": func(e Editor, ctx context.Context, a EditorActor) error {
			return e.ReorderVenues(ctx, a, uuid.New(), []uuid.UUID{uuid.New()})
		},
	}

	for name, op := range ops {
		for _, role := range roles {
			repo := &fakeEditorRepo{detail: &domain.GuideCollectionAdminDetail{}, activeVenues: 5}
			e := NewEditor(repo)
			err := op(e, context.Background(), EditorActor{UserID: uuid.New(), Role: role})
			if !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("%s as %s: got %v, want ErrForbidden", name, role, err)
			}
			if repo.writes != 0 {
				t.Errorf("%s as %s: repository was touched %d times despite the refusal", name, role, repo.writes)
			}
		}
	}
}

// Публикация подборки БЕЗ заведений разрешена. Гостевой список больше не прячет
// такие подборки: редакционный материал про места вне каталога — это контент сам
// по себе, ради него человек и возвращается в приложение. Раньше здесь стоял
// отказ, и он держался ровно на том правиле, которого больше нет.
func TestEditor_PublishAllowsACollectionWithNoVenues(t *testing.T) {
	for name, active := range map[string]int{
		"no venues at all":        0,
		"every venue deactivated": 0,
		"one active venue":        1,
	} {
		repo := &fakeEditorRepo{
			detail:       &domain.GuideCollectionAdminDetail{GuideCollection: domain.GuideCollection{Slug: "kids", Title: "С детьми"}},
			activeVenues: active,
		}
		e := NewEditor(repo)
		if _, err := e.Publish(context.Background(), superadmin(), uuid.New(), nil); err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if repo.statusCalls != 1 {
			t.Errorf("%s: status written %d times, want 1", name, repo.statusCalls)
		}
	}
}

// A published row must carry a time (the DB CHECK says so). The usecase supplies
// `now` when the editor named none, and passes a named time through untouched —
// a time in the FUTURE is a scheduled publication, not an error.
func TestEditor_PublishSuppliesATimeAndKeepsAScheduledOne(t *testing.T) {
	fixed := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	detail := &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{Slug: "kids", Title: "С детьми"},
	}

	t.Run("defaults to now", func(t *testing.T) {
		repo := &fakeEditorRepo{detail: detail, activeVenues: 1}
		e := NewEditor(repo).(*editor)
		e.clock = func() time.Time { return fixed }
		if _, err := e.Publish(context.Background(), superadmin(), uuid.New(), nil); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if repo.gotStatus != domain.GuideCollectionPublished {
			t.Errorf("status = %q, want published", repo.gotStatus)
		}
		if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(fixed) {
			t.Errorf("published_at = %v, want %v", repo.gotPublishedAt, fixed)
		}
	})

	t.Run("keeps a future time", func(t *testing.T) {
		later := fixed.Add(48 * time.Hour)
		repo := &fakeEditorRepo{detail: detail, activeVenues: 1}
		e := NewEditor(repo).(*editor)
		e.clock = func() time.Time { return fixed }
		if _, err := e.Publish(context.Background(), superadmin(), uuid.New(), &later); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(later) {
			t.Errorf("published_at = %v, want the scheduled %v", repo.gotPublishedAt, later)
		}
	})
}

// Unpublish clears published_at; Archive keeps it. An archived collection is one
// that WAS live, and losing the date loses that fact; a re-published draft, on
// the other hand, must not claim it has been live since whenever it first was.
func TestEditor_UnpublishClearsTheDateAndArchiveKeepsIt(t *testing.T) {
	was := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	detail := &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{Slug: "kids", Title: "С детьми", PublishedAt: &was},
	}

	repo := &fakeEditorRepo{detail: detail}
	if _, err := NewEditor(repo).Unpublish(context.Background(), superadmin(), uuid.New()); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if repo.gotStatus != domain.GuideCollectionDraft {
		t.Errorf("status = %q, want draft", repo.gotStatus)
	}
	if repo.gotPublishedAt != nil {
		t.Errorf("published_at = %v, want cleared", repo.gotPublishedAt)
	}

	repo = &fakeEditorRepo{detail: detail}
	if _, err := NewEditor(repo).Archive(context.Background(), superadmin(), uuid.New()); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if repo.gotStatus != domain.GuideCollectionArchived {
		t.Errorf("status = %q, want archived", repo.gotStatus)
	}
	if repo.gotPublishedAt == nil || !repo.gotPublishedAt.Equal(was) {
		t.Errorf("published_at = %v, want it kept at %v", repo.gotPublishedAt, was)
	}
}

// A slug ends up in a URL the app builds. Anything that is not lowercase latin
// with single hyphens is refused at the usecase, not stored and discovered in
// production as a dead link.
func TestEditor_RefusesAnUnusableSlugOrAnEmptyTitle(t *testing.T) {
	bad := []CollectionInput{
		{Slug: "", Title: "С детьми"},
		{Slug: "   ", Title: "С детьми"},
		{Slug: "с детьми", Title: "С детьми"},
		{Slug: "kids collection", Title: "С детьми"},
		{Slug: "kids--collection", Title: "С детьми"},
		{Slug: "-kids", Title: "С детьми"},
		{Slug: "kids", Title: "   "},
	}
	for _, in := range bad {
		repo := &fakeEditorRepo{}
		if _, err := NewEditor(repo).CreateCollection(context.Background(), superadmin(), in); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("slug %q title %q: got %v, want ErrValidation", in.Slug, in.Title, err)
		} else if repo.writes != 0 {
			t.Errorf("slug %q: written despite the refusal", in.Slug)
		}
	}

	// An upper-case slug is normalized, not refused: nobody should have to learn
	// that rule from an error message.
	repo := &fakeEditorRepo{}
	if _, err := NewEditor(repo).CreateCollection(context.Background(), superadmin(),
		CollectionInput{Slug: "  Kids-Friendly ", Title: " С детьми "}); err != nil {
		t.Fatalf("create: %v", err)
	}
}

// An empty cover URL is "no cover", not a cover whose address is the empty
// string: the guest response omits a nil cover and would otherwise hand the app
// "" and make it render a broken image.
func TestEditor_EmptyCoverURLBecomesNoCover(t *testing.T) {
	empty := "   "
	in := CollectionInput{Slug: "kids", Title: "С детьми", CoverImageURL: &empty}
	w, err := validateCollection(in)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if w.CoverImageURL != nil {
		t.Errorf("cover = %q, want nil", *w.CoverImageURL)
	}
}

// A blank translation is dropped rather than stored: I18n.Resolve answers with
// whatever the map holds, so {"kk": ""} makes a kk client see an empty title
// instead of falling back to the ru one.
func TestEditor_BlankTranslationsAreDroppedNotStored(t *testing.T) {
	w, err := validateCollection(CollectionInput{
		Slug: "kids", Title: "С детьми",
		TitleI18n:    domain.I18n{"kk": "  ", "en": "With kids"},
		SubtitleI18n: domain.I18n{"kk": ""},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, ok := w.TitleI18n["kk"]; ok {
		t.Error("blank kk title was stored")
	}
	if w.TitleI18n["en"] != "With kids" {
		t.Errorf("en title = %q, want it kept", w.TitleI18n["en"])
	}
	if w.SubtitleI18n != nil {
		t.Errorf("subtitle i18n = %v, want nil once its only entry was blank", w.SubtitleI18n)
	}
}

// The reorder payload reaches the repository unchanged and in order: the
// membership check happens once, in SQL, under the collection's row lock. A
// usecase that re-sorted or de-duplicated here would hide a stale client instead
// of refusing it.
func TestEditor_ReorderPassesTheOrderThroughUntouched(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	repo := &fakeEditorRepo{}
	if err := NewEditor(repo).ReorderVenues(context.Background(), superadmin(), uuid.New(), []uuid.UUID{c, a, b}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if len(repo.gotOrder) != 3 || repo.gotOrder[0] != c || repo.gotOrder[1] != a || repo.gotOrder[2] != b {
		t.Errorf("order = %v, want [%s %s %s]", repo.gotOrder, c, a, b)
	}

	repo = &fakeEditorRepo{}
	err := NewEditor(repo).ReorderVenues(context.Background(), superadmin(), uuid.New(), []uuid.UUID{a, uuid.Nil})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("empty id: got %v, want ErrValidation", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideOrderMismatch {
		t.Errorf("empty id: code %q ok=%v, want %q", code, ok, domain.CodeGuideOrderMismatch)
	}
	if repo.reorderCalls != 0 {
		t.Error("empty id reached the repository")
	}
}

// См. остальные фейки: метод нужен для соответствия интерфейсу, поведение
// подсветки проверяется отдельно.
func (r *fakeEditorRepo) SetVenueHighlight(_ context.Context, _, _ uuid.UUID, _, _ *uuid.UUID) error {
	return nil
}

// --- articles vs collections (migration 0092) ---

// An admin build that predates the split posts no `kind` at all. Its creates
// must keep producing exactly what they always produced — a collection — and
// not a row of some empty third kind the CHECK would reject.
func TestEditor_KindDefaultsToCollection(t *testing.T) {
	repo := &fakeEditorRepo{}
	e := NewEditor(repo)

	if _, err := e.CreateCollection(context.Background(), superadmin(),
		CollectionInput{Slug: "kids", Title: "С детьми"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.gotWrite.Kind != domain.GuideKindCollection {
		t.Fatalf("kind = %q, want %q", repo.gotWrite.Kind, domain.GuideKindCollection)
	}
}

// An explicit kind is carried through untouched.
func TestEditor_ExplicitArticleKindReachesTheRepository(t *testing.T) {
	repo := &fakeEditorRepo{}
	e := NewEditor(repo)

	if _, err := e.CreateCollection(context.Background(), superadmin(), CollectionInput{
		Slug: "chto-proishodit", Title: "Что происходит", Kind: domain.GuideKindArticle,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.gotWrite.Kind != domain.GuideKindArticle {
		t.Fatalf("kind = %q, want %q", repo.gotWrite.Kind, domain.GuideKindArticle)
	}
}

// A kind we cannot store is refused with a machine-readable code and NOTHING is
// written. Coercing it to "collection" would silently put a piece the editor
// meant as an article into the guide's rubric navigation.
func TestEditor_UnknownKindIsRefusedAndNothingIsWritten(t *testing.T) {
	for _, bad := range []domain.GuideCollectionKind{"post", "Collection ", "статья"} {
		repo := &fakeEditorRepo{}
		e := NewEditor(repo)

		_, err := e.CreateCollection(context.Background(), superadmin(), CollectionInput{
			Slug: "kids", Title: "С детьми", Kind: bad,
		})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("kind %q: err = %v, want ErrValidation", bad, err)
		}
		if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideUnknownKind {
			t.Fatalf("kind %q: code = %q, want %q", bad, code, domain.CodeGuideUnknownKind)
		}
		if repo.writes != 0 {
			t.Fatalf("kind %q: %d write(s) reached the repository", bad, repo.writes)
		}
	}
}

// Turning an item that carries rubrics into an article is refused rather than
// silently dropping the rubrics: dropping them is a destructive edit the editor
// did not ask for and would not see in the response.
func TestEditor_ArticleWithRubricsIsRefusedOnUpdate(t *testing.T) {
	repo := &fakeEditorRepo{detail: &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{ID: uuid.New(), Slug: "kids", Title: "С детьми"},
		Categories:      []domain.GuideCategory{{ID: uuid.New(), Slug: "breakfasts"}},
	}}
	e := NewEditor(repo)

	_, err := e.UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "С детьми", Kind: domain.GuideKindArticle,
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideArticleHasRubrics {
		t.Fatalf("code = %q, want %q", code, domain.CodeGuideArticleHasRubrics)
	}
	if repo.writes != 0 {
		t.Fatalf("%d write(s) reached the repository", repo.writes)
	}
}

// The same item WITHOUT rubrics converts freely — that is how a collection
// becomes an article once its rubrics are detached.
func TestEditor_RubriclessCollectionMayBecomeAnArticle(t *testing.T) {
	repo := &fakeEditorRepo{detail: &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{ID: uuid.New(), Slug: "kids", Title: "С детьми"},
	}}
	e := NewEditor(repo)

	if _, err := e.UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "С детьми", Kind: domain.GuideKindArticle,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.gotWrite.Kind != domain.GuideKindArticle {
		t.Fatalf("kind = %q, want %q", repo.gotWrite.Kind, domain.GuideKindArticle)
	}
}

// The invariant holds from the other direction too: attaching a rubric to an
// existing article is refused.
func TestEditor_AttachingRubricsToAnArticleIsRefused(t *testing.T) {
	repo := &fakeEditorRepo{detail: &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{
			ID: uuid.New(), Slug: "chto-proishodit", Kind: domain.GuideKindArticle,
		},
	}}
	e := NewEditor(repo)

	err := e.SetCategories(context.Background(), superadmin(), uuid.New(), []uuid.UUID{uuid.New()})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideArticleHasRubrics {
		t.Fatalf("code = %q, want %q", code, domain.CodeGuideArticleHasRubrics)
	}
	if repo.writes != 0 {
		t.Fatalf("%d write(s) reached the repository", repo.writes)
	}
}

// DETACHING every rubric from an article is legal: an empty list is how the
// invariant is satisfied, so refusing it would make an article that somehow
// acquired rubrics impossible to fix.
func TestEditor_ClearingRubricsOnAnArticleIsAllowed(t *testing.T) {
	repo := &fakeEditorRepo{detail: &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{
			ID: uuid.New(), Slug: "chto-proishodit", Kind: domain.GuideKindArticle,
		},
	}}
	e := NewEditor(repo)

	if err := e.SetCategories(context.Background(), superadmin(), uuid.New(), nil); err != nil {
		t.Fatalf("clear rubrics: %v", err)
	}
	if repo.writes != 1 {
		t.Fatalf("writes = %d, want 1", repo.writes)
	}
}

// The cabinet listing passes its ?kind= through, and refuses an unknown one
// instead of answering with a silently empty page.
func TestEditor_ListCollectionsKindFilter(t *testing.T) {
	repo := &fakeEditorRepo{}
	e := NewEditor(repo)
	kind := domain.GuideKindArticle

	if _, _, err := e.ListCollections(context.Background(), superadmin(),
		AdminListInput{Kind: &kind}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if repo.gotAdminFilter.Kind == nil || *repo.gotAdminFilter.Kind != domain.GuideKindArticle {
		t.Fatalf("filter kind = %v, want article", repo.gotAdminFilter.Kind)
	}

	bogus := domain.GuideCollectionKind("post")
	_, _, err := e.ListCollections(context.Background(), superadmin(), AdminListInput{Kind: &bogus})
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeGuideUnknownKind {
		t.Fatalf("code = %q, want %q", code, domain.CodeGuideUnknownKind)
	}
}

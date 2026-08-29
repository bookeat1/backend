package gastroguide

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
	uc "backend-core/internal/usecase/gastroguide"
)

// The merge itself is unit-tested in usecase/gastroguide. What can only be
// checked here is that it SURVIVES THE DATABASE: the maps travel through a
// jsonb column, and a language the merge preserved is worth nothing if the
// column round-trip drops it or mangles the encoding.
//
// The ko/zh rows are the reason this test exists. Some guide rows were imported
// with translations in languages the app cannot render. The policy is that they
// are preserved (deleting an editor's text because our own import was sloppy is
// not a fix) and unwritable (nothing may add another one), and both halves have
// to hold across a real UPDATE.

// editorUsecase wires the REAL repository into the usecase: the merge lives in
// the usecase and the storage in the repository, so only the two together prove
// anything about what an admin request leaves in the table.
func editorUsecase(t *testing.T) (uc.Editor, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, _, ctx := setup(t)
	return uc.NewEditor(NewEditor(pool, sqltx.NewManager(pool))), pool, ctx
}

func superadminActor() uc.EditorActor {
	return uc.EditorActor{UserID: uuid.New(), Role: domain.RoleAdmin}
}

func str(s string) *string { return &s }

// A partial edit through the real repository keeps every language it did not
// mention — the supported ones and the strays alike — and leaves title_i18n["ru"]
// equal to the title column.
func TestCollectionI18nPatch_RoundTripsThroughPostgres(t *testing.T) {
	e, pool, ctx := editorUsecase(t)

	id := seedEditableCollection(t, pool, ctx, "kids")
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_collections
		 SET title = 'С детьми',
		     title_i18n = '{"ru":"С детьми","kk":"Балалармен","en":"With kids","ko":"아이들과","zh":"带孩子"}'::jsonb,
		     description = 'Описание', description_i18n = '{"ru":"Описание","kk":"Сипаттама"}'::jsonb
		 WHERE id = $1`, id); err != nil {
		t.Fatalf("seed translations: %v", err)
	}

	if _, err := e.UpdateCollection(ctx, superadminActor(), id, uc.CollectionInput{
		Slug: "kids", Title: "С детьми", Description: "Описание",
		TitleI18n: domain.I18nPatch{"en": str("Kids welcome"), "kk": nil},
	}); err != nil {
		t.Fatalf("update collection: %v", err)
	}

	got, err := e.GetCollection(ctx, superadminActor(), id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.TitleI18n["en"] != "Kids welcome" {
		t.Errorf("en = %q, want the written translation", got.TitleI18n["en"])
	}
	if _, ok := got.TitleI18n["kk"]; ok {
		t.Error("kk survived a null")
	}
	if got.TitleI18n["ko"] != "아이들과" || got.TitleI18n["zh"] != "带孩子" {
		t.Errorf("title i18n = %v, want the stray locales preserved through the column", got.TitleI18n)
	}
	if got.TitleI18n["ru"] != got.Title {
		t.Errorf("title_i18n[ru] = %q, title = %q — the invariant broke", got.TitleI18n["ru"], got.Title)
	}
	// description_i18n was not mentioned at all and must be exactly as stored.
	if got.DescriptionI18n["kk"] != "Сипаттама" {
		t.Errorf("description i18n = %v, want it untouched", got.DescriptionI18n)
	}
}

// A rubric goes through GetCategory → merge → UpdateCategory, the read the
// partial write needs. Same guarantees, one field.
func TestCategoryI18nPatch_RoundTripsThroughPostgres(t *testing.T) {
	e, pool, ctx := editorUsecase(t)

	created, err := e.CreateCategory(ctx, superadminActor(), uc.CategoryInput{
		Slug: "breakfasts", Title: "Завтраки", IsActive: true,
		TitleI18n: domain.I18nPatch{"kk": str("Таңғы ас"), "en": str("Breakfasts")},
	})
	if err != nil {
		t.Fatalf("create category: %v", err)
	}
	if created.TitleI18n["ru"] != "Завтраки" {
		t.Errorf("ru = %q, want the column on create too", created.TitleI18n["ru"])
	}
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_categories SET title_i18n = title_i18n || '{"ko":"아침"}'::jsonb WHERE id = $1`,
		created.ID); err != nil {
		t.Fatalf("seed stray locale: %v", err)
	}

	updated, err := e.UpdateCategory(ctx, superadminActor(), created.ID, uc.CategoryInput{
		Slug: "breakfasts", Title: "Завтраки и бранчи", IsActive: true,
		TitleI18n: domain.I18nPatch{"en": nil},
	})
	if err != nil {
		t.Fatalf("update category: %v", err)
	}
	if _, ok := updated.TitleI18n["en"]; ok {
		t.Error("en survived a null")
	}
	if updated.TitleI18n["kk"] != "Таңғы ас" {
		t.Errorf("kk = %q, want the unmentioned translation kept", updated.TitleI18n["kk"])
	}
	if updated.TitleI18n["ko"] != "아침" {
		t.Errorf("title i18n = %v, want the stray locale preserved", updated.TitleI18n)
	}
	if updated.TitleI18n["ru"] != "Завтраки и бранчи" {
		t.Errorf("ru = %q, want the new title", updated.TitleI18n["ru"])
	}
}

// The note under a venue card is the field two editors are most likely to touch
// at the same time, and it lives in the join table rather than the collection.
func TestVenueNoteI18nPatch_RoundTripsThroughPostgres(t *testing.T) {
	e, pool, ctx := editorUsecase(t)

	col := seedEditableCollection(t, pool, ctx, "kids")
	venue := seedVenue(t, pool, ctx, "Daily Coffee", true)
	if err := e.AttachVenue(ctx, superadminActor(), col, uc.AttachVenueInput{
		RestaurantID: venue, Note: "Есть детская комната",
		NoteI18n: domain.I18nPatch{"kk": str("Балалар бөлмесі бар")},
	}); err != nil {
		t.Fatalf("attach venue: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE gastroguide_collection_venues SET note_i18n = note_i18n || '{"zh":"有儿童房"}'::jsonb
		 WHERE collection_id = $1 AND restaurant_id = $2`, col, venue); err != nil {
		t.Fatalf("seed stray locale: %v", err)
	}

	if err := e.SetVenueNote(ctx, superadminActor(), col, venue,
		"Есть детская комната и веранда", domain.I18nPatch{"en": str("Kids room")}); err != nil {
		t.Fatalf("set note: %v", err)
	}

	detail, err := e.GetCollection(ctx, superadminActor(), col)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(detail.Venues) != 1 {
		t.Fatalf("venues = %d, want 1", len(detail.Venues))
	}
	got := detail.Venues[0]
	if got.NoteI18n["kk"] != "Балалар бөлмесі бар" {
		t.Errorf("note i18n = %v, want kk kept", got.NoteI18n)
	}
	if got.NoteI18n["en"] != "Kids room" {
		t.Errorf("en = %q, want the written translation", got.NoteI18n["en"])
	}
	if got.NoteI18n["zh"] != "有儿童房" {
		t.Errorf("note i18n = %v, want the stray locale preserved", got.NoteI18n)
	}
	if got.NoteI18n["ru"] != got.Note {
		t.Errorf("note_i18n[ru] = %q, note = %q — the invariant broke", got.NoteI18n["ru"], got.Note)
	}
}

// A route and its stops, end to end: an unsupported language is refused BEFORE
// anything reaches the tables, and nothing about the row changes.
func TestRouteI18nPatch_RoundTripsThroughPostgres(t *testing.T) {
	pool, _, ctx := routeEditorSetup(t)
	e := uc.NewRouteEditor(NewRouteEditor(pool, sqltx.NewManager(pool)))

	route, err := e.CreateRoute(ctx, superadminActor(), uc.RouteInput{
		Slug: "classic-almaty", Title: "Классический тур", DurationLabel: "1 день",
		TitleI18n:         domain.I18nPatch{"kk": str("Классикалық тур")},
		DurationLabelI18n: domain.I18nPatch{"kk": str("1 күн")},
	})
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE gastro_routes SET title_i18n = title_i18n || '{"ko":"클래식 투어"}'::jsonb WHERE id = $1`,
		route.ID); err != nil {
		t.Fatalf("seed stray locale: %v", err)
	}

	point, err := e.AddPoint(ctx, superadminActor(), route.ID, uc.PointInput{
		Kind: domain.GuideRoutePointPlace, Title: "Утро: Панфиловцев",
		Address:     "парк 28 панфиловцев",
		AddressI18n: domain.I18nPatch{"en": str("Panfilov park")},
	})
	if err != nil {
		t.Fatalf("add point: %v", err)
	}

	updated, err := e.UpdateRoute(ctx, superadminActor(), route.ID, uc.RouteInput{
		Slug: "classic-almaty", Title: "Классический тур", DurationLabel: "1 день",
		TitleI18n: domain.I18nPatch{"en": str("The classic tour")},
	})
	if err != nil {
		t.Fatalf("update route: %v", err)
	}
	if updated.TitleI18n["kk"] != "Классикалық тур" || updated.TitleI18n["en"] != "The classic tour" {
		t.Errorf("title i18n = %v", updated.TitleI18n)
	}
	if updated.TitleI18n["ko"] != "클래식 투어" {
		t.Errorf("title i18n = %v, want the stray locale preserved", updated.TitleI18n)
	}
	if updated.TitleI18n["ru"] != updated.Title {
		t.Errorf("title_i18n[ru] = %q, title = %q — the invariant broke", updated.TitleI18n["ru"], updated.Title)
	}
	if updated.DurationLabelI18n["kk"] != "1 күн" {
		t.Errorf("duration i18n = %v, want the unmentioned field untouched", updated.DurationLabelI18n)
	}

	// An unsupported language is refused, and the stop is left exactly as it is.
	if _, err := e.UpdatePoint(ctx, superadminActor(), route.ID, point.ID, uc.PointInput{
		Kind: domain.GuideRoutePointPlace, Title: "Утро: Панфиловцев",
		Address:     "парк 28 панфиловцев",
		AddressI18n: domain.I18nPatch{"ko": str("판필로프 공원")},
	}); err == nil {
		t.Fatal("a korean address translation was accepted")
	}
	detail, err := e.GetRoute(ctx, superadminActor(), route.ID)
	if err != nil {
		t.Fatalf("read route: %v", err)
	}
	if len(detail.Points) != 1 || detail.Points[0].AddressI18n["en"] != "Panfilov park" {
		t.Fatalf("point address i18n = %v, want it untouched by the refused write", detail.Points)
	}
	if _, ok := detail.Points[0].AddressI18n["ko"]; ok {
		t.Error("the refused korean translation was stored anyway")
	}
}

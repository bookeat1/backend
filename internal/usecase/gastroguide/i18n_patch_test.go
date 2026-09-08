package gastroguide

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The gastroguide's admin writes take PARTIAL translation updates, the same
// domain.I18nPatch every other content type takes. What that has to mean, and
// what these tests pin down:
//
//   - a key with a string writes that language;
//   - a key with null (or a blank string) removes it;
//   - a language the object does not mention is left exactly as stored — this
//     is the one the old full-replace protocol could not express, and the
//     reason a second editor's Kazakh used to vanish when the first saved
//     English from a form opened minutes earlier;
//   - i18n["ru"] is the plain column, always;
//   - a language outside ru/kk/en is refused, and so is the same language
//     spelled twice.
//
// The ko/zh cases are not hypothetical: rows imported before the guide had a
// locale policy carry them. They must survive an edit (losing text nobody asked
// us to delete is worse than storing text nothing reads) and must stay
// unwritable, which is exactly what "preserved by the merge, refused by the
// validation" means.

func storedCollection(title, subtitle string, t18n, s18n domain.I18n) *domain.GuideCollectionAdminDetail {
	return &domain.GuideCollectionAdminDetail{
		GuideCollection: domain.GuideCollection{
			ID: uuid.New(), Slug: "kids", Title: title, TitleI18n: t18n,
			Subtitle: subtitle, SubtitleI18n: s18n,
			Kind: domain.GuideKindCollection,
		},
	}
}

// The bug this whole change exists for: an editor saving English must not wipe
// the Kazakh a colleague wrote while their form was open.
func TestCollectionPatch_KeepsTheLanguageItDoesNotMention(t *testing.T) {
	repo := &fakeEditorRepo{detail: storedCollection("С детьми", "",
		domain.I18n{"ru": "С детьми", "kk": "Балалармен", "en": "With kids"}, nil)}
	e := NewEditor(repo)

	if _, err := e.UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "С детьми",
		TitleI18n: domain.I18nPatch{"en": str("Kids welcome")},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.gotWrite.TitleI18n
	if got["kk"] != "Балалармен" {
		t.Errorf("kk = %q, want the stored translation kept", got["kk"])
	}
	if got["en"] != "Kids welcome" {
		t.Errorf("en = %q, want the written one", got["en"])
	}
	if got["ru"] != "С детьми" {
		t.Errorf("ru = %q, want the plain column", got["ru"])
	}
}

// null removes a language. Blank is the same request said differently: an empty
// translation reads as missing anyway (I18n.Resolve), so keeping the key would
// store a value nothing can use and still ship it in every payload.
func TestCollectionPatch_NullAndBlankRemoveALanguage(t *testing.T) {
	for name, patch := range map[string]domain.I18nPatch{
		"null":  {"en": nil},
		"blank": {"en": str("   ")},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeEditorRepo{detail: storedCollection("С детьми", "",
				domain.I18n{"kk": "Балалармен", "en": "With kids"}, nil)}
			if _, err := NewEditor(repo).UpdateCollection(context.Background(), superadmin(), uuid.New(),
				CollectionInput{Slug: "kids", Title: "С детьми", TitleI18n: patch}); err != nil {
				t.Fatalf("update: %v", err)
			}
			if _, ok := repo.gotWrite.TitleI18n["en"]; ok {
				t.Error("en survived a removal")
			}
			if repo.gotWrite.TitleI18n["kk"] != "Балалармен" {
				t.Error("kk was collateral damage")
			}
		})
	}
}

// No object at all touches no translation. An admin build that predates this
// protocol posts a collection without any `*_i18n` field, and it must keep
// editing titles without silently deleting every translation of them — which is
// precisely what the old full-replace write did.
func TestCollectionPatch_AbsentObjectLeavesTranslationsAlone(t *testing.T) {
	stored := domain.I18n{"kk": "Балалармен", "en": "With kids"}
	repo := &fakeEditorRepo{detail: storedCollection("С детьми", "Подзаголовок", stored,
		domain.I18n{"kk": "Тақырыпша"})}
	e := NewEditor(repo)

	if _, err := e.UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "С детьми и с собаками", Subtitle: "Подзаголовок",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.gotWrite.TitleI18n["kk"] != "Балалармен" || repo.gotWrite.TitleI18n["en"] != "With kids" {
		t.Errorf("title i18n = %v, want the stored translations untouched", repo.gotWrite.TitleI18n)
	}
	if repo.gotWrite.SubtitleI18n["kk"] != "Тақырыпша" {
		t.Errorf("subtitle i18n = %v, want the stored translation untouched", repo.gotWrite.SubtitleI18n)
	}
	// The ru entry still follows the column, because the column DID change.
	if repo.gotWrite.TitleI18n["ru"] != "С детьми и с собаками" {
		t.Errorf("ru = %q, want the new plain title", repo.gotWrite.TitleI18n["ru"])
	}
}

// The invariant the whole scheme rests on: i18n["ru"] IS the plain column. A
// `ru` key in the patch cannot contradict it, and cannot delete it — the
// Russian text is cleared by clearing the plain field, which for a title is not
// allowed at all.
func TestCollectionPatch_RussianAlwaysEqualsTheColumn(t *testing.T) {
	repo := &fakeEditorRepo{detail: storedCollection("Старый", "", domain.I18n{"ru": "Старый"}, nil)}
	e := NewEditor(repo)

	if _, err := e.UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "Новый",
		TitleI18n: domain.I18nPatch{"ru": str("Что-то третье")},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if repo.gotWrite.TitleI18n["ru"] != "Новый" {
		t.Errorf("ru = %q, want the plain column to win", repo.gotWrite.TitleI18n["ru"])
	}

	repo = &fakeEditorRepo{detail: storedCollection("Старый", "", domain.I18n{"ru": "Старый"}, nil)}
	_, err := NewEditor(repo).UpdateCollection(context.Background(), superadmin(), uuid.New(), CollectionInput{
		Slug: "kids", Title: "Новый", TitleI18n: domain.I18nPatch{"ru": nil},
	})
	assertValidation(t, err, "deleting ru")
	if repo.writes != 0 {
		t.Errorf("%d write(s) reached the repository", repo.writes)
	}
}

// An empty subtitle clears both the column and its ru entry: an empty
// translation reads as missing, so storing "" would be a value nothing can use.
func TestCollectionPatch_ClearingThePlainFieldClearsRussian(t *testing.T) {
	repo := &fakeEditorRepo{detail: storedCollection("С детьми", "Было", nil,
		domain.I18n{"ru": "Было", "kk": "Болды"})}

	if _, err := NewEditor(repo).UpdateCollection(context.Background(), superadmin(), uuid.New(),
		CollectionInput{Slug: "kids", Title: "С детьми", Subtitle: ""}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, ok := repo.gotWrite.SubtitleI18n["ru"]; ok {
		t.Error("ru survived an emptied subtitle")
	}
	if repo.gotWrite.SubtitleI18n["kk"] != "Болды" {
		t.Error("kk was dropped along with the Russian text")
	}
}

// A language we cannot render is refused rather than stored. kz/kk-KZ are the
// spellings that do occur in the wild and normalize to kk; ko and zh are the
// ones the old import left in the data, and this is the half of the policy that
// stops them coming back.
func TestPatch_LanguageValidation(t *testing.T) {
	cases := map[string]struct {
		patch   domain.I18nPatch
		wantErr bool
	}{
		"kk":                 {domain.I18nPatch{"kk": str("Балалармен")}, false},
		"kz alias":           {domain.I18nPatch{"kz": str("Балалармен")}, false},
		"kk-KZ region tag":   {domain.I18nPatch{"kk-KZ": str("Балалармен")}, false},
		"en":                 {domain.I18nPatch{"en": str("With kids")}, false},
		"ko":                 {domain.I18nPatch{"ko": str("아이들과")}, false},
		"zh":                 {domain.I18nPatch{"zh": str("带孩子")}, false},
		"zh-Hans script tag": {domain.I18nPatch{"zh-Hans": str("带孩子")}, false},
		"french":             {domain.I18nPatch{"fr": str("Avec enfants")}, true},
		"kk twice":           {domain.I18nPatch{"kk": str("а"), "kk-KZ": str("б")}, true},
		"zh twice":           {domain.I18nPatch{"zh": str("а"), "zh-Hant": str("б")}, true},
		"deleting a stray":   {domain.I18nPatch{"fr": nil}, true},
		"empty object":       {domain.I18nPatch{}, false},
		"unknown gibberish":  {domain.I18nPatch{"xx": str("x")}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeEditorRepo{detail: storedCollection("С детьми", "", nil, nil)}
			_, err := NewEditor(repo).UpdateCollection(context.Background(), superadmin(), uuid.New(),
				CollectionInput{Slug: "kids", Title: "С детьми", TitleI18n: tc.patch})
			switch {
			case tc.wantErr:
				assertValidation(t, err, name)
				if repo.writes != 0 {
					t.Errorf("%d write(s) reached the repository", repo.writes)
				}
			case err != nil:
				t.Fatalf("update: %v", err)
			}
		})
	}
}

// A locale the code does not recognize survives an edit untouched. The merge
// copies what it cannot name instead of dropping it: deleting an editor's text
// because our own import was sloppy is not a fix, and the validation above
// already guarantees nothing can ADD another one.
//
// ja/fr stand in for what ko/zh used to be here — rows the old import left
// behind that nothing could read. ko and zh are servable since 2026-09-02, so
// they are asserted alongside as ordinary translations.
func TestPatch_StrayLocalesSurviveAnEdit(t *testing.T) {
	repo := &fakeEditorRepo{detail: storedCollection("С детьми", "",
		domain.I18n{"ru": "С детьми", "ko": "아이들과", "ja": "子供と", "fr": "Avec enfants"}, nil)}

	if _, err := NewEditor(repo).UpdateCollection(context.Background(), superadmin(), uuid.New(),
		CollectionInput{
			Slug: "kids", Title: "С детьми",
			TitleI18n: domain.I18nPatch{"kk": str("Балалармен")},
		}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.gotWrite.TitleI18n
	if got["ja"] != "子供と" || got["fr"] != "Avec enfants" {
		t.Errorf("title i18n = %v, want the unrecognized locales preserved", got)
	}
	if got["ko"] != "아이들과" {
		t.Errorf("title i18n = %v, want the Korean translation kept too", got)
	}
	if got["kk"] != "Балалармен" {
		t.Errorf("kk = %q, want the written translation", got["kk"])
	}
}

// A rubric is the same protocol on a one-field object.
func TestCategoryPatch_MergesAndValidates(t *testing.T) {
	repo := &fakeEditorRepo{category: &domain.GuideCategory{
		ID: uuid.New(), Slug: "breakfasts", Title: "Завтраки",
		TitleI18n: domain.I18n{"ru": "Завтраки", "kk": "Таңғы ас", "en": "Breakfasts"},
	}}
	e := NewEditor(repo)

	if _, err := e.UpdateCategory(context.Background(), superadmin(), uuid.New(), CategoryInput{
		Slug: "breakfasts", Title: "Завтраки", IsActive: true,
		TitleI18n: domain.I18nPatch{"en": nil},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := repo.gotCategoryWrite.TitleI18n
	if _, ok := got["en"]; ok {
		t.Error("en survived a removal")
	}
	if got["kk"] != "Таңғы ас" || got["ru"] != "Завтраки" {
		t.Errorf("title i18n = %v, want kk kept and ru equal to the column", got)
	}

	repo = &fakeEditorRepo{category: &domain.GuideCategory{ID: uuid.New()}}
	_, err := NewEditor(repo).UpdateCategory(context.Background(), superadmin(), uuid.New(), CategoryInput{
		Slug: "breakfasts", Title: "Завтраки", TitleI18n: domain.I18nPatch{"fr": str("Petits déjeuners")},
	})
	assertValidation(t, err, "french rubric title")
	if repo.writes != 0 {
		t.Errorf("%d write(s) reached the repository", repo.writes)
	}
}

// The editor's line under a venue card is localized too, and it is the field
// most likely to be edited by two people at once — it is the one thing on the
// screen that is specific to this collection.
func TestVenueNotePatch_MergesAndValidates(t *testing.T) {
	rid := uuid.New()
	detail := storedCollection("С детьми", "", nil, nil)
	detail.Venues = []domain.GuideCollectionVenue{{
		RestaurantID: rid, Note: "Есть детская комната",
		NoteI18n: domain.I18n{"ru": "Есть детская комната", "kk": "Балалар бөлмесі бар", "ja": "キッズルーム"},
	}}
	repo := &fakeEditorRepo{detail: detail}

	if err := NewEditor(repo).SetVenueNote(context.Background(), superadmin(), uuid.New(), rid,
		"Есть детская комната", domain.I18nPatch{"en": str("Kids room")}); err != nil {
		t.Fatalf("set note: %v", err)
	}
	if repo.gotNoteI18n["kk"] != "Балалар бөлмесі бар" {
		t.Errorf("note i18n = %v, want kk kept", repo.gotNoteI18n)
	}
	if repo.gotNoteI18n["ja"] != "キッズルーム" {
		t.Errorf("note i18n = %v, want the unrecognized locale preserved", repo.gotNoteI18n)
	}
	if repo.gotNoteI18n["ru"] != "Есть детская комната" {
		t.Errorf("ru = %q, want the plain note", repo.gotNoteI18n["ru"])
	}

	// A venue that is not in this collection is ErrNotFound, not a silent
	// no-op: the cabinet must be told its screen is stale.
	err := NewEditor(&fakeEditorRepo{detail: storedCollection("С детьми", "", nil, nil)}).
		SetVenueNote(context.Background(), superadmin(), uuid.New(), uuid.New(), "x", nil)
	if err == nil {
		t.Fatal("note on a venue outside the collection was accepted")
	}
}

// An attach starts from nothing, so its note is a patch applied to an empty map
// — and the ru invariant is established at the same moment the row is created.
func TestAttachVenue_NoteStartsFromAnEmptyMap(t *testing.T) {
	repo := &fakeEditorRepo{}
	if err := NewEditor(repo).AttachVenue(context.Background(), superadmin(), uuid.New(), AttachVenueInput{
		RestaurantID: uuid.New(), Note: "  Есть веранда  ",
		NoteI18n: domain.I18nPatch{"kk": str("Веранда бар"), "en": nil},
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if repo.gotNote != "Есть веранда" {
		t.Errorf("note = %q, want it trimmed", repo.gotNote)
	}
	if repo.gotNoteI18n["ru"] != "Есть веранда" || repo.gotNoteI18n["kk"] != "Веранда бар" {
		t.Errorf("note i18n = %v", repo.gotNoteI18n)
	}
	if _, ok := repo.gotNoteI18n["en"]; ok {
		t.Error("a removal on a fresh row invented an en entry")
	}

	repo = &fakeEditorRepo{}
	err := NewEditor(repo).AttachVenue(context.Background(), superadmin(), uuid.New(), AttachVenueInput{
		RestaurantID: uuid.New(), NoteI18n: domain.I18nPatch{"fr": str("Il y a une terrasse")},
	})
	assertValidation(t, err, "french note")
	if repo.writes != 0 {
		t.Errorf("%d write(s) reached the repository", repo.writes)
	}
}

func assertValidation(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted, want ErrValidation", what)
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("%s: err = %v, want ErrValidation", what, err)
	}
}

// «Гастропрогулки» take the same protocol. A route carries three localized
// fields and its stops carry three more, and the walk-length label is the one
// most often written per-language ("1 день" / "1 күн"), so losing it to a
// colleague's save is the same bug in a different table.
func TestRoutePatch_MergesKeepsAndValidates(t *testing.T) {
	repo := &fakeRouteRepo{detail: &domain.GastroRouteAdminDetail{
		GastroRoute: domain.GastroRoute{
			ID: uuid.New(), Slug: "classic-almaty", Title: "Классический тур",
			TitleI18n:         domain.I18n{"ru": "Классический тур", "kk": "Классикалық тур"},
			DurationLabel:     "1 день",
			DurationLabelI18n: domain.I18n{"ru": "1 день", "kk": "1 күн", "ja": "1日"},
			DescriptionI18n:   domain.I18n{"en": "A classic day"},
		},
	}}
	e := NewRouteEditor(repo)

	if _, err := e.UpdateRoute(context.Background(), superadmin(), uuid.New(), RouteInput{
		Slug: "classic-almaty", Title: "Классический тур", DurationLabel: "1 день",
		TitleI18n: domain.I18nPatch{"en": str("The classic tour")},
	}); err != nil {
		t.Fatalf("update route: %v", err)
	}
	got := repo.gotRoute
	if got.TitleI18n["kk"] != "Классикалық тур" || got.TitleI18n["en"] != "The classic tour" {
		t.Errorf("title i18n = %v", got.TitleI18n)
	}
	if got.TitleI18n["ru"] != "Классический тур" {
		t.Errorf("ru = %q, want the plain column", got.TitleI18n["ru"])
	}
	// Untouched objects stay untouched, unrecognized locales included.
	if got.DurationLabelI18n["kk"] != "1 күн" || got.DurationLabelI18n["ja"] != "1日" {
		t.Errorf("duration i18n = %v, want it left alone", got.DurationLabelI18n)
	}
	if got.DescriptionI18n["en"] != "A classic day" {
		t.Errorf("description i18n = %v, want it left alone", got.DescriptionI18n)
	}

	repo.writes = 0
	_, err := e.UpdateRoute(context.Background(), superadmin(), uuid.New(), RouteInput{
		Slug: "classic-almaty", Title: "Классический тур",
		DurationLabelI18n: domain.I18nPatch{"fr": str("Une journée")},
	})
	assertValidation(t, err, "french duration label")
	if repo.writes != 0 {
		t.Errorf("%d write(s) reached the repository", repo.writes)
	}
}

// A stop's own text is merged against the stop, not against the route: the
// editor screen edits one stop at a time, and the stop is what the patch is
// partial against.
func TestRoutePointPatch_MergesAgainstTheStop(t *testing.T) {
	pointID := uuid.New()
	repo := &fakeRouteRepo{detail: &domain.GastroRouteAdminDetail{
		GastroRoute: domain.GastroRoute{ID: uuid.New(), Slug: "classic-almaty", Title: "Тур"},
		Points: []domain.GuideRoutePoint{{
			ID: pointID, Kind: domain.GuideRoutePointPlace, Title: "Утро: Панфиловцев",
			TitleI18n:   domain.I18n{"kk": "Таң: Панфилов", "en": "Morning: Panfilov"},
			Address:     "парк 28 панфиловцев",
			AddressI18n: domain.I18n{"ko": "판필로프 공원"},
		}},
	}}
	e := NewRouteEditor(repo)

	if _, err := e.UpdatePoint(context.Background(), superadmin(), uuid.New(), pointID, PointInput{
		Kind: domain.GuideRoutePointPlace, Title: "Утро: Панфиловцев",
		Address:   "парк 28 панфиловцев",
		TitleI18n: domain.I18nPatch{"en": nil},
	}); err != nil {
		t.Fatalf("update point: %v", err)
	}
	got := repo.gotPoint
	if _, ok := got.TitleI18n["en"]; ok {
		t.Error("en survived a removal")
	}
	if got.TitleI18n["kk"] != "Таң: Панфилов" {
		t.Errorf("title i18n = %v, want kk kept", got.TitleI18n)
	}
	if got.TitleI18n["ru"] != "Утро: Панфиловцев" {
		t.Errorf("ru = %q, want the plain column", got.TitleI18n["ru"])
	}
	if got.AddressI18n["ko"] != "판필로프 공원" {
		t.Errorf("address i18n = %v, want the untouched field left alone", got.AddressI18n)
	}

	// A stop that belongs to another route is ErrNotFound, not a write against
	// the wrong row.
	if _, err := e.UpdatePoint(context.Background(), superadmin(), uuid.New(), uuid.New(), PointInput{
		Kind: domain.GuideRoutePointPlace, Title: "Точка",
	}); err == nil {
		t.Fatal("an unknown stop was accepted")
	}
}

// A new stop starts from an empty map, exactly like a new venue note.
func TestRoutePointPatch_AddStartsFromAnEmptyMap(t *testing.T) {
	repo := &fakeRouteRepo{}
	if _, err := NewRouteEditor(repo).AddPoint(context.Background(), superadmin(), uuid.New(), PointInput{
		Kind: domain.GuideRoutePointPlace, Title: "Точка",
		TitleI18n: domain.I18nPatch{"kz": str("Нүкте")},
	}); err != nil {
		t.Fatalf("add point: %v", err)
	}
	// The kz alias is normalized to kk on the way in — a store build sending
	// the old spelling must not create a language nothing reads.
	if repo.gotPoint.TitleI18n["kk"] != "Нүкте" {
		t.Errorf("title i18n = %v, want the kz alias stored as kk", repo.gotPoint.TitleI18n)
	}
	if repo.gotPoint.TitleI18n["ru"] != "Точка" {
		t.Errorf("ru = %q, want the plain column", repo.gotPoint.TitleI18n["ru"])
	}
}

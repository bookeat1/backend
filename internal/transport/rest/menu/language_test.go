package menu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// menuFixture: one dish WITH a Kazakh translation and one WITHOUT. Every
// language case below is decided by these two rows.
func menuFixture(rid uuid.UUID) []domain.MenuItem {
	kk := "Бешбармақ"
	return []domain.MenuItem{
		{
			ID: uuid.New(), RestaurantID: rid, Name: "Бешбармак",
			NameI18n:        domain.I18n{"ru": "Бешбармак", "kk": kk},
			Description:     "Мясо с тестом",
			DescriptionI18n: domain.I18n{"ru": "Мясо с тестом", "kk": "Ет пен қамыр"},
			Category:        ptrs("Горячее"), CategoryI18n: domain.I18n{"ru": "Горячее", "kk": "Ыстық"},
			Price: "3200.00", IsAvailable: true,
		},
		{
			ID: uuid.New(), RestaurantID: rid, Name: "Плов",
			NameI18n:    domain.I18n{"ru": "Плов"},
			Description: "Рис с мясом",
			Category:    ptrs("Горячее"),
			Price:       "2500.00", IsAvailable: true,
		},
	}
}

func ptrs(s string) *string { return &s }

type dishJSON struct {
	Name        string            `json:"name"`
	NameI18n    map[string]string `json:"name_i18n"`
	Description string            `json:"description"`
	Category    *string           `json:"category"`
}

func getMenu(t *testing.T, f *fakeFacade, rid uuid.UUID, query, acceptLanguage string) []dishJSON {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+rid.String()+"/menu"+query, nil)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	w := httptest.NewRecorder()
	router(f).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Data []dishJSON `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return body.Data
}

// REGRESSION. `?lang=kk` used to answer `{"data":[]}` on production: the
// listing selected ROWS by menu_items.language, and no row was labelled 'kk'.
// The menu a guest sees must never depend on the language they read it in.
func TestMenuIsNeverEmptyBecauseOfTheRequestedLanguage(t *testing.T) {
	rid := uuid.New()
	for _, q := range []string{"", "?lang=ru", "?lang=kk", "?lang=kz", "?lang=en", "?lang=fr", "?lang=", "?lang=%%"} {
		f := &fakeFacade{items: menuFixture(rid)}
		got := getMenu(t, f, rid, q, "")
		if len(got) != 2 {
			t.Fatalf("query %q: got %d dishes, want 2", q, len(got))
		}
		if f.listRID != rid {
			t.Fatalf("query %q: usecase got rid %s", q, f.listRID)
		}
	}
}

// A dish WITH a translation is served in Kazakh; a dish WITHOUT one falls back
// to Russian instead of disappearing. Both are in the same response.
func TestMenuInKazakhTranslatesWhatItHasAndFallsBackForTheRest(t *testing.T) {
	rid := uuid.New()
	for _, q := range []string{"?lang=kk", "?lang=kz", "?lang=KK-KZ"} {
		f := &fakeFacade{items: menuFixture(rid)}
		got := getMenu(t, f, rid, q, "")
		if len(got) != 2 {
			t.Fatalf("%s: got %d dishes, want 2", q, len(got))
		}
		if got[0].Name != "Бешбармақ" || got[0].Description != "Ет пен қамыр" || *got[0].Category != "Ыстық" {
			t.Fatalf("%s: translated dish not localized: %+v", q, got[0])
		}
		if got[1].Name != "Плов" || got[1].Description != "Рис с мясом" {
			t.Fatalf("%s: untranslated dish must fall back to Russian: %+v", q, got[1])
		}
		// The raw maps stay in the payload: the installed mobile build resolves
		// the menu itself and would lose translations if they were dropped.
		if got[0].NameI18n["kk"] != "Бешбармақ" || got[0].NameI18n["ru"] != "Бешбармак" {
			t.Fatalf("%s: i18n map must be preserved: %+v", q, got[0].NameI18n)
		}
	}
}

// Accept-Language is honoured, like on every other public resource — the menu
// used to read only ?lang= and ignore the header.
func TestMenuHonoursAcceptLanguage(t *testing.T) {
	rid := uuid.New()
	f := &fakeFacade{items: menuFixture(rid)}
	got := getMenu(t, f, rid, "", "kk-KZ,ru;q=0.8")
	if len(got) != 2 || got[0].Name != "Бешбармақ" {
		t.Fatalf("Accept-Language ignored: %+v", got)
	}

	// The query parameter still wins over the header.
	f = &fakeFacade{items: menuFixture(rid)}
	got = getMenu(t, f, rid, "?lang=ru", "kk-KZ")
	if got[0].Name != "Бешбармак" {
		t.Fatalf("?lang must win over Accept-Language: %+v", got[0])
	}
}

// An unknown language is answered in Russian with the full menu, not with an
// empty list and not with a 4xx: a stale client must never be able to blank the
// screen by asking for a language we do not have.
func TestMenuInAnUnknownLanguageIsRussianAndComplete(t *testing.T) {
	rid := uuid.New()
	f := &fakeFacade{items: menuFixture(rid)}
	got := getMenu(t, f, rid, "?lang=zz", "")
	if len(got) != 2 || got[0].Name != "Бешбармак" || got[1].Name != "Плов" {
		t.Fatalf("unknown language must serve the Russian menu: %+v", got)
	}
}

// No language signal at all must leave the payload byte-identical to what
// clients that never ask for a language have always received.
func TestMenuWithoutALanguageSignalKeepsTheBaseText(t *testing.T) {
	rid := uuid.New()
	f := &fakeFacade{items: menuFixture(rid)}
	got := getMenu(t, f, rid, "", "")
	if got[0].Name != "Бешбармак" || got[0].Description != "Мясо с тестом" || *got[0].Category != "Горячее" {
		t.Fatalf("base text changed for a client that asked for nothing: %+v", got[0])
	}
}

// The same dish must never be served twice, whatever the language: the venue
// listing is what a per-language row copy would duplicate.
func TestMenuNeverRepeatsADish(t *testing.T) {
	rid := uuid.New()
	f := &fakeFacade{items: menuFixture(rid)}
	for _, q := range []string{"", "?lang=kk", "?lang=kz", "?lang=en"} {
		seen := map[string]bool{}
		for _, d := range getMenu(t, f, rid, q, "") {
			if seen[d.NameI18n["ru"]] {
				t.Fatalf("query %q: dish %q served twice", q, d.Name)
			}
			seen[d.NameI18n["ru"]] = true
		}
	}
}

package stories

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func translatedStory() domain.Story {
	caption := "Летнее меню"
	return domain.Story{
		ID:          uuid.New(),
		ImageURL:    "https://cdn/a.jpg",
		Caption:     &caption,
		CaptionI18n: domain.I18n{"ru": "Летнее меню", "kk": "Жазғы мәзір"},
		SortOrder:   0,
		IsActive:    true,
	}
}

// doLang issues a public read in a given language (the ?lang= form, the same
// signal reqlocale reads off Accept-Language).
func doLang(r *gin.Engine, path, lang string) *httptest.ResponseRecorder {
	if lang != "" {
		path += "?lang=" + lang
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func captionOf(t *testing.T, w *httptest.ResponseRecorder) any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("want one card, got %d", len(env.Data))
	}
	return env.Data[0]["caption"]
}

// The guest read localizes the caption and falls back to the Russian column
// when the language has no translation — the same contract as every other
// public content route.
func TestPublicListLocalizesCaption(t *testing.T) {
	r := newRouter(&fakeFacade{rv: []domain.Story{translatedStory()}})
	path := "/api/v1/restaurants/" + uuid.New().String() + "/stories"

	if got := captionOf(t, doLang(r, path, "kk")); got != "Жазғы мәзір" {
		t.Errorf("kk caption = %v, want the Kazakh translation", got)
	}
	if got := captionOf(t, doLang(r, path, "en")); got != "Летнее меню" {
		t.Errorf("en caption = %v, want the Russian fallback", got)
	}
	// The historical alias old store builds still send.
	if got := captionOf(t, doLang(r, path, "kz")); got != "Жазғы мәзір" {
		t.Errorf("kz caption = %v, want the same as kk", got)
	}
	// No language asked for: the stored column, byte for byte.
	if got := captionOf(t, doLang(r, path, "")); got != "Летнее меню" {
		t.Errorf("caption = %v, want the stored column", got)
	}
}

// A card without a caption stays without one in every language: nil is not an
// empty string, and the client must not draw an empty label.
func TestPublicListKeepsACaptionlessCardCaptionless(t *testing.T) {
	s := translatedStory()
	s.Caption, s.CaptionI18n = nil, nil
	r := newRouter(&fakeFacade{rv: []domain.Story{s}})

	if got := captionOf(t, doLang(r, "/api/v1/restaurants/"+uuid.New().String()+"/stories", "kk")); got != nil {
		t.Errorf("caption = %v, want it absent", got)
	}
}

// The guest never receives the raw map; the cabinet always does.
func TestGuestShapeHasNoRawMapAndCabinetDoes(t *testing.T) {
	s := translatedStory()

	guest, err := json.Marshal(storyToResponse(&s, domain.LocaleKK))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var g map[string]any
	if err := json.Unmarshal(guest, &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := g["caption_i18n"]; ok {
		t.Error("the guest shape must not carry caption_i18n")
	}

	cabinet, err := json.Marshal(adminStoryToResponse(&s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(cabinet, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := c["caption_i18n"].(map[string]any)
	if !ok || m["kk"] != "Жазғы мәзір" {
		t.Fatalf("caption_i18n = %v, want the raw map for the editor", c["caption_i18n"])
	}
	if c["caption"] != "Летнее меню" {
		t.Errorf("caption = %v, want the stored column", c["caption"])
	}
}

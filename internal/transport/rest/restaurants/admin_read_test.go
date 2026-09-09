package restaurants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// adminReadPayload is the slice of GET /admin/restaurants/:id the cabinet's
// settings screen actually reads: the two blocks that went blank when a venue
// was deactivated, plus the name/name_i18n pair the rename bug lived in.
type adminReadPayload struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	NameI18n   map[string]string `json:"name_i18n"`
	IsActive   bool              `json:"is_active"`
	PriceRange *struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"price_range"`
	SocialLinks []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"social_links"`
}

// hiddenVenue is a DEACTIVATED venue that still has everything the settings
// screen needs: an average-check range, social links, and a ru translation of
// its name that differs from the stored column.
func hiddenVenue(id uuid.UUID) *domain.RestaurantAggregate {
	min, max := 5000, 15000
	return &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{
			ID:       id,
			Name:     "Новое имя",
			NameI18n: domain.I18n{"ru": "Старое имя", "kk": "Ескі атауы"},
			IsActive: false,
			PriceMin: &min, PriceMax: &max,
		},
		SocialLinks: []domain.SocialLink{{ID: uuid.New(), Type: "instagram", URL: "https://x"}},
	}
}

func newScopedRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(f, nil, nil)
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	// Registered on the SAME engine as the superadmin catalog listing on
	// purpose: /admin/restaurants and /admin/restaurants/:id are siblings in
	// gin's tree, and a conflict there would only ever show up as a panic at
	// boot.
	h.RegisterAdminGlobal(api)
	h.RegisterRestaurantScoped(api)
	return r
}

func adminRead(t *testing.T, r *gin.Engine, path string) adminReadPayload {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, body %s", path, w.Code, w.Body.String())
	}
	var env struct {
		Data adminReadPayload `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestAdminGetServesDeactivatedVenueWithSettingsFields is the regression: the
// panel's «Средний чек» and «Соцсети» cards prefill from the venue detail, and
// a hidden venue answered 404 on the only route that carried both.
func TestAdminGetServesDeactivatedVenueWithSettingsFields(t *testing.T) {
	id := uuid.New()
	r := newScopedRouter(&fakeFacade{agg: hiddenVenue(id)})

	got := adminRead(t, r, "/api/v1/admin/restaurants/"+id.String())
	if got.ID != id.String() {
		t.Fatalf("id = %q, want %q", got.ID, id)
	}
	if got.IsActive {
		t.Error("is_active = true, want the hidden venue to report itself hidden")
	}
	if got.PriceRange == nil || got.PriceRange.Min != 5000 || got.PriceRange.Max != 15000 {
		t.Errorf("price_range = %+v, want {5000 15000}", got.PriceRange)
	}
	if len(got.SocialLinks) != 1 || got.SocialLinks[0].Type != "instagram" {
		t.Errorf("social_links = %+v, want the venue's one link", got.SocialLinks)
	}
}

// TestAdminGetIsNotLocalized proves the cabinet is shown the COLUMN it edits,
// not the translation the public route would resolve for Accept-Language: ru.
// Being handed the translation is exactly how a rename reverted itself.
func TestAdminGetIsNotLocalized(t *testing.T) {
	id := uuid.New()
	r := newScopedRouter(&fakeFacade{agg: hiddenVenue(id)})

	got := adminRead(t, r, "/api/v1/admin/restaurants/"+id.String()+"?lang=ru")
	if got.Name != "Новое имя" {
		t.Errorf("name = %q, want the stored column", got.Name)
	}
	if got.NameI18n["ru"] != "Старое имя" || got.NameI18n["kk"] != "Ескі атауы" {
		t.Errorf("name_i18n = %v, want the stored map served as-is", got.NameI18n)
	}
}

// TestPublicGetStillHidesDeactivatedVenue guards the other side of the change:
// the new cabinet read must not have relaxed the public catalog.
func TestPublicGetStillHidesDeactivatedVenue(t *testing.T) {
	id := uuid.New()
	r := newScopedRouter(&fakeFacade{agg: hiddenVenue(id)})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+id.String(), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("public GET of a hidden venue = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

func TestAdminGetRejectsMalformedID(t *testing.T) {
	r := newScopedRouter(&fakeFacade{agg: hiddenVenue(uuid.New())})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/restaurants/not-a-uuid", nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("GET with a bad id = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
}

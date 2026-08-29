package menu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/menu"
)

// fakeFacade records what the transport asked for. Only the methods this file
// exercises do anything; the rest exist to satisfy the interface.
type fakeFacade struct {
	highlights    []domain.MenuItem
	highlightsRID uuid.UUID
	highlightsLim int

	setRID, setItem uuid.UUID
	setOn           bool
	setErr          error

	replaced []uuid.UUID

	// items backs ListByRestaurant; listRID records what the transport asked
	// for.
	items   []domain.MenuItem
	listRID uuid.UUID
}

func (f *fakeFacade) ListByRestaurant(_ context.Context, rid uuid.UUID) ([]domain.MenuItem, error) {
	f.listRID = rid
	return f.items, nil
}
func (f *fakeFacade) Get(context.Context, uuid.UUID) (*domain.MenuItem, error) { return nil, nil }
func (f *fakeFacade) Categories(context.Context) ([]domain.MenuCategory, error) {
	return nil, nil
}
func (f *fakeFacade) Create(context.Context, uuid.UUID, uc.ItemInput) (*domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeFacade) Update(context.Context, uuid.UUID, uuid.UUID, uc.ItemInput) (*domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeFacade) Delete(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeFacade) SetAvailable(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return nil
}
func (f *fakeFacade) SetAvailableBulk(context.Context, uuid.UUID, []uuid.UUID, bool) (int, error) {
	return 0, nil
}
func (f *fakeFacade) SetFeatured(context.Context, uuid.UUID, uuid.UUID, bool) error { return nil }
func (f *fakeFacade) ListFeatured(context.Context, domain.City, int) ([]domain.FeaturedMenuItem, error) {
	return nil, nil
}
func (f *fakeFacade) ListHighlights(_ context.Context, rid uuid.UUID, limit int) ([]domain.MenuItem, error) {
	f.highlightsRID, f.highlightsLim = rid, limit
	return f.highlights, nil
}
func (f *fakeFacade) SetTopPick(_ context.Context, rid, itemID uuid.UUID, on bool) error {
	f.setRID, f.setItem, f.setOn = rid, itemID, on
	return f.setErr
}
func (f *fakeFacade) ReplaceTopPicks(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	f.replaced = ids
	return nil
}
func (f *fakeFacade) ListTopPicks(context.Context, uuid.UUID) ([]domain.MenuItem, error) {
	return f.highlights, nil
}
func (f *fakeFacade) CreateCategory(context.Context, uc.CategoryInput) (*domain.MenuCategory, error) {
	return nil, nil
}
func (f *fakeFacade) UpdateCategory(context.Context, uuid.UUID, uc.CategoryInput) (*domain.MenuCategory, error) {
	return nil, nil
}
func (f *fakeFacade) DeleteCategory(context.Context, uuid.UUID) error { return nil }

// router mounts ALL three groups on one engine, exactly as bootstrap does. Its
// first job is to prove the new routes do not make gin panic on a wildcard
// conflict — a router panic is a boot failure, not a 500.
func router(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	h := NewHandler(f)
	h.RegisterPublic(api)
	h.RegisterScoped(api)
	h.RegisterAdmin(api.Group("/admin"))
	return r
}

func do(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHighlightsEndpointReturnsTheRailWithNumericPrices(t *testing.T) {
	rid := uuid.New()
	slot := 1
	f := &fakeFacade{highlights: []domain.MenuItem{
		{ID: uuid.New(), RestaurantID: rid, Name: "Отмеченное", Price: "8990.00", IsAvailable: true, TopPickPosition: &slot},
		{ID: uuid.New(), RestaurantID: rid, Name: "Выведенное", Price: "1200.50", IsAvailable: true},
	}}
	w := do(t, router(f), http.MethodGet, "/api/v1/restaurants/"+rid.String()+"/menu-highlights?limit=5", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if f.highlightsRID != rid || f.highlightsLim != 5 {
		t.Fatalf("usecase got rid=%s limit=%d", f.highlightsRID, f.highlightsLim)
	}
	var body struct {
		Data []struct {
			Name       string `json:"name"`
			Price      string `json:"price"`
			PriceMinor *int64 `json:"price_minor"`
			IsTopPick  bool   `json:"is_top_pick"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("want 2 cards, got %s", w.Body.String())
	}
	// Order is the usecase's decision and the transport must not re-sort it.
	if body.Data[0].Name != "Отмеченное" || !body.Data[0].IsTopPick {
		t.Fatalf("the venue's own pick must lead: %s", w.Body.String())
	}
	if body.Data[1].IsTopPick {
		t.Fatalf("the derived filler must not claim to be a pick: %s", w.Body.String())
	}
	for _, card := range body.Data {
		if card.PriceMinor == nil {
			t.Fatalf("card %q has no price_minor: %s", card.Name, w.Body.String())
		}
	}
	if *body.Data[0].PriceMinor != 899000 || *body.Data[1].PriceMinor != 120050 {
		t.Fatalf("wrong minor units: %s", w.Body.String())
	}
}

func TestSetTopPickEndpointPassesTheFlagThrough(t *testing.T) {
	rid, item := uuid.New(), uuid.New()
	f := &fakeFacade{}
	path := "/api/v1/restaurants/" + rid.String() + "/menu-items/" + item.String() + "/top-pick"
	if w := do(t, router(f), http.MethodPatch, path, `{"is_top_pick":true}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if f.setRID != rid || f.setItem != item || !f.setOn {
		t.Fatalf("usecase got rid=%s item=%s on=%v", f.setRID, f.setItem, f.setOn)
	}
	if w := do(t, router(f), http.MethodPatch, path, `{"is_top_pick":false}`); w.Code != http.StatusOK {
		t.Fatalf("unmark status %d: %s", w.Code, w.Body.String())
	}
}

// A full rail must reach the panel as its own machine-readable code, so it can
// say "снимите одно из отмеченных" instead of a generic validation error.
func TestSetTopPickEndpointSurfacesTheLimitCode(t *testing.T) {
	rid, item := uuid.New(), uuid.New()
	f := &fakeFacade{setErr: domain.WithCode(domain.CodeMenuTopPicksLimit, domain.ErrValidation)}
	path := "/api/v1/restaurants/" + rid.String() + "/menu-items/" + item.String() + "/top-pick"
	w := do(t, router(f), http.MethodPatch, path, `{"is_top_pick":true}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != string(domain.CodeMenuTopPicksLimit) {
		t.Fatalf("want code %q, got %q", domain.CodeMenuTopPicksLimit, body.Code)
	}
}

func TestReplaceTopPicksEndpointKeepsTheGivenOrderAndRejectsGarbageIDs(t *testing.T) {
	rid := uuid.New()
	a, b := uuid.New(), uuid.New()
	f := &fakeFacade{}
	path := "/api/v1/restaurants/" + rid.String() + "/menu-highlights"
	w := do(t, router(f), http.MethodPut, path, `{"item_ids":["`+a.String()+`","`+b.String()+`"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(f.replaced) != 2 || f.replaced[0] != a || f.replaced[1] != b {
		t.Fatalf("order was not preserved: %v", f.replaced)
	}

	f.replaced = nil
	w = do(t, router(f), http.MethodPut, path, `{"item_ids":["не-uuid"]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("garbage id: status %d: %s", w.Code, w.Body.String())
	}
	if f.replaced != nil {
		t.Fatal("a malformed id must not reach the usecase")
	}
}

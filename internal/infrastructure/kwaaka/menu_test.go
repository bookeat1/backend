package kwaaka

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"backend-core/internal/domain"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(srv.Client(), Config{
		BaseURL:     srv.URL,
		PathPrefix:  "/v1/table-booking",
		Token:       "test-token",
		Service:     "bookeat",
		Timeout:     time.Second,
		MaxAttempts: 2,
	})
}

// realMenuFixture is a trimmed version of the ACTUAL 200 response captured
// from the Kwaaka test store 2026-09-21 (GET
// /restaurants/{id}/menu?service=bookeat) — see doc.go. It keeps the field
// shape exactly as Kwaaka sent it (empty language_code, empty currency_code)
// so the mapper is tested against reality, not an idealised payload.
const realMenuFixture = `{
  "id": "menu-1", "name": "Меню Kwaaka", "is_active": true,
  "sections": [
    {"id": "sec-1", "name": "Меню Kwaaka", "section_order": 1, "is_available": true, "is_deleted": false},
    {"id": "sec-2", "name": "Стопнутая секция", "section_order": 2, "is_available": false, "is_deleted": false}
  ],
  "products": [
    {
      "id": "prod-1", "section": "sec-1",
      "name": [{"language_code": "", "value": "Наггетсы 9 шт"}],
      "description": [{"language_code": "", "value": ""}],
      "images": [], "is_available": true,
      "price": [{"value": 2890, "currency_code": ""}]
    },
    {
      "id": "prod-2", "section": "sec-1",
      "name": [{"language_code": "", "value": "Стоп-лист блюдо"}],
      "description": [], "images": [], "is_available": false,
      "price": [{"value": 1500, "currency_code": ""}]
    },
    {
      "id": "prod-3", "section": "sec-2",
      "name": [{"language_code": "", "value": "Блюдо скрытой секции"}],
      "description": [], "images": [], "is_available": true,
      "price": [{"value": 990, "currency_code": ""}]
    }
  ],
  "combos": []
}`

func TestFetchMenu_MapsRealShape(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realMenuFixture))
	}))
	defer srv.Close()

	src := NewMenuSource(testClient(t, srv))
	menu, err := src.FetchMenu(context.Background(), "kwaaka-restaurant-1")
	if err != nil {
		t.Fatalf("FetchMenu: %v", err)
	}

	if want := "/v1/table-booking/restaurants/kwaaka-restaurant-1/menu?service=bookeat"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// Authorization carries the token AS-IS, no "Bearer " prefix (apiKey
	// scheme, spec components.securitySchemes.ApiKeyAuth).
	if gotAuth != "test-token" {
		t.Errorf("Authorization header = %q, want bare token", gotAuth)
	}

	if len(menu.Products) != 3 {
		t.Fatalf("got %d products, want 3", len(menu.Products))
	}

	byID := map[string]domain.KwaakaMenuProduct{}
	for _, p := range menu.Products {
		byID[p.ExternalID] = p
	}

	p1 := byID["prod-1"]
	if p1.Name != "Наггетсы 9 шт" || p1.Price != "2890.00" || !p1.IsAvailable || p1.Category != "Меню Kwaaka" {
		t.Errorf("prod-1 mapped wrong: %+v", p1)
	}
	if !domain.ValidPrice(p1.Price) {
		t.Errorf("prod-1 price %q is not a valid domain price string", p1.Price)
	}

	// Explicit product-level stop list.
	if p2 := byID["prod-2"]; p2.IsAvailable {
		t.Errorf("prod-2 should be unavailable (product-level is_available=false), got %+v", p2)
	}

	// A dish in a hidden section must read unavailable even though its OWN
	// is_available flag says true — see mapMenu's doc comment.
	if p3 := byID["prod-3"]; p3.IsAvailable {
		t.Errorf("prod-3 should be unavailable (section is_available=false), got %+v", p3)
	}
}

func TestFetchMenu_SkipsProductsMissingIDOrName(t *testing.T) {
	body := `{"sections":[],"products":[
		{"id":"","name":[{"value":"no id"}],"is_available":true,"price":[]},
		{"id":"prod-x","name":[],"is_available":true,"price":[]},
		{"id":"prod-ok","name":[{"value":"ok"}],"is_available":true,"price":[]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	menu, err := NewMenuSource(testClient(t, srv)).FetchMenu(context.Background(), "r1")
	if err != nil {
		t.Fatalf("FetchMenu: %v", err)
	}
	if len(menu.Products) != 1 || menu.Products[0].ExternalID != "prod-ok" {
		t.Fatalf("got %+v, want only prod-ok", menu.Products)
	}
	// A missing price must never upsert as a garbage/negative amount.
	if menu.Products[0].Price != "0.00" {
		t.Errorf("price = %q, want 0.00 for a product with no price entries", menu.Products[0].Price)
	}
}

// TestFetchMenu_NoPriceForcesUnavailable asserts bug 2026-09-22: Kwaaka
// omitting/zeroing a product's price must never let it publish as a free
// dish, even when Kwaaka's own is_available (and the section's) say true.
func TestFetchMenu_NoPriceForcesUnavailable(t *testing.T) {
	body := `{"sections":[{"id":"sec-1","name":"Sec","is_available":true,"is_deleted":false}],"products":[
		{"id":"no-price","section":"sec-1","name":[{"value":"No price"}],"is_available":true,"price":[]},
		{"id":"zero-price","section":"sec-1","name":[{"value":"Zero price"}],"is_available":true,"price":[{"value":0}]},
		{"id":"negative-price","section":"sec-1","name":[{"value":"Negative price"}],"is_available":true,"price":[{"value":-5}]},
		{"id":"has-price","section":"sec-1","name":[{"value":"Has price"}],"is_available":true,"price":[{"value":1500}]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	menu, err := NewMenuSource(testClient(t, srv)).FetchMenu(context.Background(), "r1")
	if err != nil {
		t.Fatalf("FetchMenu: %v", err)
	}
	byID := map[string]domain.KwaakaMenuProduct{}
	for _, p := range menu.Products {
		byID[p.ExternalID] = p
	}

	for _, id := range []string{"no-price", "zero-price", "negative-price"} {
		p := byID[id]
		if p.IsAvailable {
			t.Errorf("%s: IsAvailable = true, want false (no positive price, even though Kwaaka sent is_available=true)", id)
		}
		if !domain.ValidPrice(p.Price) {
			t.Errorf("%s: price %q must still be a well-formed price string", id, p.Price)
		}
	}
	if p := byID["has-price"]; !p.IsAvailable {
		t.Error("has-price: a product with a real positive price must keep Kwaaka's own is_available value")
	}
}

func TestFetchMenu_EmptyRestaurantID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call kwaaka with an empty restaurant id")
	}))
	defer srv.Close()

	_, err := NewMenuSource(testClient(t, srv)).FetchMenu(context.Background(), "  ")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestFetchMenu_ServerErrorRetriesThenFails(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := testClient(t, srv)
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil } // no real waiting in tests

	_, err := NewMenuSource(c).FetchMenu(context.Background(), "r1")
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls != 2 { // MaxAttempts: 2
		t.Fatalf("calls = %d, want 2 (retried once on 5xx)", calls)
	}
}

func TestFetchMenu_BadRequestDoesNotRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"service query param is required"}`))
	}))
	defer srv.Close()

	_, err := NewMenuSource(testClient(t, srv)).FetchMenu(context.Background(), "r1")
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (a 4xx is not retried)", calls)
	}
}

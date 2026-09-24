package restaurants

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
)

// authedPatch builds a PATCH /restaurants/:id request carrying the given
// AuthUser on the context, the same way middleware.Auth would have put it
// there in production — this package's handlers read the actor off the
// context directly rather than through a role-gate middleware, so a unit test
// has to seed it the same way.
func authedPatch(t *testing.T, path string, body map[string]any, au *middleware.AuthUser) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if au != nil {
		req = req.WithContext(middleware.WithAuthUser(req.Context(), *au))
	}
	return req
}

// TestUpdateStripsKwaakaRestaurantIDForNonAdmin proves a venue's own
// manager/owner cannot repoint their POS integration through PATCH
// /restaurants/:id, exactly like they cannot self-promote is_premium.
func TestUpdateStripsKwaakaRestaurantIDForNonAdmin(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	r := newScopedRouter(f)

	manager := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleRestaurant)}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedPatch(t, "/api/v1/restaurants/"+id.String(),
		map[string]any{"kwaaka_restaurant_id": "kw-99"}, &manager))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotUpdate.KwaakaRestaurantID != nil {
		t.Errorf("KwaakaRestaurantID reached the facade as %v, want nil (stripped for a non-admin caller)",
			*f.gotUpdate.KwaakaRestaurantID)
	}
}

// TestUpdateKeepsKwaakaRestaurantIDForAdmin proves the superadmin path this
// field exists for actually works.
func TestUpdateKeepsKwaakaRestaurantIDForAdmin(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	r := newScopedRouter(f)

	admin := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleAdmin)}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedPatch(t, "/api/v1/restaurants/"+id.String(),
		map[string]any{"kwaaka_restaurant_id": "kw-99"}, &admin))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotUpdate.KwaakaRestaurantID == nil || *f.gotUpdate.KwaakaRestaurantID != "kw-99" {
		t.Errorf("KwaakaRestaurantID = %v, want \"kw-99\" to reach the facade for a superadmin caller",
			f.gotUpdate.KwaakaRestaurantID)
	}
}

// TestUpdateWithNoAuthUserStripsKwaakaRestaurantID covers the "unauthenticated
// context" branch of the same gate (GetAuthUser's ok=false), which a bare
// !ok || role != admin check can get backwards.
func TestUpdateWithNoAuthUserStripsKwaakaRestaurantID(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	r := newScopedRouter(f)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedPatch(t, "/api/v1/restaurants/"+id.String(),
		map[string]any{"kwaaka_restaurant_id": "kw-99"}, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotUpdate.KwaakaRestaurantID != nil {
		t.Errorf("KwaakaRestaurantID reached the facade as %v, want nil (no authenticated caller)",
			*f.gotUpdate.KwaakaRestaurantID)
	}
}

// kwaakaField unmarshals just the one field this test cares about, using a
// pointer-to-pointer so "key absent" (outer nil) and "key present but null"
// are both distinguishable from "key present with a value".
type kwaakaField struct {
	KwaakaRestaurantID *string `json:"kwaaka_restaurant_id"`
}

func decodeEnvelopeData(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	return env.Data
}

// TestAdminGetIncludesKwaakaRestaurantID proves the cabinet/superadmin detail
// read carries the field the frontend needs to show/edit the POS link.
func TestAdminGetIncludesKwaakaRestaurantID(t *testing.T) {
	id := uuid.New()
	kwaaka := "kw-42"
	agg := hiddenVenue(id)
	agg.KwaakaRestaurantID = &kwaaka
	r := newScopedRouter(&fakeFacade{agg: agg})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/restaurants/"+id.String(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got kwaakaField
	if err := json.Unmarshal(decodeEnvelopeData(t, w.Body.Bytes()), &got); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if got.KwaakaRestaurantID == nil || *got.KwaakaRestaurantID != kwaaka {
		t.Errorf("kwaaka_restaurant_id = %v, want %q", got.KwaakaRestaurantID, kwaaka)
	}
}

// TestAdminListIncludesKwaakaRestaurantID proves the superadmin catalog
// listing carries the same field per row.
func TestAdminListIncludesKwaakaRestaurantID(t *testing.T) {
	id := uuid.New()
	kwaaka := "kw-42"
	rest := activeVenue(id)
	rest.KwaakaRestaurantID = &kwaaka
	r := newScopedRouter(&fakeFacade{item: domain.RestaurantListItem{Restaurant: rest}})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/restaurants", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Items []kwaakaField `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(env.Data.Items) != 1 || env.Data.Items[0].KwaakaRestaurantID == nil ||
		*env.Data.Items[0].KwaakaRestaurantID != kwaaka {
		t.Errorf("admin list items = %+v, want one row carrying kwaaka_restaurant_id=%q", env.Data.Items, kwaaka)
	}
}

// TestPublicGetOmitsKwaakaRestaurantID is the write-up's core safety
// requirement: an unauthenticated guest reading a venue's detail must not
// learn its Kwaaka POS identifier.
func TestPublicGetOmitsKwaakaRestaurantID(t *testing.T) {
	id := uuid.New()
	kwaaka := "kw-42"
	rest := activeVenue(id)
	rest.KwaakaRestaurantID = &kwaaka
	r := newScopedRouter(&fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: rest}})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+id.String(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	raw := decodeEnvelopeData(t, w.Body.Bytes())
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if _, present := asMap["kwaaka_restaurant_id"]; present {
		t.Error("the public GET /restaurants/:id response must not carry kwaaka_restaurant_id")
	}
}

// TestPublicListOmitsKwaakaRestaurantID is the listing counterpart of the
// above.
func TestPublicListOmitsKwaakaRestaurantID(t *testing.T) {
	id := uuid.New()
	kwaaka := "kw-42"
	rest := activeVenue(id)
	rest.KwaakaRestaurantID = &kwaaka
	r := newScopedRouter(&fakeFacade{item: domain.RestaurantListItem{Restaurant: rest}})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Items []map[string]json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(env.Data.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(env.Data.Items))
	}
	if _, present := env.Data.Items[0]["kwaaka_restaurant_id"]; present {
		t.Error("the public GET /restaurants listing must not carry kwaaka_restaurant_id")
	}
}

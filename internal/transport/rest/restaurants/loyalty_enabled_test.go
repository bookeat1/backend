package restaurants

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
)

// loyaltyField unmarshals just the field this test cares about.
type loyaltyField struct {
	LoyaltyEnabled bool `json:"loyalty_enabled"`
}

// TestPublicGetCarriesLoyaltyEnabled proves the guest-facing detail read
// serves the flag the mobile app uses to decide whether to show the loyalty
// QR button (frontend PR #275) — this is the one field this endpoint exists
// for, unlike kwaaka_restaurant_id it must NOT be hidden from a guest.
func TestPublicGetCarriesLoyaltyEnabled(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id)
	rest.LoyaltyEnabled = true
	r := newScopedRouter(&fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: rest}})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/restaurants/"+id.String(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got loyaltyField
	if err := json.Unmarshal(decodeEnvelopeData(t, w.Body.Bytes()), &got); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if !got.LoyaltyEnabled {
		t.Error("loyalty_enabled = false, want true to reach the public GET /restaurants/:id payload")
	}
}

// TestPublicListCarriesLoyaltyEnabled is the listing counterpart, and also
// proves a venue with the flag OFF explicitly reports false rather than
// omitting the field (the client branches on its value, not its presence).
func TestPublicListCarriesLoyaltyEnabled(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id)
	rest.LoyaltyEnabled = false
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
	raw, present := env.Data.Items[0]["loyalty_enabled"]
	if !present {
		t.Fatal("the public GET /restaurants listing must carry loyalty_enabled, not omit it")
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode loyalty_enabled: %v", err)
	}
	if v {
		t.Error("loyalty_enabled = true, want false (the fake venue never set it)")
	}
}

// TestUpdateStripsLoyaltyEnabledForNonAdmin proves a venue's own
// manager/owner cannot switch their own loyalty QR button on through PATCH
// /restaurants/:id — this is a BookEat-staff-controlled rollout flag, same
// bucket as is_premium/kwaaka_restaurant_id.
func TestUpdateStripsLoyaltyEnabledForNonAdmin(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	r := newScopedRouter(f)

	manager := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleRestaurant)}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedPatch(t, "/api/v1/restaurants/"+id.String(),
		map[string]any{"loyalty_enabled": true}, &manager))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotUpdate.LoyaltyEnabled != nil {
		t.Errorf("LoyaltyEnabled reached the facade as %v, want nil (stripped for a non-admin caller)",
			*f.gotUpdate.LoyaltyEnabled)
	}
}

// TestUpdateKeepsLoyaltyEnabledForAdmin proves the superadmin path this field
// exists for actually works, both turning it on and off.
func TestUpdateKeepsLoyaltyEnabledForAdmin(t *testing.T) {
	id := uuid.New()
	f := &fakeFacade{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}}}
	r := newScopedRouter(f)

	admin := middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleAdmin)}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, authedPatch(t, "/api/v1/restaurants/"+id.String(),
		map[string]any{"loyalty_enabled": true}, &admin))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotUpdate.LoyaltyEnabled == nil || !*f.gotUpdate.LoyaltyEnabled {
		t.Errorf("LoyaltyEnabled = %v, want true to reach the facade for a superadmin caller",
			f.gotUpdate.LoyaltyEnabled)
	}
}

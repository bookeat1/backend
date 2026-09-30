package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	adminuc "backend-core/internal/usecase/admin"
)

// statefulPaySettings is a paymentSettingsWriter whose pre-order columns behave
// like the Postgres row: a patch writes only the fields marked Set.
type statefulPaySettings struct {
	fakePaySettings
	override domain.PaymentSettingsOverride
	writes   int
}

func (f *statefulPaySettings) UpdatePreorderSettings(_ context.Context, _ uuid.UUID, p domain.PreorderSettingsPatch) error {
	f.writes++
	if p.EnabledSet {
		f.override.PreorderPaymentRequired = p.Enabled
	}
	if p.MinAmountSet {
		f.override.PreorderMinAmountMinor = p.MinAmountMinor
	}
	return nil
}

func (f *statefulPaySettings) GetPaymentOverride(context.Context, uuid.UUID) (domain.PaymentSettingsOverride, error) {
	return f.override, nil
}

func preorderRouter(role domain.Role, store *statefulPaySettings, global bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	uc := adminuc.NewUseCase(
		fakePerms{allow: true}, &fakeRest{}, &fakeMenu{}, &fakeWH{}, &fakeOverrides{}, &fakeGuests{},
		&fakeBookingList{}, &fakeBookingTx{}, store, fakeTelegramSettings{},
		adminuc.WithPreorderPaymentGlobalRequired(global),
	)
	r := gin.New()
	authed := r.Group("/api/v1")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	scoped := authed.Group("")
	scoped.Use(middleware.RequireRestaurantManager(fakeManagers{manages: true}, "id"))
	NewHandler(uc).RegisterRoutes(scoped)
	return r
}

type preorderBody struct {
	Data map[string]json.RawMessage `json:"data"`
}

func decodePreorder(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var b preorderBody
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	return b.Data
}

// GET must tell "inherits" (null) from "not required" (false), and carry the
// platform default and the effective payments switch.
func TestGetPreorderSettings_DistinguishesNullFromFalse(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	f := false
	for name, tc := range map[string]struct {
		override domain.PaymentSettingsOverride
		wantKey  string
	}{
		"NULL column":  {domain.PaymentSettingsOverride{}, "null"},
		"explicit off": {domain.PaymentSettingsOverride{PreorderPaymentRequired: &f}, "false"},
	} {
		store := &statefulPaySettings{override: tc.override}
		w := do(preorderRouter(domain.RoleAdmin, store, true), http.MethodGet, base(rid)+"/payment-settings/preorder", nil, nil, uid)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", name, w.Code, w.Body)
		}
		d := decodePreorder(t, w.Body.Bytes())
		if string(d["enabled"]) != tc.wantKey {
			t.Errorf("%s: enabled = %s, want %s", name, d["enabled"], tc.wantKey)
		}
		if string(d["enabled_global"]) != "true" {
			t.Errorf("%s: enabled_global = %s, want true", name, d["enabled_global"])
		}
		for _, k := range []string{"payments_enabled_effective", "min_amount_minor"} {
			if _, ok := d[k]; !ok {
				t.Errorf("%s: response has no %q key: %s", name, k, w.Body)
			}
		}
	}
}

// An owner/manager (global role restaurant) may read but not write; the row is
// untouched. A superadmin may write.
func TestPutPreorderSettings_SuperadminOnly(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/payment-settings/preorder"

	store := &statefulPaySettings{}
	r := preorderRouter(domain.RoleRestaurant, store, false)
	if w := do(r, http.MethodGet, url, nil, nil, uid); w.Code != http.StatusOK {
		t.Fatalf("owner/manager GET: status = %d, want 200 (%s)", w.Code, w.Body)
	}
	if w := do(r, http.MethodPut, url, map[string]any{"enabled": true}, nil, uid); w.Code != http.StatusForbidden {
		t.Fatalf("owner/manager PUT: status = %d, want 403 (%s)", w.Code, w.Body)
	}
	if store.writes != 0 || store.override.PreorderPaymentRequired != nil {
		t.Fatalf("forbidden PUT changed the row: writes=%d override=%+v", store.writes, store.override)
	}

	ra := preorderRouter(domain.RoleAdmin, store, false)
	w := do(ra, http.MethodPut, url, map[string]any{"enabled": true}, nil, uid)
	if w.Code != http.StatusOK {
		t.Fatalf("superadmin PUT: status = %d, want 200 (%s)", w.Code, w.Body)
	}
	if string(decodePreorder(t, w.Body.Bytes())["enabled"]) != "true" {
		t.Fatalf("PUT response does not echo the stored value: %s", w.Body)
	}
}

// PUT semantics over the wire: absent key keeps, null clears/inherits, value sets.
func TestPutPreorderSettings_AbsentNullValue(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/payment-settings/preorder"
	yes := true
	min := int64(250000)
	store := &statefulPaySettings{override: domain.PaymentSettingsOverride{PreorderPaymentRequired: &yes, PreorderMinAmountMinor: &min}}
	r := preorderRouter(domain.RoleAdmin, store, false)

	put := func(raw string) map[string]json.RawMessage {
		t.Helper()
		w := do(r, http.MethodPut, url, nil, []byte(raw), uid)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: status = %d (%s)", raw, w.Code, w.Body)
		}
		return decodePreorder(t, w.Body.Bytes())
	}

	// Only the switch: the minimum must NOT be wiped.
	d := put(`{"enabled":false}`)
	if string(d["enabled"]) != "false" || string(d["min_amount_minor"]) != "250000" {
		t.Fatalf("switch-only PUT: %v, want enabled=false, min kept 250000", d)
	}
	// enabled: null = back to inherit; the minimum still survives.
	d = put(`{"enabled":null}`)
	if string(d["enabled"]) != "null" || string(d["min_amount_minor"]) != "250000" {
		t.Fatalf("enabled:null PUT: %v, want enabled=null, min kept", d)
	}
	// Only the minimum: the (inherit) flag must not be forced to a value.
	d = put(`{"min_amount_minor":1000}`)
	if string(d["enabled"]) != "null" || string(d["min_amount_minor"]) != "1000" {
		t.Fatalf("min-only PUT: %v, want enabled still null, min 1000", d)
	}
	// Explicit null clears the minimum.
	d = put(`{"min_amount_minor":null}`)
	if string(d["min_amount_minor"]) != "null" {
		t.Fatalf("min:null PUT: %v, want min cleared", d)
	}
}

// Out-of-range or malformed input is 422 and nothing is written.
func TestPutPreorderSettings_Validation(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/payment-settings/preorder"
	store := &statefulPaySettings{}
	r := preorderRouter(domain.RoleAdmin, store, false)
	for _, raw := range []string{
		`{"min_amount_minor":-1}`,
		`{"min_amount_minor":10000001}`,
		`{"enabled":"yes"}`,
		`{"min_amount_minor":"1000"}`,
	} {
		w := do(r, http.MethodPut, url, nil, []byte(raw), uid)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s: status = %d, want 422 (%s)", raw, w.Code, w.Body)
		}
	}
	if store.writes != 0 {
		t.Fatalf("invalid PUTs reached the store %d times", store.writes)
	}
	// The upper bound itself is accepted.
	if w := do(r, http.MethodPut, url, nil, []byte(`{"min_amount_minor":10000000}`), uid); w.Code != http.StatusOK {
		t.Fatalf("bound 10000000: status = %d, want 200 (%s)", w.Code, w.Body)
	}
}

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	adminuc "backend-core/internal/usecase/admin"
)

type kwStore struct{ row *domain.KwaakaOrderSettings }

func (s *kwStore) Get(context.Context, uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	if s.row == nil {
		return nil, domain.ErrNotFound
	}
	c := *s.row
	return &c, nil
}
func (s *kwStore) LockForUpdate(ctx context.Context, id uuid.UUID) (*domain.KwaakaOrderSettings, error) {
	return s.Get(ctx, id)
}
func (s *kwStore) Save(_ context.Context, in *domain.KwaakaOrderSettings) error {
	now := time.Now()
	in.UpdatedAt = now
	if in.OrdersEnabled {
		in.EnabledAt = &now
	}
	c := *in
	s.row = &c
	return nil
}

type kwPOS struct{ calls *int }

func (p kwPOS) GetTables(context.Context, string) ([]domain.PosTable, error) {
	if p.calls != nil {
		*p.calls++
	}
	return []domain.PosTable{{ID: "T1", Name: "BookEat 1", Number: 1, SeatingCapacity: 4}}, nil
}

type kwTx struct{}

func (kwTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (kwTx) Detach(ctx context.Context) context.Context                         { return ctx }

type linkedRest struct{ fakeRest }

func (f *linkedRest) Get(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	a, err := f.fakeRest.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	l := "kw-1"
	a.KwaakaRestaurantID = &l
	return a, nil
}

func kwaakaRouter(role domain.Role, store *kwStore, globalOn bool) *gin.Engine {
	return kwaakaRouterWith(role, true, store, kwPOS{}, globalOn)
}

func kwaakaRouterWith(role domain.Role, manages bool, store *kwStore, pos kwPOS, globalOn bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	uc := adminuc.NewUseCase(
		fakePerms{allow: true}, &linkedRest{}, &fakeMenu{}, &fakeWH{}, &fakeOverrides{}, &fakeGuests{},
		&fakeBookingList{}, &fakeBookingTx{}, fakePaySettings{}, fakeTelegramSettings{},
		adminuc.WithKwaakaOrders(store, pos, kwTx{}, adminuc.KwaakaOrdersGlobal{Enabled: globalOn, DefaultLead: time.Hour}),
	)
	r := gin.New()
	authed := r.Group("/api/v1")
	authed.Use(middleware.Auth(fakeIssuer{}, fakeUsers{role: role}))
	scoped := authed.Group("")
	scoped.Use(middleware.RequireRestaurantManager(fakeManagers{manages: manages}, "id"))
	NewHandler(uc).RegisterRoutes(scoped)
	return r
}

func TestKwaakaOrders_HTTP_SuperadminOnly(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/kwaaka-orders"
	store := &kwStore{}
	body := map[string]any{"orders_enabled": true, "pool": []map[string]any{{"kwaaka_table_id": "T1"}}}

	// An owner/manager (global role restaurant) is refused on BOTH verbs.
	r := kwaakaRouter(domain.RoleRestaurant, store, true)
	if w := do(r, http.MethodGet, url, nil, nil, uid); w.Code != http.StatusForbidden {
		t.Fatalf("owner GET = %d, want 403 (%s)", w.Code, w.Body)
	}
	if w := do(r, http.MethodPut, url, body, nil, uid); w.Code != http.StatusForbidden {
		t.Fatalf("owner PUT = %d, want 403 (%s)", w.Code, w.Body)
	}
	if store.row != nil {
		t.Fatal("forbidden PUT wrote a row")
	}

	r = kwaakaRouter(domain.RoleAdmin, store, false)
	w := do(r, http.MethodPut, url, body, nil, uid)
	if w.Code != http.StatusOK {
		t.Fatalf("superadmin PUT = %d (%s)", w.Code, w.Body)
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	// The master switch is in the response and wins over the venue switch.
	for k, want := range map[string]string{
		"global_orders_enabled": "false", "orders_enabled": "true", "will_send": "false",
		"kwaaka_restaurant_id": `"kw-1"`, "pool_stale": "false", "default_lead_minutes": "60",
		"blockers": `["global_switch_off"]`,
	} {
		if string(env.Data[k]) != want {
			t.Errorf("%s = %s, want %s (%s)", k, env.Data[k], want, w.Body)
		}
	}
	if store.row == nil || store.row.UpdatedBy == nil || *store.row.UpdatedBy != uid {
		t.Fatalf("updated_by not set from the actor: %+v", store.row)
	}
	if w := do(r, http.MethodGet, url, nil, nil, uid); w.Code != http.StatusOK {
		t.Fatalf("superadmin GET = %d (%s)", w.Code, w.Body)
	}
}

func TestKwaakaOrders_HTTP_RequiredKeysAndErrorCodes(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/kwaaka-orders"
	store := &kwStore{}
	r := kwaakaRouter(domain.RoleAdmin, store, true)

	for name, tc := range map[string]struct {
		body     any
		wantCode int
		wantErr  string
	}{
		"missing orders_enabled": {map[string]any{"pool": []any{}}, 422, ""},
		"missing pool":           {map[string]any{"orders_enabled": false}, 422, ""},
		"lead out of range":      {map[string]any{"orders_enabled": false, "lead_minutes": 1000, "pool": []any{}}, 422, "validation_failed"},
		"enable without pool":    {map[string]any{"orders_enabled": true, "pool": []any{}}, 422, "kwaaka_table_pool_required"},
		"stale form":             {map[string]any{"orders_enabled": false, "kwaaka_restaurant_id": "kw-OLD", "pool": []any{}}, 422, "kwaaka_link_mismatch"},
		"unknown table": {map[string]any{"orders_enabled": true, "pool": []map[string]any{{"kwaaka_table_id": "NOPE"}}}, 422,
			"kwaaka_table_unknown"},
	} {
		w := do(r, http.MethodPut, url, tc.body, nil, uid)
		if w.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d (%s)", name, w.Code, tc.wantCode, w.Body)
		}
		if tc.wantErr != "" {
			var e struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if e.Code != tc.wantErr {
				t.Errorf("%s: code = %q, want %q", name, e.Code, tc.wantErr)
			}
		}
	}
	if store.row != nil {
		t.Fatalf("a refused PUT wrote: %+v", store.row)
	}
}

// Authz matrix on BOTH verbs (PR #168 gate finding 5): nobody but the superadmin
// reaches the settings, and a refused call neither writes nor calls the POS.
func TestKwaakaOrders_HTTP_AuthzMatrix(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	url := base(rid) + "/kwaaka-orders"
	body := map[string]any{"orders_enabled": true, "pool": []map[string]any{{"kwaaka_table_id": "T1"}}}

	cases := []struct {
		name    string
		role    domain.Role
		manages bool
		noToken bool
		want    int
	}{
		{"no token", domain.RoleAdmin, true, true, http.StatusUnauthorized},
		{"guest", domain.RoleUser, false, false, http.StatusForbidden},
		// owner and manager are both global role "restaurant" bound to the venue
		// through restaurant_managers; neither is a platform superadmin.
		{"venue owner or manager (manages this venue)", domain.RoleRestaurant, true, false, http.StatusForbidden},
		{"restaurant account of a foreign venue", domain.RoleRestaurant, false, false, http.StatusForbidden},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				calls := 0
				store := &kwStore{}
				r := kwaakaRouterWith(tc.role, tc.manages, store, kwPOS{calls: &calls}, true)
				var w *httptest.ResponseRecorder
				if tc.noToken {
					raw, _ := json.Marshal(body)
					req := httptest.NewRequest(method, url, bytes.NewReader(raw))
					req.Header.Set("Content-Type", "application/json")
					w = httptest.NewRecorder()
					r.ServeHTTP(w, req)
				} else {
					var b any
					if method == http.MethodPut {
						b = body
					}
					w = do(r, method, url, b, nil, uid)
				}
				if w.Code != tc.want {
					t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body)
				}
				if store.row != nil || calls != 0 {
					t.Fatalf("a refused call wrote or called the POS: row=%+v posCalls=%d", store.row, calls)
				}
			})
		}
	}
}

// The 422 for unknown pool tables names them (gate finding 4).
func TestKwaakaOrders_HTTP_UnknownTablesInBody(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	r := kwaakaRouter(domain.RoleAdmin, &kwStore{}, true)
	w := do(r, http.MethodPut, base(rid)+"/kwaaka-orders", map[string]any{
		"orders_enabled": true,
		"pool":           []map[string]any{{"kwaaka_table_id": "T1"}, {"kwaaka_table_id": "GONE"}},
	}, nil, uid)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	var e struct {
		Code    string `json:"code"`
		Details struct {
			Unknown []string `json:"unknown_table_ids"`
		} `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "kwaaka_table_unknown" || len(e.Details.Unknown) != 1 || e.Details.Unknown[0] != "GONE" {
		t.Fatalf("body = %s", w.Body)
	}
}

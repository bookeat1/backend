package promocodes

import (
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
)

// fakeFacade is a hand-written double (repo convention: no mock framework).
// It records what the handler passed down, which is half of what these tests
// are about — the code must reach the usecase AS TYPED, because normalization
// is the usecase's job and doing it twice in two places is how the two
// spellings drift apart.
type fakeFacade struct {
	gotCode string
	gotRest uuid.UUID
	gotUser uuid.UUID
	res     *domain.PromoCodeResolution
	err     error
}

func (f *fakeFacade) Precheck(_ context.Context, code string, rid, uid uuid.UUID) (*domain.PromoCodeResolution, error) {
	f.gotCode, f.gotRest, f.gotUser = code, rid, uid
	return f.res, f.err
}

func (f *fakeFacade) ResolveForBooking(context.Context, string, uuid.UUID, uuid.UUID) (*domain.PromoCodeResolution, error) {
	panic("not used by the transport layer")
}
func (f *fakeFacade) ConsumeTx(context.Context, uuid.UUID, uuid.UUID) error {
	panic("not used by the transport layer")
}

func newRouter(f *fakeFacade, user *middleware.AuthUser) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	grp.Use(func(c *gin.Context) {
		if user != nil {
			c.Request = c.Request.WithContext(middleware.WithAuthUser(c.Request.Context(), *user))
		}
		c.Next()
	})
	NewHandler(f).RegisterGuestRoutes(grp)
	return r
}

func resolution() *domain.PromoCodeResolution {
	return &domain.PromoCodeResolution{
		PromoCodeID: uuid.New(),
		Code:        "MARATHON26",
		PromotionID: uuid.MustParse("6a3736b9-d4e5-4ec6-9ed2-7233476184fd"),
		Title:       "Алматы марафон",
		TitleI18n:   domain.I18n{"kk": "Алматы марафоны"},
		Terms:       "Покажите бронь",
		ValidUntil:  time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}
}

// The happy path: the guest's spelling travels untouched, and the response
// carries the NORMALIZED code back so the client stores what we store.
func TestPrecheckReturnsTheCampaignAndTheNormalizedCode(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	f := &fakeFacade{res: resolution()}
	r := newRouter(f, &middleware.AuthUser{ID: uid, Role: string(domain.RoleUser)})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/promo-codes/%20marathon-26?restaurant_id="+rid.String(), nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if f.gotCode != " marathon-26" {
		t.Errorf("code reached the usecase as %q, want it untouched", f.gotCode)
	}
	if f.gotRest != rid || f.gotUser != uid {
		t.Errorf("restaurant/user = %v/%v, want %v/%v", f.gotRest, f.gotUser, rid, uid)
	}
	var env struct {
		Data promoCodeResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Data.Code != "MARATHON26" {
		t.Errorf("code = %q, want the normalized form", env.Data.Code)
	}
	if env.Data.ValidUntil != "2026-10-27T09:00:00Z" {
		t.Errorf("valid_until = %q", env.Data.ValidUntil)
	}
}

// Accept-Language picks the campaign's translation, exactly like the promo card.
func TestPrecheckLocalizesTheTitle(t *testing.T) {
	f := &fakeFacade{res: resolution()}
	r := newRouter(f, &middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/promo-codes/MARATHON26?restaurant_id="+uuid.NewString(), nil)
	req.Header.Set("Accept-Language", "kk")
	r.ServeHTTP(w, req)

	var env struct {
		Data promoCodeResponse `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Data.Title != "Алматы марафоны" {
		t.Errorf("title = %q, want the Kazakh one", env.Data.Title)
	}
}

// Every refusal the usecase can produce must come out as its own machine code —
// that is what the client branches on to pick the text it shows.
func TestPrecheckMapsRefusalsToTheirCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantHTTP int
	}{
		{"not found", domain.WithCode(domain.CodePromoCodeNotFound, domain.ErrNotFound),
			string(domain.CodePromoCodeNotFound), http.StatusNotFound},
		{"paused", domain.WithCode(domain.CodePromoCodeInactive, domain.ErrValidation),
			string(domain.CodePromoCodeInactive), http.StatusUnprocessableEntity},
		{"expired", domain.WithCode(domain.CodePromoCodeExpired, domain.ErrValidation),
			string(domain.CodePromoCodeExpired), http.StatusUnprocessableEntity},
		{"wrong venue", domain.WithCode(domain.CodePromoCodeWrongVenue, domain.ErrValidation),
			string(domain.CodePromoCodeWrongVenue), http.StatusUnprocessableEntity},
		{"limit reached", domain.WithCode(domain.CodePromoCodeLimitReached, domain.ErrValidation),
			string(domain.CodePromoCodeLimitReached), http.StatusUnprocessableEntity},
		{"already used", domain.WithCode(domain.CodePromoCodeAlreadyUsed, domain.ErrValidation),
			string(domain.CodePromoCodeAlreadyUsed), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFacade{err: tc.err}
			r := newRouter(f, &middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
				"/api/v1/promo-codes/X26?restaurant_id="+uuid.NewString(), nil))

			if w.Code != tc.wantHTTP {
				t.Errorf("status = %d, want %d (%s)", w.Code, tc.wantHTTP, w.Body.String())
			}
			var env struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q (%s)", env.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

// A missing or unparseable restaurant is a 400 and never reaches the usecase:
// answering "this code is valid" without knowing the venue would be a promise
// the booking path can refuse a second later.
func TestPrecheckRequiresARestaurant(t *testing.T) {
	for _, q := range []string{"", "?restaurant_id=", "?restaurant_id=nonsense"} {
		f := &fakeFacade{res: resolution()}
		r := newRouter(f, &middleware.AuthUser{ID: uuid.New(), Role: string(domain.RoleUser)})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/promo-codes/MARATHON26"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", q, w.Code)
		}
		if f.gotCode != "" {
			t.Errorf("query %q: the usecase was called anyway", q)
		}
	}
}

// Without auth there is nobody to answer "have you already used it" for.
func TestPrecheckRefusesAnonymous(t *testing.T) {
	f := &fakeFacade{res: resolution()}
	r := newRouter(f, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/promo-codes/MARATHON26?restaurant_id="+uuid.NewString(), nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (%s)", w.Code, w.Body.String())
	}
}

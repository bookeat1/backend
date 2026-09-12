package promocodes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/promocodes"
)

// fakeEditor is a hand-written double for uc.Editor (repo convention: no mock
// framework). Patch records exactly the PatchPromoCodeInput it received so a
// test can assert on the MaxUsesTotal double pointer without going through a
// real usecase.
type fakeEditor struct {
	patchIn uc.PatchPromoCodeInput
	patched bool
	item    *uc.AdminPromoCode
	err     error
}

func (f *fakeEditor) List(context.Context, domain.PromoCodeFilter) ([]uc.AdminPromoCode, error) {
	panic("not used by these tests")
}
func (f *fakeEditor) Get(context.Context, uuid.UUID) (*uc.AdminPromoCode, error) {
	panic("not used by these tests")
}
func (f *fakeEditor) Create(context.Context, uc.CreatePromoCodeInput, uuid.UUID) (*uc.AdminPromoCode, error) {
	panic("not used by these tests")
}
func (f *fakeEditor) Patch(_ context.Context, _ uuid.UUID, in uc.PatchPromoCodeInput) (*uc.AdminPromoCode, error) {
	f.patchIn, f.patched = in, true
	if f.err != nil {
		return nil, f.err
	}
	if f.item != nil {
		return f.item, nil
	}
	return &uc.AdminPromoCode{Code: domain.PromoCode{
		ID: uuid.New(), Code: "MARATHON26", PromotionID: uuid.New(),
		StartsAt: time.Now(), ExpiresAt: time.Now().Add(24 * time.Hour),
		Status: domain.PromoCodeActive, MaxUsesPerUser: 1,
	}}, nil
}
func (f *fakeEditor) Delete(context.Context, uuid.UUID) error {
	panic("not used by these tests")
}

func newAdminRouter(e *fakeEditor) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	NewAdminHandler(e).RegisterAdminRoutes(grp)
	return r
}

func patchRequest(id uuid.UUID, body string) *http.Request {
	return httptest.NewRequest(http.MethodPatch,
		"/api/v1/admin/promo-codes/"+id.String(), strings.NewReader(body))
}

// An explicit JSON null for max_uses_total must reach the usecase as a
// non-nil **int pointing at nil ("clear the limit"), not as a nil **int
// ("field wasn't sent") — that is exactly the bug the gate blocked: a double
// pointer decoded straight off the wire by encoding/json collapses both cases
// to the same nil outer pointer.
func TestPatchExplicitNullClearsTheOverallLimit(t *testing.T) {
	e := &fakeEditor{}
	r := newAdminRouter(e)
	w := httptest.NewRecorder()
	req := patchRequest(uuid.New(), `{"max_uses_total": null}`)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !e.patched {
		t.Fatal("the usecase was never called")
	}
	if e.patchIn.MaxUsesTotal == nil {
		t.Fatal("MaxUsesTotal reached the usecase as nil (absent) — null must set the OUTER pointer, not leave it nil")
	}
	if *e.patchIn.MaxUsesTotal != nil {
		t.Errorf("MaxUsesTotal = pointer to %v, want pointer to nil (no overall limit)", **e.patchIn.MaxUsesTotal)
	}
}

// A body that never mentions max_uses_total must leave it untouched: the
// usecase treats a nil **int as "don't touch it" (admin.go:204), so the
// transport layer must not manufacture one.
func TestPatchWithoutMaxUsesFieldLeavesLimitUntouched(t *testing.T) {
	e := &fakeEditor{}
	r := newAdminRouter(e)
	w := httptest.NewRecorder()
	req := patchRequest(uuid.New(), `{"status": "paused"}`)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if e.patchIn.MaxUsesTotal != nil {
		t.Errorf("MaxUsesTotal = %v, want nil (field wasn't sent)", **e.patchIn.MaxUsesTotal)
	}
}

// Setting an explicit numeric value must also reach the usecase correctly —
// the third leg of the tri-state, so a fix for null doesn't quietly break the
// ordinary "change the limit to N" call.
func TestPatchWithNumericValueSetsTheLimit(t *testing.T) {
	e := &fakeEditor{}
	r := newAdminRouter(e)
	w := httptest.NewRecorder()
	req := patchRequest(uuid.New(), `{"max_uses_total": 50}`)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if e.patchIn.MaxUsesTotal == nil || *e.patchIn.MaxUsesTotal == nil {
		t.Fatal("MaxUsesTotal did not reach the usecase as a set value")
	}
	if got := **e.patchIn.MaxUsesTotal; got != 50 {
		t.Errorf("MaxUsesTotal = %d, want 50", got)
	}
}

// A garbage value for max_uses_total is a 400, not a silent zero.
func TestPatchRejectsAnInvalidMaxUsesTotal(t *testing.T) {
	e := &fakeEditor{}
	r := newAdminRouter(e)
	w := httptest.NewRecorder()
	req := patchRequest(uuid.New(), `{"max_uses_total": "not-a-number"}`)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", w.Code, w.Body.String())
	}
	if e.patched {
		t.Error("the usecase was called with a garbage value")
	}
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != string(domain.CodeValidation) {
		t.Errorf("code = %q, want %q", env.Code, domain.CodeValidation)
	}
}

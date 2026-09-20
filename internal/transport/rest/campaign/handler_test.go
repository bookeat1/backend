package campaign

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/campaign"
)

// --- fake resolver -----------------------------------------------------------

type fakeResolver struct {
	outcome campaign.Outcome
	link    *domain.CampaignLink
	err     error

	gotSlug string
	gotUA   string
}

func (f *fakeResolver) Resolve(_ context.Context, slug, userAgent string) (campaign.Outcome, *domain.CampaignLink, error) {
	f.gotSlug, f.gotUA = slug, userAgent
	return f.outcome, f.link, f.err
}

func newTestRouter(r resolver, webBaseURL string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	g := gin.New()
	NewHandler(r, webBaseURL).RegisterRoutes(g)
	return g
}

func TestResolveActiveLinkReturnsLandingWithBothButtons(t *testing.T) {
	promoID := uuid.MustParse("6a3736b9-d4e5-4ec6-9ed2-7233476184fd")
	f := &fakeResolver{
		outcome: campaign.OutcomeActive,
		link: &domain.CampaignLink{
			ID:          uuid.New(),
			Slug:        "am26-flyer",
			PromotionID: promoID,
			TargetURL:   "https://bookeat.godetour.link/lQ9BPpUvJc",
			Status:      domain.CampaignLinkActive,
		},
	}
	g := newTestRouter(f, "https://book-eat.com")

	req := httptest.NewRequest(http.MethodGet, "/m/am26-flyer", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0)")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://bookeat.godetour.link/lQ9BPpUvJc") {
		t.Errorf("body missing the install/open link: %s", body)
	}
	if !strings.Contains(body, "https://book-eat.com/?promo=6a3736b9-d4e5-4ec6-9ed2-7233476184fd") {
		t.Errorf("body missing the web fallback link: %s", body)
	}
	if f.gotSlug != "am26-flyer" {
		t.Errorf("resolver got slug %q, want am26-flyer", f.gotSlug)
	}
	if f.gotUA == "" {
		t.Error("resolver did not receive the User-Agent")
	}
}

func TestResolveUnknownSlugReturns404(t *testing.T) {
	f := &fakeResolver{outcome: campaign.OutcomeNotFound, link: nil}
	g := newTestRouter(f, "https://book-eat.com")

	req := httptest.NewRequest(http.MethodGet, "/m/does-not-exist", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestResolveDisabledSlugReturns200CampaignOver(t *testing.T) {
	f := &fakeResolver{
		outcome: campaign.OutcomeDisabled,
		link: &domain.CampaignLink{
			Slug:   "am26-flyer",
			Status: domain.CampaignLinkDisabled,
		},
	}
	g := newTestRouter(f, "https://book-eat.com")

	req := httptest.NewRequest(http.MethodGet, "/m/am26-flyer", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "завершена") {
		t.Errorf("body does not say the campaign is over: %s", rec.Body.String())
	}
}

func TestResolveErrorReturns500(t *testing.T) {
	f := &fakeResolver{err: context.DeadlineExceeded}
	g := newTestRouter(f, "https://book-eat.com")

	req := httptest.NewRequest(http.MethodGet, "/m/am26-flyer", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

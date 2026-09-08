package staticmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/staticmap"
)

// fakeCoords is the restaurant lookup the usecase needs.
type fakeCoords struct {
	byID map[uuid.UUID]*domain.RestaurantAggregate
}

func (f fakeCoords) GetByID(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

// fakeProvider is the map vendor double.
type fakeProvider struct {
	img uc.Image
	err error
}

func (fakeProvider) Name() string { return "fake" }
func (p fakeProvider) Render(context.Context, uc.RenderRequest) (uc.Image, error) {
	return p.img, p.err
}

var pngBody = []byte("\x89PNG\r\n\x1a\nfake")

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newRouter wires the REAL usecase behind the handler over fakes, so these
// tests exercise the same status/code mapping production uses.
func newRouter(t *testing.T, provider uc.Provider, venues map[uuid.UUID]*domain.RestaurantAggregate) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	usecase := uc.New(fakeCoords{byID: venues}, provider, uc.NewMemoryCache(time.Hour, uc.DefaultCacheMaxBytes), quiet())
	NewHandler(usecase).RegisterPublic(api)
	return r
}

func activeVenue(id uuid.UUID) map[uuid.UUID]*domain.RestaurantAggregate {
	lat, lon := 51.128207, 71.430420
	return map[uuid.UUID]*domain.RestaurantAggregate{
		id: {Restaurant: domain.Restaurant{ID: id, Latitude: &lat, Longitude: &lon, IsActive: true}},
	}
}

func do(r *gin.Engine, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// decodeEnvelope asserts the failure body is the repo's standard JSON envelope
// and returns it. The app branches on "code", never on the message.
func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) response.Envelope {
	t.Helper()
	var env response.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not a JSON envelope (%q): %v", w.Body.String(), err)
	}
	return env
}

// The success path is bytes, not an envelope, plus the caching headers the app
// and any CDN in front of us rely on.
func TestGetMapSuccess(t *testing.T) {
	id := uuid.New()
	r := newRouter(t, fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}}, activeVenue(id))

	w := do(r, "/api/v1/restaurants/"+id.String()+"/map", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if w.Body.String() != string(pngBody) {
		t.Errorf("body was not the provider's image")
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q, want public, max-age=3600 (the cache TTL)", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag — a repeat request cannot be answered with 304")
	}
}

func TestGetMapNotModified(t *testing.T) {
	id := uuid.New()
	r := newRouter(t, fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}}, activeVenue(id))
	path := "/api/v1/restaurants/" + id.String() + "/map"

	first := do(r, path, nil)
	etag := first.Header().Get("ETag")

	second := do(r, path, map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", second.Body.Len())
	}
	if second.Header().Get("Cache-Control") == "" {
		t.Error("304 must still carry Cache-Control")
	}
}

// Each failure mode has to be distinguishable by the client. This is the table
// the mobile app is written against.
func TestGetMapFailureModes(t *testing.T) {
	known := uuid.New()
	unknown := uuid.New()
	noCoords := uuid.New()

	venuesWithBoth := activeVenue(known)
	venuesWithBoth[noCoords] = &domain.RestaurantAggregate{
		Restaurant: domain.Restaurant{ID: noCoords, IsActive: true}, // lat/lon NULL
	}

	tests := []struct {
		name     string
		provider uc.Provider
		target   string
		wantCode int
		wantBody domain.ErrorCode
	}{
		{
			name:     "unknown restaurant",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/" + unknown.String() + "/map",
			wantCode: http.StatusNotFound,
			wantBody: domain.CodeNotFound,
		},
		{
			name:     "restaurant id is not a uuid",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/not-a-uuid/map",
			wantCode: http.StatusNotFound,
			wantBody: domain.CodeNotFound,
		},
		{
			name:     "restaurant without coordinates",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/" + noCoords.String() + "/map",
			wantCode: http.StatusNotFound,
			wantBody: domain.CodeMapNoCoordinates,
		},
		{
			name:     "no provider key configured",
			provider: nil,
			target:   "/api/v1/restaurants/" + known.String() + "/map",
			wantCode: http.StatusServiceUnavailable,
			wantBody: domain.CodeMapNotConfigured,
		},
		{
			name:     "provider down",
			provider: fakeProvider{err: uc.ErrProviderUnavailable},
			target:   "/api/v1/restaurants/" + known.String() + "/map",
			wantCode: http.StatusServiceUnavailable,
			wantBody: domain.CodeMapProviderUnavailable,
		},
		{
			name:     "provider rate limited",
			provider: fakeProvider{err: uc.ErrProviderRateLimited},
			target:   "/api/v1/restaurants/" + known.String() + "/map",
			wantCode: http.StatusServiceUnavailable,
			wantBody: domain.CodeMapProviderRateLimited,
		},
		{
			name:     "size outside the whitelist",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/" + known.String() + "/map?size=4096x4096",
			wantCode: http.StatusUnprocessableEntity,
			wantBody: domain.CodeValidation,
		},
		{
			name:     "zoom outside the whitelist",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/" + known.String() + "/map?zoom=22",
			wantCode: http.StatusUnprocessableEntity,
			wantBody: domain.CodeValidation,
		},
		{
			name:     "scale outside the whitelist",
			provider: fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}},
			target:   "/api/v1/restaurants/" + known.String() + "/map?scale=8",
			wantCode: http.StatusUnprocessableEntity,
			wantBody: domain.CodeValidation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var provider uc.Provider
			if tc.provider != nil {
				provider = tc.provider
			}
			r := newRouter(t, provider, venuesWithBoth)

			w := do(r, tc.target, nil)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantCode, w.Body.String())
			}
			// Never a 500 with a stack: every one of these is an expected outcome.
			if w.Code >= 500 && w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d — this failure must never be an internal error", w.Code)
			}
			env := decodeEnvelope(t, w)
			if env.Code != string(tc.wantBody) {
				t.Errorf("code = %q, want %q", env.Code, tc.wantBody)
			}
			if env.Error == "" {
				t.Error("empty error message")
			}
			// A failure must never be served as a half-image.
			if ct := w.Header().Get("Content-Type"); ct != "" && !contains(ct, "json") {
				t.Errorf("Content-Type = %q on a failure, want JSON", ct)
			}
		})
	}
}

// Every whitelisted combination must actually be reachable through the route.
func TestGetMapAcceptsWholeWhitelist(t *testing.T) {
	id := uuid.New()
	r := newRouter(t, fakeProvider{img: uc.Image{Bytes: pngBody, ContentType: "image/png"}}, activeVenue(id))
	for _, size := range uc.SizeNames() {
		for _, scale := range []int{1, 2} {
			for _, zoom := range []int{14, 15, 16, 17, 18} {
				target := fmt.Sprintf("/api/v1/restaurants/%s/map?size=%s&scale=%d&zoom=%d", id, size, scale, zoom)
				if w := do(r, target, nil); w.Code != http.StatusOK {
					t.Errorf("%s → %d, want 200", target, w.Code)
				}
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

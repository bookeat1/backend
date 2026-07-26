package twogis

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"backend-core/internal/usecase/staticmap"
)

const testAPIKey = "fake-2gis-key" //nolint:gosec // test-only literal, not a credential

// sampleRequest is a fully bounded render, as ParseParams would have produced.
var sampleRequest = staticmap.RenderRequest{
	Lat: 51.128207, Lon: 71.430420,
	Width: 640, Height: 360, Scale: 1, Zoom: 16,
}

// fakeProvider stands in for 2GIS: it records the raw request URI (so the URL
// shape can be pinned) and replies with whatever the test scripts. No live
// network is ever touched.
func fakeProvider(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func pngHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	cfg := Config{APIKey: testAPIKey, BaseURL: srv.URL, Timeout: 2 * time.Second}
	if !cfg.Configured() {
		t.Fatal("test config should be Configured()")
	}
	return NewClient(cfg)
}

// The URL shape IS the integration: 2GIS's documented parameter syntax uses
// "@", "~" and ":" structurally, and a query encoder that percent-escapes them
// would silently produce a different (or rejected) request. This pins it.
func TestRenderRequestURLShape(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	if _, err := newTestClient(t, srv).Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	uri := (*seen)[0]

	for _, want := range []string{
		"s=640x360",                       // documented {width}x{height}
		"z=16",                            // documented zoom
		"c=51.128207,71.430420",           // documented "latitude,longitude"
		"pt=51.128207,71.430420~k:p~c:rd", // a small red pin on the venue
		"key=" + testAPIKey,
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("request URI %q does not contain %q", uri, want)
		}
	}
	if strings.Contains(uri, "%3A") || strings.Contains(uri, "%7E") {
		t.Errorf("request URI %q has percent-escaped 2GIS syntax characters", uri)
	}
	// @1x is the provider default and must not be sent explicitly.
	if strings.Contains(uri, "@") {
		t.Errorf("request URI %q carries a scale suffix at scale 1", uri)
	}
}

func TestRenderHiDPIAppendsScaleSuffix(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	req := sampleRequest
	req.Scale = 2
	if _, err := newTestClient(t, srv).Render(context.Background(), req); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains((*seen)[0], "s=640x360@2x") {
		t.Errorf("request URI %q, want s=640x360@2x", (*seen)[0])
	}
}

func TestRenderSuccessReturnsBytesAndMediaType(t *testing.T) {
	srv, _ := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write([]byte("\x89PNGdata"))
	})
	img, err := newTestClient(t, srv).Render(context.Background(), sampleRequest)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(img.Bytes) != "\x89PNGdata" {
		t.Errorf("bytes = %q", img.Bytes)
	}
	if img.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png (parameters stripped)", img.ContentType)
	}
}

func TestRenderOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        error
	}{
		{"rate limited", http.StatusTooManyRequests, "application/json", `{"error":"quota"}`, staticmap.ErrProviderRateLimited},
		{"server error", http.StatusInternalServerError, "text/plain", "boom", staticmap.ErrProviderUnavailable},
		{"bad gateway", http.StatusBadGateway, "text/html", "<html>", staticmap.ErrProviderUnavailable},
		{"bad key", http.StatusUnauthorized, "application/json", `{"error":"bad key"}`, staticmap.ErrProviderRejected},
		{"forbidden", http.StatusForbidden, "application/json", `{"error":"no plan"}`, staticmap.ErrProviderRejected},
		{"bad parameters", http.StatusBadRequest, "application/json", `{"error":"bad size"}`, staticmap.ErrProviderRejected},
		{"200 but not an image", http.StatusOK, "application/json", `{"error":"something"}`, staticmap.ErrProviderUnavailable},
		{"200 empty image", http.StatusOK, "image/png", "", staticmap.ErrProviderUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := newTestClient(t, srv).Render(context.Background(), sampleRequest)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			assertNoKeyLeak(t, err)
		})
	}
}

// A provider that never answers must not hold a guest's request open: the
// client's own timeout has to fire and report a transient failure.
func TestRenderProviderTimeout(t *testing.T) {
	block := make(chan struct{})
	srv, _ := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		<-block
		pngHandler(w, nil)
	})
	t.Cleanup(func() { close(block) })

	c := NewClient(Config{APIKey: testAPIKey, BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Render(context.Background(), sampleRequest)
	if !errors.Is(err, staticmap.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Render took %v — the client timeout did not fire", elapsed)
	}
	assertNoKeyLeak(t, err)
}

// The caller's context deadline must be honoured too, not only the client's.
func TestRenderRespectsContextDeadline(t *testing.T) {
	block := make(chan struct{})
	srv, _ := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		<-block
		pngHandler(w, nil)
	})
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestClient(t, srv).Render(ctx, sampleRequest)
	if !errors.Is(err, staticmap.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
	assertNoKeyLeak(t, err)
}

// A misbehaving endpoint must not be able to make us allocate without bound.
func TestRenderRejectsOversizedBody(t *testing.T) {
	srv, _ := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 5000))
	})
	c := NewClient(Config{APIKey: testAPIKey, BaseURL: srv.URL, Timeout: 2 * time.Second, MaxBytes: 1024})
	if _, err := c.Render(context.Background(), sampleRequest); !errors.Is(err, staticmap.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
}

// The one rule that has no acceptable exception: the key must never appear in
// anything we return or log. Go's *url.Error stringifies the whole request URL,
// which is exactly how it would leak.
func assertNoKeyLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("error text leaks the API key: %v", err)
	}
}

func TestRenderUnreachableProviderDoesNotLeakTheKey(t *testing.T) {
	// A closed listener: net/http fails with *url.Error carrying the full URL.
	srv := httptest.NewServer(http.HandlerFunc(pngHandler))
	base := srv.URL
	srv.Close()

	c := NewClient(Config{APIKey: testAPIKey, BaseURL: base, Timeout: time.Second})
	_, err := c.Render(context.Background(), sampleRequest)
	if !errors.Is(err, staticmap.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
	assertNoKeyLeak(t, err)
}

func TestConfiguredRequiresAKey(t *testing.T) {
	if (Config{}).Configured() {
		t.Error("empty config reported as configured")
	}
	if (Config{APIKey: "   "}).Configured() {
		t.Error("whitespace-only key reported as configured")
	}
	if !(Config{APIKey: "k"}).Configured() {
		t.Error("a key should be enough to be configured")
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(Config{APIKey: testAPIKey})
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.http.Timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", c.http.Timeout, defaultTimeout)
	}
	if c.maxBytes != defaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", c.maxBytes, defaultMaxBytes)
	}
	if c.Name() != "2gis" {
		t.Errorf("Name = %q, want 2gis", c.Name())
	}
}

// A key with URL-significant characters must not break out of its query value.
func TestAPIKeyIsEscapedInTheQuery(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	c := NewClient(Config{APIKey: "a b&z=9#x+y", BaseURL: srv.URL, Timeout: time.Second})
	if _, err := c.Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	uri := (*seen)[0]
	parsed, err := url.ParseRequestURI(uri)
	if err != nil {
		t.Fatalf("built an unparseable URI %q: %v", uri, err)
	}
	q := parsed.Query()
	if got := q.Get("key"); got != "a b&z=9#x+y" {
		t.Errorf("key round-trips as %q, want the literal key back", got)
	}
	if got := q.Get("z"); got != "16" {
		t.Errorf("z = %q, want 16 — the key escaped its own value", got)
	}
}

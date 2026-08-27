package geoapify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"backend-core/internal/usecase/staticmap"
)

const testAPIKey = "fake-geoapify-key" //nolint:gosec // test-only literal, not a credential

// sampleRequest is a fully bounded render, as ParseParams would have produced.
// The coordinates are Astana's centre — deliberately a point where lat and lon
// cannot be confused with each other, because Geoapify takes lon,lat while 2GIS
// takes lat,lon and that swap is the single most likely bug in this file.
var sampleRequest = staticmap.RenderRequest{
	Lat: 51.128207, Lon: 71.430420,
	Width: 640, Height: 360, Scale: 1, Zoom: 16,
}

// fakeProvider stands in for Geoapify: it records the raw request URI (so the
// URL shape can be pinned) and replies with whatever the test scripts. No live
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

// The URL shape IS the integration. Geoapify's syntax uses ":" and ";"
// structurally, and a query encoder that percent-escapes them would silently
// produce a rejected request. This pins the whole documented shape.
func TestRenderRequestURLShape(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	if _, err := newTestClient(t, srv).Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	uri := (*seen)[0]

	for _, want := range []string{
		"style=" + DefaultStyle,
		"width=640",
		"height=360",
		"format=png",
		"center=lonlat:71.430420,51.128207", // lon,lat — NOT lat,lon
		"zoom=16",                           //
		"marker=lonlat:71.430420,51.128207;type:material;color:%23e53935;size:40", // the venue pin
		"lang=" + DefaultLang,
		"apiKey=" + testAPIKey,
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("request URI %q does not contain %q", uri, want)
		}
	}
	if strings.Contains(uri, "%3A") || strings.Contains(uri, "%3B") {
		t.Errorf("request URI %q has percent-escaped Geoapify syntax characters", uri)
	}
	// scaleFactor=1 is the provider default and must not be sent explicitly.
	if strings.Contains(uri, "scaleFactor") {
		t.Errorf("request URI %q carries a scaleFactor at scale 1", uri)
	}
}

func TestRenderHiDPIAsksForScaleFactorTwo(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	req := sampleRequest
	req.Scale = 2
	if _, err := newTestClient(t, srv).Render(context.Background(), req); err != nil {
		t.Fatalf("Render: %v", err)
	}
	uri := (*seen)[0]
	if !strings.Contains(uri, "scaleFactor=2") {
		t.Errorf("request URI %q, want scaleFactor=2", uri)
	}
	// width/height stay LOGICAL — scaleFactor multiplies the output pixels.
	// Doubling them here as well would ask for a 2560x1440 render and burn four
	// times the credits.
	if !strings.Contains(uri, "width=640") || !strings.Contains(uri, "height=360") {
		t.Errorf("request URI %q, want the logical size unchanged at scale 2", uri)
	}
}

// The style and the language are operator-set, and "-" is the documented way to
// leave the provider's own (local-name) labelling alone.
func TestRenderHonoursConfiguredStyleAndLang(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	c := NewClient(Config{APIKey: testAPIKey, BaseURL: srv.URL, Style: "osm-carto", Lang: "kk", Timeout: time.Second})
	if _, err := c.Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if uri := (*seen)[0]; !strings.Contains(uri, "style=osm-carto") || !strings.Contains(uri, "lang=kk") {
		t.Errorf("request URI %q, want style=osm-carto and lang=kk", uri)
	}

	srv2, seen2 := fakeProvider(t, pngHandler)
	c2 := NewClient(Config{APIKey: testAPIKey, BaseURL: srv2.URL, Lang: "-", Timeout: time.Second})
	if _, err := c2.Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if uri := (*seen2)[0]; strings.Contains(uri, "lang=") {
		t.Errorf("request URI %q carries lang= although it was disabled with \"-\"", uri)
	}
}

// A key with URL-hostile characters must not break the query string apart.
func TestRenderEscapesTheKeyWithoutBreakingTheQuery(t *testing.T) {
	srv, seen := fakeProvider(t, pngHandler)
	c := NewClient(Config{APIKey: "a b&c=d", BaseURL: srv.URL, Timeout: time.Second})
	if _, err := c.Render(context.Background(), sampleRequest); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if uri := (*seen)[0]; !strings.HasSuffix(uri, "apiKey=a%20b%26c%3Dd") {
		t.Errorf("request URI %q, want a fully escaped apiKey at the end", uri)
	}
}

// Without a key the provider must not be constructed at all — the usecase then
// answers map_not_configured instead of paying for a request that cannot work.
func TestConfiguredRequiresAKey(t *testing.T) {
	for _, key := range []string{"", "   ", "\t\n"} {
		if (Config{APIKey: key}).Configured() {
			t.Errorf("Configured() = true for key %q, want false", key)
		}
	}
	if !(Config{APIKey: " k "}).Configured() {
		t.Error("Configured() = false for a non-blank key")
	}
}

func TestNewClientFallsBackToDefaults(t *testing.T) {
	c := NewClient(Config{APIKey: testAPIKey})
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.style != DefaultStyle {
		t.Errorf("style = %q, want %q", c.style, DefaultStyle)
	}
	if c.lang != DefaultLang {
		t.Errorf("lang = %q, want %q", c.lang, DefaultLang)
	}
	if c.maxBytes != defaultMaxBytes {
		t.Errorf("maxBytes = %d, want %d", c.maxBytes, defaultMaxBytes)
	}
	if c.Name() != "geoapify" {
		t.Errorf("Name() = %q, want geoapify (it is part of the cache key)", c.Name())
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
		{"bad key", http.StatusUnauthorized, "application/json", `{"error":"Invalid apiKey"}`, staticmap.ErrProviderRejected},
		{"forbidden", http.StatusForbidden, "application/json", `{"error":"no plan"}`, staticmap.ErrProviderRejected},
		{"bad parameters", http.StatusBadRequest, "application/json", `{"error":"bad zoom"}`, staticmap.ErrProviderRejected},
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

// The provider's 4xx body quotes the request back at us, key included. It must
// never reach the error we return.
func TestRenderDoesNotEchoTheProviderBody(t *testing.T) {
	srv, _ := fakeProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad request","url":"` + r.URL.RequestURI() + `"}`))
	})
	_, err := newTestClient(t, srv).Render(context.Background(), sampleRequest)
	if !errors.Is(err, staticmap.ErrProviderRejected) {
		t.Fatalf("error = %v, want ErrProviderRejected", err)
	}
	assertNoKeyLeak(t, err)
	if strings.Contains(err.Error(), "bad request") {
		t.Errorf("error echoes the provider body: %v", err)
	}
}

// A provider that never answers must not hold a guest's request open.
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

// The success path's own key-leak hazard: the key travels in the query string,
// so following a redirect would re-send it to whatever the 3xx points at AND
// attach a Referer header carrying the full previous URL to a third party we
// never chose. So: refuse to follow, and prove the target never hears from us.
func TestRenderDoesNotFollowRedirectsAndNeverForwardsTheKey(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// The third party. It must never be contacted. Guarded by a mutex
			// because the handler runs on the server's goroutine, not the test's.
			var mu sync.Mutex
			var attackerHits []string
			attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attackerHits = append(attackerHits,
					r.Method+" "+r.URL.RequestURI()+" Referer="+r.Header.Get("Referer"))
				mu.Unlock()
				pngHandler(w, r)
			}))
			t.Cleanup(attacker.Close)

			provider, seen := fakeProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", attacker.URL+"/relay")
				w.WriteHeader(status)
			})

			_, err := newTestClient(t, provider).Render(context.Background(), sampleRequest)
			if !errors.Is(err, staticmap.ErrProviderUnavailable) {
				t.Fatalf("error = %v, want ErrProviderUnavailable (a redirect is not a usable image)", err)
			}
			assertNoKeyLeak(t, err)

			mu.Lock()
			hits := append([]string(nil), attackerHits...)
			mu.Unlock()
			if len(hits) != 0 {
				t.Fatalf("the redirect target was contacted (%s) — the key was handed to a third party", hits[0])
			}
			if len(*seen) != 1 {
				t.Errorf("provider saw %d requests, want exactly 1 (no redirect hops)", len(*seen))
			}
		})
	}
}

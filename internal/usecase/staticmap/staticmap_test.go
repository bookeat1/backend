package staticmap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func defaultParams(t *testing.T) Params {
	t.Helper()
	p, err := ParseParams("", "", "")
	if err != nil {
		t.Fatalf("ParseParams defaults: %v", err)
	}
	return p
}

// newUC wires the usecase over the two fakes plus a real memory cache.
func newUC(t *testing.T, rest *fakeRestaurants, p Provider) *UseCase {
	t.Helper()
	return New(rest, p, NewMemoryCache(time.Hour, DefaultCacheMaxBytes), quietLogger())
}

// pngBytes is a stand-in body; the usecase never parses it, it only must not be
// empty (an empty body would render as a broken tile in the app).
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}

func TestRenderSuccessPassesCoordinatesAndBounds(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.128207), ptr(71.430420), true) // Astana

	prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
	uc := newUC(t, rest, prov)

	p, err := ParseParams("wide", "2", "17")
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	img, err := uc.Render(context.Background(), id, p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(img.Bytes) != string(pngBytes) {
		t.Errorf("image bytes not passed through")
	}
	if img.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", img.ContentType)
	}
	// The ETag is what makes the 304 path work; the usecase must fill it in when
	// the provider does not.
	if img.ETag == "" || img.ETag != ETagOf(pngBytes) {
		t.Errorf("ETag = %q, want %q", img.ETag, ETagOf(pngBytes))
	}

	got := prov.lastRequest()
	want := RenderRequest{Lat: 51.128207, Lon: 71.430420, Width: 640, Height: 360, Scale: 2, Zoom: 17}
	if got != want {
		t.Errorf("provider request = %+v, want %+v", got, want)
	}
}

// The whole point of the cache: a second screen open must cost neither a
// provider request (money) nor a database round-trip.
func TestRenderCacheHitCallsNeitherProviderNorRepositoryTwice(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(43.238949), ptr(76.889709), true) // Almaty
	prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
	uc := newUC(t, rest, prov)
	p := defaultParams(t)

	for i := 0; i < 3; i++ {
		if _, err := uc.Render(context.Background(), id, p); err != nil {
			t.Fatalf("Render #%d: %v", i, err)
		}
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("provider calls = %d, want 1", n)
	}
	if n := rest.calls.Load(); n != 1 {
		t.Errorf("repository calls = %d, want 1", n)
	}
}

// Different whitelisted params are different pictures, so they must not share a
// cache entry.
func TestRenderDifferentParamsAreDifferentCacheEntries(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)
	prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
	uc := newUC(t, rest, prov)

	small, _ := ParseParams("card", "", "")
	wide, _ := ParseParams("wide", "", "")
	if _, err := uc.Render(context.Background(), id, small); err != nil {
		t.Fatalf("Render small: %v", err)
	}
	if _, err := uc.Render(context.Background(), id, wide); err != nil {
		t.Fatalf("Render wide: %v", err)
	}
	if n := prov.calls.Load(); n != 2 {
		t.Errorf("provider calls = %d, want 2 (one per size)", n)
	}
}

// A failure must NOT be cached: a provider outage would otherwise pin a venue's
// map to "unavailable" for the whole TTL.
func TestRenderFailureIsNotCached(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)
	prov := &fakeProvider{err: ErrProviderUnavailable}
	uc := newUC(t, rest, prov)
	p := defaultParams(t)

	if _, err := uc.Render(context.Background(), id, p); err == nil {
		t.Fatal("Render: want error")
	}
	prov.err = nil
	prov.img = Image{Bytes: pngBytes, ContentType: "image/png"}
	if _, err := uc.Render(context.Background(), id, p); err != nil {
		t.Fatalf("Render after recovery: %v", err)
	}
	if n := prov.calls.Load(); n != 2 {
		t.Errorf("provider calls = %d, want 2 (the failure must not be cached)", n)
	}
}

func TestRenderProviderOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		providerErr error
		wantErr     error
		wantCode    domain.ErrorCode
	}{
		{"provider down", ErrProviderUnavailable, domain.ErrUnavailable, domain.CodeMapProviderUnavailable},
		{"rate limited", ErrProviderRateLimited, domain.ErrUnavailable, domain.CodeMapProviderRateLimited},
		{"rejected (bad key)", ErrProviderRejected, domain.ErrUnavailable, domain.CodeMapProviderUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			rest := newFakeRestaurants()
			rest.add(id, ptr(51.1), ptr(71.4), true)
			uc := newUC(t, rest, &fakeProvider{err: tc.providerErr})

			_, err := uc.Render(context.Background(), id, defaultParams(t))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			// A provider failure must never surface as an internal error.
			if errors.Is(err, domain.ErrNotFound) {
				t.Error("provider failure must not masquerade as not found")
			}
			code, ok := domain.CodeOf(err)
			if !ok || code != tc.wantCode {
				t.Errorf("code = %q (ok=%v), want %q", code, ok, tc.wantCode)
			}
		})
	}
}

// A provider that answers 200 with nothing must not become a zero-byte
// "image" the app tries to render.
func TestRenderEmptyImageIsTreatedAsProviderFailure(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)
	uc := newUC(t, rest, &fakeProvider{img: Image{ContentType: "image/png"}})

	_, err := uc.Render(context.Background(), id, defaultParams(t))
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeMapProviderUnavailable {
		t.Errorf("code = %q, want %q", code, domain.CodeMapProviderUnavailable)
	}
}

func TestRenderWithoutProviderConfigured(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)
	uc := New(rest, nil, NewMemoryCache(time.Hour, DefaultCacheMaxBytes), quietLogger())

	if uc.Configured() {
		t.Error("Configured() = true without a provider")
	}
	_, err := uc.Render(context.Background(), id, defaultParams(t))
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeMapNotConfigured {
		t.Errorf("code = %q, want %q", code, domain.CodeMapNotConfigured)
	}
	// Unconfigured must be answered before any lookup — the answer is the same
	// for every venue, so there is nothing to look up.
	if n := rest.calls.Load(); n != 0 {
		t.Errorf("repository calls = %d, want 0", n)
	}
}

func TestRenderUnknownRestaurant(t *testing.T) {
	rest := newFakeRestaurants()
	prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
	uc := newUC(t, rest, prov)

	_, err := uc.Render(context.Background(), uuid.New(), defaultParams(t))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if n := prov.calls.Load(); n != 0 {
		t.Errorf("provider calls = %d, want 0 — an unknown id must never cost a provider request", n)
	}
}

func TestRenderRestaurantWithoutUsableCoordinates(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon *float64
	}{
		{"both null", nil, nil},
		{"latitude null", nil, ptr(71.4)},
		{"longitude null", ptr(51.1), nil},
		{"latitude out of range", ptr(120.0), ptr(71.4)},
		{"longitude out of range", ptr(51.1), ptr(-200.0)},
		{"null island", ptr(0.0), ptr(0.0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			rest := newFakeRestaurants()
			rest.add(id, tc.lat, tc.lon, true)
			prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
			uc := newUC(t, rest, prov)

			_, err := uc.Render(context.Background(), id, defaultParams(t))
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
			if code, _ := domain.CodeOf(err); code != domain.CodeMapNoCoordinates {
				t.Errorf("code = %q, want %q — the app must be able to tell this from an unknown id",
					code, domain.CodeMapNoCoordinates)
			}
			if n := prov.calls.Load(); n != 0 {
				t.Errorf("provider calls = %d, want 0", n)
			}
		})
	}
}

// A hidden venue must not become discoverable through the map endpoint.
func TestRenderInactiveRestaurantIsNotFound(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), false)
	prov := &fakeProvider{img: Image{Bytes: pngBytes, ContentType: "image/png"}}
	uc := newUC(t, rest, prov)

	_, err := uc.Render(context.Background(), id, defaultParams(t))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if n := prov.calls.Load(); n != 0 {
		t.Errorf("provider calls = %d, want 0", n)
	}
}

// The cold-cache stampede: N simultaneous first-openings of the same venue must
// cost exactly ONE provider request, because every one of them is billed.
func TestRenderConcurrentColdCacheCallsProviderOnce(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)

	release := make(chan struct{})
	// hold every caller inside the provider until they have all arrived
	prov := &fakeProvider{render: blockingRender(make(chan struct{}, 1), release,
		Image{Bytes: pngBytes, ContentType: "image/png"})}
	uc := newUC(t, rest, prov)
	p := defaultParams(t)

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = uc.Render(context.Background(), id, p)
		}(i)
	}
	// Give the goroutines a moment to pile up on the same key, then let the
	// single in-flight provider call finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Render #%d: %v", i, err)
		}
	}
	if got := prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1 — %d concurrent cold-cache requests must collapse into one paid render", got, n)
	}
}

// The guest who happened to start the shared render must not be able to take it
// down for everybody else by navigating away. On mobile networks a cancelled
// request is routine, so if the leader's context reached the provider call, a
// single guest closing a screen would fail every other guest waiting on the
// same venue.
func TestRenderLeaderCancellationDoesNotFailTheFollowers(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	prov := &fakeProvider{render: blockingRender(entered, release,
		Image{Bytes: pngBytes, ContentType: "image/png"})}
	uc := newUC(t, rest, prov)
	p := defaultParams(t)

	// The leader: starts the render, then its request context is cancelled.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := uc.Render(leaderCtx, id, p)
		leaderErr <- err
	}()
	<-entered // the shared render is now in flight, owned by the leader

	// The followers pile onto the same key with their own, healthy contexts.
	const followers = 5
	var wg sync.WaitGroup
	imgs := make([]Image, followers)
	errs := make([]error, followers)
	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			imgs[i], errs[i] = uc.Render(context.Background(), id, p)
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let the followers reach the wait

	cancelLeader() // guest A navigates away
	<-leaderErr    // A's own call returns promptly, that is its right
	close(release) // the provider finally answers
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("follower #%d got %v — the leader's cancellation killed a healthy caller's render", i, err)
		}
		if string(imgs[i].Bytes) != string(pngBytes) {
			t.Errorf("follower #%d got no image", i)
		}
	}
	if got := prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1", got)
	}
}

// A caller that goes away must get a prompt, non-internal answer for itself —
// and must never be reported as a 500.
func TestRenderCallerCancellationIsNotAnInternalError(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	prov := &fakeProvider{render: blockingRender(entered, release,
		Image{Bytes: pngBytes, ContentType: "image/png"})}
	uc := newUC(t, rest, prov)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := uc.Render(ctx, id, defaultParams(t))
		done <- err
	}()
	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("error = %v, want domain.ErrUnavailable (503), never an internal error", err)
		}
		if code, _ := domain.CodeOf(err); code != domain.CodeMapProviderUnavailable {
			t.Errorf("code = %q, want %q", code, domain.CodeMapProviderUnavailable)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Render did not return after its caller's context was cancelled")
	}
}

// The render was already paid for; if every original caller walked away, the
// picture must still end up in the cache instead of being thrown out and
// re-bought on the next request.
func TestRenderAbandonedWorkStillPopulatesTheCache(t *testing.T) {
	id := uuid.New()
	rest := newFakeRestaurants()
	rest.add(id, ptr(51.1), ptr(71.4), true)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	prov := &fakeProvider{render: blockingRender(entered, release,
		Image{Bytes: pngBytes, ContentType: "image/png"})}
	uc := newUC(t, rest, prov)
	p := defaultParams(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = uc.Render(ctx, id, p) }()
	<-entered
	cancel()
	close(release)

	// Wait for the detached render to finish and write through to the cache.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := uc.cache.Get(uc.cacheKey(id, p)); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := uc.Render(context.Background(), id, p); err != nil {
		t.Fatalf("Render after the abandoned one: %v", err)
	}
	if got := prov.calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want 1 — the abandoned render was paid for and must not be re-bought", got)
	}
}

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestInMemoryLimiter_AllowsUpToLimitThenRejects checks the core token-bucket
// behaviour in isolation, no HTTP involved: exactly `limit` requests succeed
// inside the window, the next one is rejected with a positive retryAfter.
func TestInMemoryLimiter_AllowsUpToLimitThenRejects(t *testing.T) {
	l := NewInMemoryLimiter(time.Minute, time.Minute)

	const limit = 3
	window := 100 * time.Millisecond

	for i := 0; i < limit; i++ {
		allowed, retryAfter := l.Allow("k", limit, window)
		if !allowed {
			t.Fatalf("request %d: want allowed, got rejected (retryAfter=%v)", i, retryAfter)
		}
	}

	allowed, retryAfter := l.Allow("k", limit, window)
	if allowed {
		t.Fatal("request beyond limit: want rejected, got allowed")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter = %v, want > 0", retryAfter)
	}
}

// TestInMemoryLimiter_RecoversAfterWindow checks that once enough of the
// window has elapsed for the bucket to refill a token, the next request is
// allowed again — i.e. the limit is a rolling budget, not a permanent ban.
func TestInMemoryLimiter_RecoversAfterWindow(t *testing.T) {
	l := NewInMemoryLimiter(time.Minute, time.Minute)

	const limit = 1
	window := 50 * time.Millisecond

	if allowed, _ := l.Allow("k", limit, window); !allowed {
		t.Fatal("first request should be allowed")
	}
	if allowed, _ := l.Allow("k", limit, window); allowed {
		t.Fatal("second immediate request should be rejected")
	}

	time.Sleep(window + 20*time.Millisecond)

	if allowed, _ := l.Allow("k", limit, window); !allowed {
		t.Fatal("request after the window elapsed should be allowed again")
	}
}

// TestInMemoryLimiter_KeysAreIndependent checks that two distinct keys (e.g.
// two different client IPs) get their own independent budget.
func TestInMemoryLimiter_KeysAreIndependent(t *testing.T) {
	l := NewInMemoryLimiter(time.Minute, time.Minute)

	if allowed, _ := l.Allow("ip-a", 1, time.Minute); !allowed {
		t.Fatal("ip-a first request should be allowed")
	}
	if allowed, _ := l.Allow("ip-a", 1, time.Minute); allowed {
		t.Fatal("ip-a second request should be rejected")
	}
	if allowed, _ := l.Allow("ip-b", 1, time.Minute); !allowed {
		t.Fatal("ip-b (a different key) should not be affected by ip-a's budget")
	}
}

// TestInMemoryLimiter_ZeroLimitDisables checks the documented escape hatch: a
// non-positive limit or window always allows.
func TestInMemoryLimiter_ZeroLimitDisables(t *testing.T) {
	l := NewInMemoryLimiter(time.Minute, time.Minute)
	for i := 0; i < 100; i++ {
		if allowed, _ := l.Allow("k", 0, time.Minute); !allowed {
			t.Fatal("limit=0 should always allow")
		}
	}
}

// TestInMemoryLimiter_SweepEvictsIdleBuckets checks the memory bound: a
// bucket untouched for longer than idleTTL is evicted on the next sweep, so
// the map does not grow forever as new keys (e.g. new client IPs) appear.
func TestInMemoryLimiter_SweepEvictsIdleBuckets(t *testing.T) {
	l := NewInMemoryLimiter(30*time.Millisecond, 10*time.Millisecond)

	l.Allow("stale-key", 1, time.Minute)
	l.mu.Lock()
	if _, ok := l.buckets["stale-key"]; !ok {
		l.mu.Unlock()
		t.Fatal("bucket should exist right after first use")
	}
	l.mu.Unlock()

	// Let the key go idle past idleTTL, then trigger a sweep via any other
	// call once sweepEvery has also elapsed.
	time.Sleep(60 * time.Millisecond)
	l.Allow("other-key", 1, time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets["stale-key"]; ok {
		t.Error("idle bucket should have been evicted by the sweep")
	}
}

// TestInMemoryLimiter_ConcurrentSameKey exercises the concurrency case: many
// goroutines racing on the same key must never let more than `limit`
// requests through in aggregate before the window has had a chance to
// refill anything.
func TestInMemoryLimiter_ConcurrentSameKey(t *testing.T) {
	l := NewInMemoryLimiter(time.Minute, time.Minute)
	const limit = 20
	const workers = 100

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			allowed, _ := l.Allow("shared", limit, time.Hour)
			if allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowedCount != limit {
		t.Errorf("allowedCount = %d, want exactly %d (limit) out of %d concurrent callers", allowedCount, limit, workers)
	}
}

// fixedLimiter is a Limiter test double whose Allow always returns a fixed
// decision, used to test RateLimit's HTTP-facing behaviour (status code,
// headers, envelope) independently of InMemoryLimiter's own logic.
type fixedLimiter struct {
	allowed    bool
	retryAfter time.Duration
}

func (f fixedLimiter) Allow(string, int, time.Duration) (bool, time.Duration) {
	return f.allowed, f.retryAfter
}

func newRateLimitEngine(cfg RateLimitConfig, limiter Limiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(cfg, limiter))
	r.GET("/api/v1/restaurants", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/v1/auth/otp/request", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func strictBudget() RateLimitConfig {
	return RateLimitConfig{
		Enabled: true,
		Strict:  RateLimitBudget{Limit: 5, Window: time.Minute},
		Soft:    RateLimitBudget{Limit: 60, Window: time.Minute},
		Webhook: RateLimitBudget{Limit: 120, Window: time.Minute},
		Default: RateLimitBudget{Limit: 30, Window: time.Minute},
	}
}

func TestRateLimit_RejectsWith429AndRetryAfter(t *testing.T) {
	r := newRateLimitEngine(strictBudget(), fixedLimiter{allowed: false, retryAfter: 7 * time.Second})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want \"7\"", got)
	}
	if !bodyHasErrorEnvelope(rec.Body.String()) {
		t.Errorf("body = %q, want the standard error envelope", rec.Body.String())
	}
}

func TestRateLimit_AllowsWhenLimiterAllows(t *testing.T) {
	r := newRateLimitEngine(strictBudget(), fixedLimiter{allowed: true})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRateLimit_DisabledConfigNeverLimits(t *testing.T) {
	cfg := strictBudget()
	cfg.Enabled = false
	r := newRateLimitEngine(cfg, fixedLimiter{allowed: false, retryAfter: time.Second})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rate limiting disabled)", rec.Code)
	}
}

func TestRateLimit_HealthCheckIsExempt(t *testing.T) {
	r := newRateLimitEngine(strictBudget(), fixedLimiter{allowed: false, retryAfter: time.Second})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (health check is exempt from rate limiting)", rec.Code)
	}
}

// TestRateLimit_EndToEndWithInMemoryLimiter exercises RateLimit wired to the
// real InMemoryLimiter (not a fake), against a real gin engine: the
// sensitive-tier budget triggers after its limit, then recovers once the
// window elapses — the same two properties the task asked to be covered by a
// test.
func TestRateLimit_EndToEndWithInMemoryLimiter(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled: true,
		Strict:  RateLimitBudget{Limit: 2, Window: 80 * time.Millisecond},
		Soft:    RateLimitBudget{Limit: 60, Window: time.Minute},
		Webhook: RateLimitBudget{Limit: 120, Window: time.Minute},
		Default: RateLimitBudget{Limit: 30, Window: time.Minute},
	}
	limiter := NewInMemoryLimiter(time.Minute, time.Minute)
	r := newRateLimitEngine(cfg, limiter)

	get := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
		req.RemoteAddr = "203.0.113.9:12345"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(); code != http.StatusOK {
		t.Fatalf("request 1: status = %d, want 200", code)
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("request 2: status = %d, want 200", code)
	}
	if code := get(); code != http.StatusTooManyRequests {
		t.Fatalf("request 3 (over budget): status = %d, want 429", code)
	}

	time.Sleep(100 * time.Millisecond)

	if code := get(); code != http.StatusOK {
		t.Fatalf("request after window elapsed: status = %d, want 200", code)
	}
}

// TestRateLimit_DifferentIPsHaveIndependentBudgets checks that the bucket key
// includes the client IP: one IP being throttled must never affect another.
func TestRateLimit_DifferentIPsHaveIndependentBudgets(t *testing.T) {
	cfg := RateLimitConfig{
		Enabled: true,
		Strict:  RateLimitBudget{Limit: 1, Window: time.Minute},
		Soft:    RateLimitBudget{Limit: 60, Window: time.Minute},
		Webhook: RateLimitBudget{Limit: 120, Window: time.Minute},
		Default: RateLimitBudget{Limit: 30, Window: time.Minute},
	}
	limiter := NewInMemoryLimiter(time.Minute, time.Minute)
	r := newRateLimitEngine(cfg, limiter)

	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	req1.RemoteAddr = "203.0.113.1:1"
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("ip 1 first request: status = %d, want 200", rec1.Code)
	}

	req1b := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	req1b.RemoteAddr = "203.0.113.1:2"
	rec1b := httptest.NewRecorder()
	r.ServeHTTP(rec1b, req1b)
	if rec1b.Code != http.StatusTooManyRequests {
		t.Fatalf("ip 1 second request: status = %d, want 429", rec1b.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	req2.RemoteAddr = "198.51.100.7:1"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("ip 2 first request: status = %d, want 200 (independent budget from ip 1)", rec2.Code)
	}
}

func bodyHasErrorEnvelope(body string) bool {
	return strings.Contains(body, `"error"`) && strings.Contains(body, "too many requests")
}

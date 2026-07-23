package middleware

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/logging"
	"backend-core/internal/transport/rest/response"
)

// RateLimitTier classifies a route into a rate-limit policy bucket. The same
// budget cannot fit every route: it either under-serves public reads or
// over-serves a sensitive write.
type RateLimitTier string

const (
	// TierStrict is for public routes with a real side effect that costs
	// money or invites abuse when hammered: OTP send, booking/payment
	// creation, guest checkout settlement. This is the tier that exists
	// because of a concrete incident risk — an unattended script hitting an
	// unthrottled endpoint overnight, which becomes a real SMS bill the
	// moment real sending is wired up behind AUTH_OTP_DEV_EXPOSE=false.
	TierStrict RateLimitTier = "strict"
	// TierSoft is for public reads (listings, menus, availability): cheap to
	// serve, needs headroom rather than a wall.
	TierSoft RateLimitTier = "soft"
	// TierWebhook is the acquirer callback routes. Deliberately not the same
	// per-IP-and-strict profile as guest traffic — see RateLimit's doc.
	TierWebhook RateLimitTier = "webhook"
	// TierDefault is every authenticated route not explicitly classified
	// below: a moderate floor so a route added later without updating
	// routeTiers is never accidentally left completely unlimited.
	TierDefault RateLimitTier = "default"
)

// RateLimitBudget is one tier's token-bucket budget: at most Limit requests
// per Window, per bucket key. Limit <= 0 or Window <= 0 disables limiting for
// that tier (Limiter.Allow always reports allowed).
type RateLimitBudget struct {
	Limit  int
	Window time.Duration
}

// RateLimitConfig holds one budget per tier. See bootstrap.NewConfig for the
// env variables and defaults, and RateLimit's doc comment for which routes
// fall into which tier.
type RateLimitConfig struct {
	Enabled bool
	Strict  RateLimitBudget
	Soft    RateLimitBudget
	Webhook RateLimitBudget
	Default RateLimitBudget
}

func (cfg RateLimitConfig) budgetFor(tier RateLimitTier) RateLimitBudget {
	switch tier {
	case TierStrict:
		return cfg.Strict
	case TierSoft:
		return cfg.Soft
	case TierWebhook:
		return cfg.Webhook
	default:
		return cfg.Default
	}
}

// Limiter decides whether one more request identified by key may proceed
// right now under the given (limit, window) budget, and if not, how long the
// caller should wait before retrying. Implementations must be safe for
// concurrent use.
//
// InMemoryLimiter is the only implementation today — this app runs as a
// single instance, so an in-process map is enough and needs no extra
// infrastructure. A Redis- (or any shared-store-) backed Limiter for a
// multi-instance deployment is a drop-in replacement behind this same
// interface: nothing in RateLimit or its callers changes.
type Limiter interface {
	Allow(key string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration)
}

// bucketState is one token bucket: Tokens refill continuously at
// limit/window per second, capped at limit, and each allowed request spends
// one token.
type bucketState struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// InMemoryLimiter is a token-bucket Limiter keyed by an arbitrary string
// (RateLimit uses "<method> <route>|<client ip>"). Memory is bounded by
// opportunistic sweeps rather than a background goroutine: every call to
// Allow checks whether it has been longer than sweepEvery since the last
// sweep and, if so, evicts buckets untouched for longer than idleTTL while it
// already holds the map lock. This keeps the type's lifecycle trivial (no
// Start/Stop, nothing to leak in tests that build many short-lived
// instances) at the cost of a sweep occasionally running on the hot path
// instead of on its own timer — acceptable because a sweep is O(buckets) and
// runs at most once per sweepEvery, not once per request.
type InMemoryLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucketState
	idleTTL    time.Duration
	sweepEvery time.Duration
	lastSweep  time.Time
}

// NewInMemoryLimiter builds an InMemoryLimiter. idleTTL is how long a bucket
// may go untouched before it is evicted (bounds memory for a growing set of
// distinct client IPs); sweepEvery is the minimum spacing between eviction
// passes. Non-positive values fall back to sane defaults (10 minutes / 1
// minute) rather than disabling cleanup outright.
func NewInMemoryLimiter(idleTTL, sweepEvery time.Duration) *InMemoryLimiter {
	if idleTTL <= 0 {
		idleTTL = 10 * time.Minute
	}
	if sweepEvery <= 0 {
		sweepEvery = time.Minute
	}
	return &InMemoryLimiter{
		buckets:    make(map[string]*bucketState),
		idleTTL:    idleTTL,
		sweepEvery: sweepEvery,
		lastSweep:  time.Now(),
	}
}

// Allow implements Limiter.
func (l *InMemoryLimiter) Allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 || window <= 0 {
		return true, 0
	}

	now := time.Now()
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucketState{tokens: float64(limit), lastSeen: now}
		l.buckets[key] = b
	}
	if now.Sub(l.lastSweep) > l.sweepEvery {
		l.sweepLocked(now)
	}
	l.mu.Unlock()

	rate := float64(limit) / window.Seconds() // tokens per second

	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.lastSeen).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rate
		if max := float64(limit); b.tokens > max {
			b.tokens = max
		}
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	retryAfter := time.Duration(deficit/rate*float64(time.Second)) + time.Millisecond
	return false, retryAfter
}

// sweepLocked evicts buckets idle for longer than idleTTL. Caller must hold
// l.mu.
func (l *InMemoryLimiter) sweepLocked(now time.Time) {
	cutoff := now.Add(-l.idleTTL)
	for k, b := range l.buckets {
		b.mu.Lock()
		stale := b.lastSeen.Before(cutoff)
		b.mu.Unlock()
		if stale {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// exemptRoutes are never rate-limited: liveness/readiness probes and the JWKS
// endpoint are polled on a fixed schedule by the orchestrator/other services,
// not user traffic — throttling them would turn a health check into a false
// outage signal.
var exemptRoutes = map[string]bool{
	"GET /health":                true,
	"GET /health/ready":          true,
	"GET /.well-known/jwks.json": true,
}

// routeTiers classifies every route this API registers (see bootstrap.NewApp)
// into a rate-limit tier. Anything not listed here — and not in
// exemptRoutes — falls back to TierDefault, so a route added later is never
// silently unlimited just because nobody updated this table.
//
// Keys are "<method> <gin route template>", matching the exact strings
// gin.Context.FullPath() returns for that route (":id" placeholders, not
// resolved ids) so one entry covers every request to that endpoint
// regardless of the concrete id in the URL.
var routeTiers = map[string]RateLimitTier{
	// Public sensitive: booking/OTP/payment/checkout creation. See TierStrict's doc.
	"POST /api/v1/auth/signup":                 TierStrict,
	"POST /api/v1/auth/login":                  TierStrict,
	"POST /api/v1/auth/otp/request":            TierStrict,
	"POST /api/v1/auth/otp/verify":             TierStrict,
	"POST /api/v1/partnership-requests":        TierStrict,
	"POST /api/v1/bookings":                    TierStrict,
	"POST /api/v1/bookings/:id/payment":        TierStrict,
	"POST /api/v1/bookings/:id/payment/settle": TierStrict,

	// Public reading: browsing traffic.
	"GET /api/v1/restaurants":                  TierSoft,
	"GET /api/v1/restaurants/:id":              TierSoft,
	"GET /api/v1/restaurant-categories":        TierSoft,
	"GET /api/v1/restaurants/:id/menu":         TierSoft,
	"GET /api/v1/menu-categories":              TierSoft,
	"GET /api/v1/restaurants/:id/availability": TierSoft,
	"GET /api/v1/bookings/:id/payment":         TierSoft,
	"GET /api/v1/payments/:id":                 TierSoft,

	// Acquirer webhooks — see RateLimit's doc for why this is its own tier.
	"POST /webhooks/payments/freedompay":      TierWebhook,
	"POST /webhooks/payments/tiptoppay/:type": TierWebhook,
}

// RateLimit enforces a per-client-IP request budget, classified by route into
// one of four tiers (see RateLimitTier's doc comments and routeTiers).
//
// # Why webhooks are not just "strict, by IP"
//
// A bank/acquirer's callback legitimately retries the same event on a
// timeout or a non-2xx response, from a small, acquirer-controlled set of
// source addresses this codebase does not have a verified, current list of
// (TODO(verify): confirm FreedomPay's and TipTopPay's outbound webhook IP
// ranges against their sandbox/ops docs before ever trying to allowlist by
// IP instead of rate limiting). Rate limiting webhooks as tightly as a
// public form would turn an acquirer's own retry-on-failure behaviour into
// permanently lost payment confirmations — worse than the abuse it
// prevents. Keying by the acquirer's event id instead of IP was considered
// and rejected: it needs parsing an unauthenticated body before authenticity
// is verified (WebhookUseCase verifies the signature first), which is its
// own attack surface. The compromise: TierWebhook keeps per-IP keying (a
// stray flood is still bounded) but ships with a budget wide enough that no
// legitimate retry burst is expected to ever hit it (see bootstrap.NewConfig
// defaults).
//
// # IP resolution
//
// The bucket key uses gin's c.ClientIP(), which only reads
// X-Forwarded-For/X-Real-IP when the immediate TCP peer is one of Engine's
// configured trusted proxies (see bootstrap.NewApp's SetTrustedProxies call,
// APP_TRUSTED_PROXIES) — an arbitrary caller cannot mint itself a fresh rate
// limit identity per request just by sending its own X-Forwarded-For.
//
// # Response on rejection
//
// 429 in the same Envelope every other error uses (response.Error), with a
// Retry-After header in whole seconds, rounded up. Never reveals the tier
// name or the bucket's internal state to the client.
//
// Register this after CORS (so a rejected preflight-adjacent request still
// carries CORS headers) and before the route groups.
func RateLimit(cfg RateLimitConfig, limiter Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !cfg.Enabled {
			c.Next()
			return
		}

		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path // unmatched route (404) — still worth bounding
		}
		routeKey := c.Request.Method + " " + route
		if exemptRoutes[routeKey] {
			c.Next()
			return
		}

		tier, ok := routeTiers[routeKey]
		if !ok {
			tier = TierDefault
		}
		budget := cfg.budgetFor(tier)
		ip := c.ClientIP()
		bucketKey := routeKey + "|" + ip

		allowed, retryAfter := limiter.Allow(bucketKey, budget.Limit, budget.Window)
		if allowed {
			c.Next()
			return
		}

		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		c.Writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		// Logged only on the 429 path (once per rejection, not per request),
		// so a sustained flood produces one alertable line per rejected
		// request rather than drowning the log with allowed traffic too.
		logging.FromContext(c.Request.Context()).Warn("http.rate_limited",
			slog.String("tier", string(tier)),
			slog.String("route", route),
			slog.String("ip", ip),
		)
		response.Error(c.Writer, http.StatusTooManyRequests, "too many requests")
		c.Abort()
	}
}

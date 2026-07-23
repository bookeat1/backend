// Package payment holds the acquirer adapters (subpackages freedompay,
// tiptoppay), the provider registry and the small HTTP client they share.
//
// It implements domain.PaymentGateway and depends on `domain` only. Nothing in
// here is imported by the domain: adding a third acquirer is a new subpackage
// plus one line in bootstrap, never a domain change (spec §2).
//
// Two rules apply to every file below and are enforced by tests:
//
//   - credentials come from environment variables only, never from the database
//     and never from a request (spec §8);
//   - no secret, signature, card number or cryptogram ever reaches a log line
//     or an error message.
package payment

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	"backend-core/internal/domain"
)

// maxResponseBytes caps what we read from an acquirer. A payment API answers in
// kilobytes; anything larger is a malfunction or an attack and must not be able
// to exhaust our memory.
const maxResponseBytes = 1 << 20

// Errors returned by the shared client. They wrap domain sentinels so the
// transport layer keeps mapping them without knowing about acquirers.
var (
	// ErrProviderUnavailable is a network failure, a timeout or a 5xx that
	// survived every retry. The payment status is UNKNOWN after this — the
	// caller must reconcile, never assume "not charged". It wraps
	// domain.ErrProviderOutcomeUnknown so usecase/payments (which never
	// imports this package) can recognise "do not retry the money-moving call
	// blindly" without a type from here (report item #1).
	ErrProviderUnavailable = fmt.Errorf("payment provider unavailable: %w", domain.ErrProviderOutcomeUnknown)
	// ErrProviderRejected is a well-formed answer that says no (4xx, or a
	// provider-level error envelope) — a DEFINITE decline, not an unknown
	// outcome. Wraps both domain.ErrValidation (existing HTTP-mapping
	// behaviour) and domain.ErrProviderDeclined (report item #1).
	ErrProviderRejected = fmt.Errorf("payment provider rejected the request: %w: %w", domain.ErrValidation, domain.ErrProviderDeclined)
	// ErrProviderMalformed is an answer we could not parse. Wraps
	// domain.ErrProviderOutcomeUnknown: we sent the request, we simply could
	// not read the confirmation, so whether money moved is unknown.
	ErrProviderMalformed = fmt.Errorf("payment provider returned a malformed response: %w", domain.ErrProviderOutcomeUnknown)
)

// Config tunes the shared HTTP client. Zero fields fall back to DefaultConfig.
type Config struct {
	// Timeout is the budget for ONE attempt, not for the whole call.
	Timeout time.Duration
	// MaxAttempts includes the first try (1 = no retries).
	MaxAttempts int
	// BaseBackoff is the first pause; it doubles per attempt up to MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultConfig is deliberately conservative: an acquirer that has not answered
// in 15 seconds is not going to, and three attempts spread over ~2 seconds is
// enough to ride out a blip without keeping a guest waiting.
func DefaultConfig() Config {
	return Config{
		Timeout:     15 * time.Second,
		MaxAttempts: 3,
		BaseBackoff: 300 * time.Millisecond,
		MaxBackoff:  3 * time.Second,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Timeout <= 0 {
		c.Timeout = d.Timeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = d.BaseBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = d.MaxBackoff
	}
	return c
}

// Doer is the minimal http.Client surface the adapters need. Satisfied by
// *http.Client, and by an httptest server's client in tests.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a retrying HTTP client shared by the acquirer adapters.
//
// It retries ONLY calls the adapter marked idempotent. That flag is not a
// guess: every request we send carries an idempotency key (TipTopPay
// X-Request-ID, FreedomPay pg_idempotency_key / a deterministic key derived
// from the operation), so a retried request resolves to the same money movement
// instead of a second one (spec §8).
type Client struct {
	doer   Doer
	cfg    Config
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(d time.Duration) time.Duration
}

// Option customises a Client. Used by tests to make backoff instant.
type Option func(*Client)

// WithSleep replaces the backoff sleep. Tests use it to avoid real waiting.
func WithSleep(fn func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = fn }
}

// WithJitter replaces the backoff jitter, making retry timing deterministic.
func WithJitter(fn func(d time.Duration) time.Duration) Option {
	return func(c *Client) { c.jitter = fn }
}

// NewClient builds a Client. doer may be nil, in which case a plain
// *http.Client is used; the per-attempt deadline comes from the context, not
// from http.Client.Timeout, so that a caller's cancellation always wins.
func NewClient(doer Doer, cfg Config, log *slog.Logger, opts ...Option) *Client {
	if doer == nil {
		doer = &http.Client{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		doer:   doer,
		cfg:    cfg.withDefaults(),
		log:    log,
		sleep:  sleepCtx,
		jitter: defaultJitter,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Request is one call to an acquirer. Provider and Op are for logs only; the
// URL and the body are never logged, because a FreedomPay body carries pg_sig
// and a TipTopPay header carries the API secret.
type Request struct {
	Provider domain.PaymentProvider
	Op       string
	Method   string
	URL      string
	Header   http.Header
	Body     []byte
	// Idempotent says the call may be retried. Set it only when the request
	// carries an idempotency key the provider honours.
	Idempotent bool
}

// Response is a fully read acquirer answer.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Do performs the request, retrying idempotent calls on transport failures,
// 429 and 5xx with exponential backoff. The context is honoured both for the
// per-attempt deadline and for the pause between attempts.
//
// The returned error never contains the URL, the body or any header: see
// TestErrorsCarryNoSecrets.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	attempts := 1
	if req.Idempotent {
		attempts = c.cfg.MaxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := c.attempt(ctx, req)
		switch {
		case err == nil && !retryableStatus(resp.StatusCode):
			return resp, nil
		case err == nil:
			lastErr = fmt.Errorf("%w: %s returned HTTP %d", ErrProviderUnavailable, req.Op, resp.StatusCode)
		case ctx.Err() != nil:
			// The caller gave up (or the whole call ran out of time). Retrying
			// cannot help and would hide the cancellation.
			return nil, fmt.Errorf("%w: %s: %w", ErrProviderUnavailable, req.Op, ctx.Err())
		default:
			lastErr = fmt.Errorf("%w: %s: transport failure", ErrProviderUnavailable, req.Op)
		}

		c.log.Warn("payment provider call failed",
			slog.String("provider", string(req.Provider)),
			slog.String("op", req.Op),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", attempts),
			slog.Bool("will_retry", attempt < attempts),
		)

		if attempt == attempts {
			break
		}
		if err := c.sleep(ctx, c.backoff(attempt)); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrProviderUnavailable, req.Op, err)
		}
	}
	return nil, lastErr
}

func (c *Client) attempt(ctx context.Context, req Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, body)
	if err != nil {
		// A bad URL is a programming error, not a provider outage; still, the
		// URL itself is not echoed — it may carry query parameters.
		return nil, fmt.Errorf("build %s request", req.Op)
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := c.doer.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do %s request", req.Op)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s response", req.Op)
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: raw}, nil
}

// backoff returns the pause before the attempt following n (1-based).
func (c *Client) backoff(n int) time.Duration {
	shift := n - 1
	if shift > 20 { // guard against overflow on absurd MaxAttempts
		shift = 20
	}
	d := time.Duration(math.Min(
		float64(c.cfg.BaseBackoff)*math.Pow(2, float64(shift)),
		float64(c.cfg.MaxBackoff),
	))
	return c.jitter(d)
}

// retryableStatus reports whether an HTTP status is worth another attempt.
// 429 and 5xx are transient; a 4xx is our fault and will not get better.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// defaultJitter spreads retries over [50%, 100%] of d so that a fleet of
// instances does not hammer a recovering acquirer in lockstep.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

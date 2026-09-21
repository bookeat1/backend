package kwaaka

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	"backend-core/internal/domain"
)

// maxResponseBytes caps what we read from Kwaaka. A menu answers in tens of
// kilobytes (the test store's was ~24 KB); anything wildly larger than that is
// a malfunction, not a bigger menu, and must not be able to exhaust memory.
const maxResponseBytes = 8 << 20

// Doer is the minimal http.Client surface the client needs. Satisfied by
// *http.Client and by an httptest server's client in tests.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a small retrying HTTP client for Kwaaka's Table Booking API.
// Every call this adapter makes today is a GET, which is safe to retry
// unconditionally on a transport failure, 429 or 5xx — there is no
// idempotency-key dance to do, unlike the payment acquirers.
type Client struct {
	doer  Doer
	cfg   Config
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient builds a Client. doer may be nil, in which case a plain
// *http.Client is used.
func NewClient(doer Doer, cfg Config) *Client {
	if doer == nil {
		doer = &http.Client{}
	}
	return &Client{doer: doer, cfg: cfg.withDefaults(), sleep: sleepCtx}
}

// get performs one authenticated GET against path (relative to
// cfg.BaseURL+cfg.PathPrefix), retrying transport failures, 429 and 5xx with
// exponential backoff up to cfg.MaxAttempts. It returns the raw response body
// on 200 and domain.ErrUnavailable (Kwaaka is an optional dependency) on
// anything else — the specific status/body is wrapped into the error message
// for logs.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	url := c.cfg.BaseURL + c.cfg.PathPrefix + path

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		body, status, err := c.attempt(ctx, url)
		switch {
		case err == nil && status == http.StatusOK:
			return body, nil
		case err == nil && !retryableStatus(status):
			return nil, fmt.Errorf("%w: kwaaka returned HTTP %d: %s", domain.ErrUnavailable, status, truncate(body))
		case err == nil:
			lastErr = fmt.Errorf("%w: kwaaka returned HTTP %d", domain.ErrUnavailable, status)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("%w: %w", domain.ErrUnavailable, ctx.Err())
		default:
			lastErr = fmt.Errorf("%w: transport failure: %w", domain.ErrUnavailable, err)
		}
		if attempt == c.cfg.MaxAttempts {
			break
		}
		if err := c.sleep(ctx, backoff(attempt)); err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
		}
	}
	return nil, lastErr
}

func (c *Client) attempt(ctx context.Context, url string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	// ApiKeyAuth: the token goes as-is into Authorization, no "Bearer " prefix
	// — confirmed against the spec's securitySchemes and by a live call.
	req.Header.Set("Authorization", c.cfg.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.doer.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return raw, resp.StatusCode, nil
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func backoff(attempt int) time.Duration {
	const base = 300 * time.Millisecond
	const max = 3 * time.Second
	shift := attempt - 1
	if shift > 10 {
		shift = 10
	}
	d := time.Duration(math.Min(float64(base)*math.Pow(2, float64(shift)), float64(max)))
	half := d / 2
	if half <= 0 {
		return 0
	}
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

func truncate(b []byte) string {
	const max = 300
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

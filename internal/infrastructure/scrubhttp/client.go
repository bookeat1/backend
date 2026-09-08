// Package scrubhttp is the shared outbound HTTP client for third-party
// provider adapters. It exists for ONE guarantee that must hold identically in
// every adapter that uses it: a credential can never reach an error string, and
// therefore never a log line.
//
// It was extracted from internal/infrastructure/otpsender (where it served the
// three OTP channels) when the WhatsApp Cloud API gained a SECOND caller — the
// venue booking-alert channel. Two copies of "post to Meta and scrub the token
// out of the error" is exactly the kind of duplication where one copy silently
// stops scrubbing.
package scrubhttp

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxBody caps how much of a provider's answer we read. Every provider behind
// this client replies with a small JSON object; anything larger is a proxy
// error page or a hostile response, and reading it in full would let a third
// party decide how much memory one of our requests costs.
const MaxBody = 64 << 10 // 64 KiB

// DefaultTimeout is used when a caller passes a non-positive timeout. A missing
// timeout on an outbound call is how a worker loop stops forever.
const DefaultTimeout = 5 * time.Second

// Client is an *http.Client that knows which strings are secrets. Tokens travel
// in headers and (for Twilio) in the URL's userinfo, and a raw *url.Error from
// net/http embeds the URL — so every error leaves this type scrubbed.
type Client struct {
	client  *http.Client
	secrets []string
}

// NewClient builds the client. Every secret that can appear in a URL, a header
// or a provider's echo must be passed here.
func NewClient(timeout time.Duration, secrets ...string) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	kept := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return &Client{client: &http.Client{Timeout: timeout}, secrets: kept}
}

// Trimmed is the one place adapters agree on what "an absent value" means:
// whitespace around a token pasted from a dashboard is absence, not a token.
func Trimmed(s string) string { return strings.TrimSpace(s) }

// Do performs the request and returns the status code and the (capped) body.
// A transport failure comes back scrubbed; the caller decides what a given
// status means for its provider.
func (c *Client) Do(req *http.Request) (int, []byte, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, c.Scrub(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return resp.StatusCode, nil, c.Scrub(err)
	}
	return resp.StatusCode, body, nil
}

// Scrub replaces every configured secret anywhere in an error message with
// "***". Exported so an adapter can put its own late-built error through the
// same filter.
func (c *Client) Scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	changed := false
	for _, s := range c.secrets {
		if strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "***")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

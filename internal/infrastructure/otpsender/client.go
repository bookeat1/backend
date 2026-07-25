package otpsender

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxProviderBody caps how much of a provider's answer we read. Every provider
// here replies with a small JSON object; anything larger is a proxy error page
// or a hostile response, and reading it in full would let a third party decide
// how much memory a login request costs.
const maxProviderBody = 64 << 10 // 64 KiB

// httpClient is the shared transport for the OTP provider adapters. It exists
// for one guarantee that must hold identically in all three: a credential can
// never reach an error string, and therefore never a log line. Tokens travel in
// headers and (for Twilio) in the URL's userinfo, and a raw *url.Error from
// net/http embeds the URL — so every error leaves this type scrubbed.
type httpClient struct {
	client  *http.Client
	secrets []string
}

func newHTTPClient(timeout time.Duration, secrets ...string) *httpClient {
	if timeout <= 0 {
		timeout = defaultChannelTimeout
	}
	kept := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return &httpClient{client: &http.Client{Timeout: timeout}, secrets: kept}
}

// trimmed is the one place the adapters agree on what "an absent value" means:
// whitespace around a token pasted from a dashboard is absence, not a token.
func trimmed(s string) string { return strings.TrimSpace(s) }

// do performs the request and returns the status code and the (capped) body.
// A transport failure comes back scrubbed; the caller decides what a given
// status means for its provider.
func (c *httpClient) do(req *http.Request) (int, []byte, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, c.scrub(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return resp.StatusCode, nil, c.scrub(err)
	}
	return resp.StatusCode, body, nil
}

// scrub replaces every configured secret anywhere in an error message with
// "***". Belt and braces with the same discipline as telegramnotify.Sender.
func (c *httpClient) scrub(err error) error {
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

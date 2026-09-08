package otpsender

import (
	"time"

	"backend-core/internal/infrastructure/scrubhttp"
)

// The shared, secret-scrubbing transport these adapters run on now lives in
// internal/infrastructure/scrubhttp — it gained a second family of callers when
// the WhatsApp Cloud API started carrying venue booking alerts as well as login
// codes. The aliases below keep the OTP adapters reading exactly as before; the
// guarantee (a credential never reaches an error string) is unchanged and is
// now enforced in one place instead of two.
type httpClient = scrubhttp.Client

func newHTTPClient(timeout time.Duration, secrets ...string) *httpClient {
	if timeout <= 0 {
		timeout = defaultChannelTimeout
	}
	return scrubhttp.NewClient(timeout, secrets...)
}

// trimmed is the one place the adapters agree on what "an absent value" means:
// whitespace around a token pasted from a dashboard is absence, not a token.
func trimmed(s string) string { return scrubhttp.Trimmed(s) }

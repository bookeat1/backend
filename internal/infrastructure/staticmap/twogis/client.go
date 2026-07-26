// Package twogis implements staticmap.Provider against 2GIS's Static API.
//
// 2GIS is the chosen provider because its data for Kazakhstan (buildings,
// entrances, venue names) is materially better than Google's. The request shape
// implemented here follows docs.2gis.com/en/maps/others/static/reference:
//
//	GET https://static.maps.2gis.com/2.0?s={W}x{H}[@2x]&z={zoom}&c={lat},{lon}&pt={lat},{lon}~k:p~c:rd~s:s&key={API_KEY}
//
// with the documented limits: width 120–1280, height 90–1280, zoom 1–18,
// coordinates as "latitude,longitude". The response is a PNG on success.
//
// The API key is a credential: it is read from the environment, appears only in
// the outgoing query string, and is never logged and never included in any
// error this package returns. That is why transport failures are converted to
// bare sentinels instead of being wrapped — Go's *url.Error stringifies the
// full request URL, key and all.
package twogis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backend-core/internal/usecase/staticmap"
)

// DefaultBaseURL is 2GIS's Static API entry point.
const DefaultBaseURL = "https://static.maps.2gis.com/2.0"

// Defaults for the knobs an operator usually leaves alone.
const (
	defaultTimeout  = 5 * time.Second
	defaultMaxBytes = int64(4 << 20) // 4 MiB — a 1280x1280@2x PNG is far below this
)

// marker is the pin drawn on the venue itself: a small red pin
// ("~k:p~c:rd~s:s" in 2GIS's point-options syntax). Fixed, never
// caller-controlled — a client-settable marker would be one more free-form
// value reaching the provider for nothing.
const markerOptions = "~k:p~c:rd~s:s"

// Config configures the 2GIS static map client.
type Config struct {
	// APIKey is the 2GIS Platform Manager access key. A credential: env only
	// (STATIC_MAP_2GIS_API_KEY), never logged. Empty = provider not configured,
	// which is a supported state (see Configured).
	APIKey string
	// BaseURL overrides the entry point (tests, staging).
	BaseURL string
	// Timeout caps one render call end to end.
	Timeout time.Duration
	// MaxBytes caps how much of the provider's response body we will read.
	MaxBytes int64
}

// Configured reports whether a key is present. Without one the provider must
// not be constructed at all — the usecase then answers map_not_configured.
func (c Config) Configured() bool { return strings.TrimSpace(c.APIKey) != "" }

// Client renders static maps through 2GIS.
type Client struct {
	baseURL  string
	apiKey   string
	maxBytes int64
	http     *http.Client
}

// NewClient builds the 2GIS static map client.
func NewClient(cfg Config) *Client {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return &Client{
		baseURL:  strings.TrimRight(base, "/"),
		apiKey:   strings.TrimSpace(cfg.APIKey),
		maxBytes: maxBytes,
		http:     &http.Client{Timeout: timeout},
	}
}

// Name implements staticmap.Provider.
func (c *Client) Name() string { return "2gis" }

// Render fetches one static map image.
//
// TODO(verify): проверить на реальном ключе 2GIS, как только он появится —
// (1) что центр `c=` действительно принимает "широта,долгота" в этом порядке
// (в документации Static API указано именно так, но у других API 2GIS
// встречается обратный порядок lon,lat); (2) какой HTTP-код и тело приходят
// при исчерпании лимита тарифа — здесь предполагается 429, и если 2GIS вместо
// этого отвечает 200 с текстом ошибки, этот случай уже попадает в
// ErrProviderUnavailable (см. проверку Content-Type), но код для клиента будет
// map_provider_unavailable, а не map_provider_rate_limited.
func (c *Client) Render(ctx context.Context, req staticmap.RenderRequest) (staticmap.Image, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(req), nil)
	if err != nil {
		// Cannot carry the URL: NewRequest's error text may quote it.
		return staticmap.Image{}, fmt.Errorf("2gis static map: build request: %w", staticmap.ErrProviderUnavailable)
	}
	httpReq.Header.Set("Accept", "image/png,image/*")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Deliberately NOT wrapping err: *url.Error stringifies the full URL,
		// which contains the API key. A timeout and a refused connection are the
		// same thing to the caller anyway — transient, retry later.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return staticmap.Image{}, fmt.Errorf("2gis static map: request timed out: %w", staticmap.ErrProviderUnavailable)
		}
		return staticmap.Image{}, fmt.Errorf("2gis static map: request failed: %w", staticmap.ErrProviderUnavailable)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return staticmap.Image{}, fmt.Errorf("2gis static map: status 429: %w", staticmap.ErrProviderRateLimited)
	case resp.StatusCode >= 500:
		return staticmap.Image{}, fmt.Errorf("2gis static map: status %d: %w", resp.StatusCode, staticmap.ErrProviderUnavailable)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// 400/401/403: a bad parameter or a bad/revoked key. Definite refusal —
		// the status is reported, the body is NOT echoed (it may quote the
		// request back at us, key included).
		return staticmap.Image{}, fmt.Errorf("2gis static map: status %d: %w", resp.StatusCode, staticmap.ErrProviderRejected)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/") {
		// 2GIS documents that a request can come back as "an error describing
		// the problem" rather than a picture. Whatever that is, it is not
		// something the app can render.
		return staticmap.Image{}, fmt.Errorf("2gis static map: non-image response: %w", staticmap.ErrProviderUnavailable)
	}

	// Bounded read: an unbounded ReadAll against a misbehaving endpoint is a
	// memory hazard, and a body larger than the cap cannot be a legitimate
	// render of a whitelisted size.
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return staticmap.Image{}, fmt.Errorf("2gis static map: read response: %w", staticmap.ErrProviderUnavailable)
	}
	if int64(len(body)) > c.maxBytes || len(body) == 0 {
		return staticmap.Image{}, fmt.Errorf("2gis static map: unusable image body (%d bytes): %w", len(body), staticmap.ErrProviderUnavailable)
	}

	return staticmap.Image{Bytes: body, ContentType: mediaType(ct)}, nil
}

// url builds the request URL.
//
// The query string is assembled by hand rather than through url.Values.Encode()
// on purpose: 2GIS's parameter syntax uses "@", "~" and ":" as structural
// characters ("880x450@2x", "55.1,71.4~k:p~c:rd~s:s"), and Encode percent-
// escapes ":" and "@". Hand-assembly keeps the literal syntax the docs
// specify. It is safe because every component is either a validated integer
// from a whitelist (see staticmap.ParseParams) or a float we format ourselves —
// no caller-supplied string ever reaches this string. The API key is the one
// value that could contain anything, so it IS escaped.
func (c *Client) url(req staticmap.RenderRequest) string {
	var b strings.Builder
	b.WriteString(c.baseURL)
	b.WriteString("?s=")
	b.WriteString(strconv.Itoa(req.Width))
	b.WriteString("x")
	b.WriteString(strconv.Itoa(req.Height))
	if req.Scale == 2 {
		b.WriteString("@2x")
	}
	b.WriteString("&z=")
	b.WriteString(strconv.Itoa(req.Zoom))
	center := coord(req.Lat) + "," + coord(req.Lon)
	b.WriteString("&c=")
	b.WriteString(center)
	b.WriteString("&pt=")
	b.WriteString(center)
	b.WriteString(markerOptions)
	b.WriteString("&key=")
	b.WriteString(escapeKey(c.apiKey))
	return b.String()
}

// coord formats a coordinate with six decimals (~11 cm) — more than enough for
// a venue pin and stable, so the same venue always produces the same URL.
func coord(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// escapeKey percent-escapes the API key for use in a query value. Written out
// instead of url.QueryEscape because that also escapes "+" as "%2B" and spaces
// as "+", both of which are correct here — but the point is to be explicit that
// this, and only this, value is untrusted-by-shape.
func escapeKey(k string) string {
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		ch := k[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", ch))
		}
	}
	return b.String()
}

// mediaType strips any parameters from a Content-Type header.
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

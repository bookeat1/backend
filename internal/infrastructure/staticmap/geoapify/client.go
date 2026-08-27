// Package geoapify implements staticmap.Provider against Geoapify's Static
// Maps API.
//
// Why Geoapify and not 2GIS today: we have no 2GIS key and no published 2GIS
// price list, while the map has to work now. Geoapify's Free plan gives 3 000
// credits/day, needs no credit card, and — unlike MapTiler's free plan, which
// is explicitly "for testing, personal or non-commercial use" — permits
// commercial use. Their pricing FAQ states it plainly: "Can I use the Free plan
// in commercial projects? Yes, we do not restrict that. However, you must
// provide an appropriate Geoapify attribution or link to our website."
// (https://www.geoapify.com/pricing/, checked 2026-08-27).
//
// ATTRIBUTION. The Free plan REQUIRES attribution, and this is not optional
// small print. The Static Maps API burns it into the picture itself: "By
// default, Static Maps API adds map style attributions into the right bottom
// corner of the map image. But you need to care about attributions yourself
// when you hide the automatically added attribution."
// (https://apidocs.geoapify.com/docs/maps/static/#attribution). So we do NOT
// hide it, and the app must not crop the bottom-right corner of the image —
// that is the whole compliance story, no extra layout element is needed.
// Consequently this client also never asks for a Font Awesome marker icon:
// those carry their own SIL OFL attribution requirement, and a plain pin costs
// us nothing.
//
// The request shape follows the API reference
// (https://apidocs.geoapify.com/docs/maps/static/#api-reference):
//
//	GET https://maps.geoapify.com/v1/staticmap?style={style}&width={W}&height={H}
//	    &format=png&center=lonlat:{lon},{lat}&zoom={z}[&scaleFactor=2]
//	    &marker=lonlat:{lon},{lat};type:material;color:%23{hex};size:{px}
//	    [&lang={lang}]&apiKey={API_KEY}
//
// Note the coordinate order: Geoapify is longitude,latitude — the OPPOSITE of
// 2GIS's latitude,longitude. width/height are the logical size (max 4096) and
// scaleFactor ∈ [1..2] multiplies the output pixels, which is exactly the
// meaning our RenderRequest.Scale already has. zoom ∈ [1..20] covers our 14–18
// whitelist.
//
// The API key is a credential: it is read from the environment, appears only in
// the outgoing query string, and is never logged and never included in any
// error this package returns. That is why transport failures are converted to
// bare sentinels instead of being wrapped — Go's *url.Error stringifies the
// full request URL, key and all. Same rule, same reasons, as the 2GIS client.
package geoapify

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

// DefaultBaseURL is Geoapify's Static Maps entry point.
const DefaultBaseURL = "https://maps.geoapify.com/v1/staticmap"

// Defaults for the knobs an operator usually leaves alone.
const (
	defaultTimeout  = 5 * time.Second
	defaultMaxBytes = int64(4 << 20) // 4 MiB — a 1280x720@2x PNG is far below this

	// DefaultStyle is Geoapify's OSM-based general-purpose style. Chosen over
	// "osm-carto" because the vector styles are the ones that support lang= and
	// styleCustomization, and because it reads well at our small preview sizes.
	DefaultStyle = "osm-bright-smooth"

	// DefaultLang labels the map in Russian. OpenStreetMap's default names in
	// Kazakhstan are Kazakh (verified on the raw OSM rendering of central
	// Almaty and Astana, 2026-08-27); the app's primary audience reads Russian.
	DefaultLang = "ru"
)

// marker is the pin drawn on the venue itself. Fixed, never caller-controlled —
// a client-settable marker would be one more free-form value reaching the
// provider for nothing.
//
// "type:material" with no icon is a plain teardrop pin, which is the closest
// equivalent of 2GIS's "~k:p~c:rd~s:s" (small red pin) and keeps us off Font
// Awesome's separate attribution requirement. The colour must arrive
// percent-encoded ("%23" = "#"), which is why it is written that way here: this
// literal goes into the query string verbatim.
const (
	markerType  = "material"
	markerColor = "%23e53935" // #e53935 — the same red as the 2GIS pin reads as
	markerSize  = 40          // total pin height in pixels
)

// Config configures the Geoapify static map client.
type Config struct {
	// APIKey is the Geoapify project API key. A credential: env only
	// (STATIC_MAP_GEOAPIFY_API_KEY), never logged. Empty = provider not
	// configured, which is a supported state (see Configured).
	APIKey string
	// BaseURL overrides the entry point (tests, staging).
	BaseURL string
	// Style is one of Geoapify's supported map styles. Empty = DefaultStyle.
	Style string
	// Lang labels the map. Empty = DefaultLang; "-" disables the parameter and
	// leaves the provider's own default (local names).
	Lang string
	// Timeout caps one render call end to end.
	Timeout time.Duration
	// MaxBytes caps how much of the provider's response body we will read.
	MaxBytes int64
}

// Configured reports whether a key is present. Without one the provider must
// not be constructed at all — the usecase then answers map_not_configured.
func (c Config) Configured() bool { return strings.TrimSpace(c.APIKey) != "" }

// Client renders static maps through Geoapify.
type Client struct {
	baseURL  string
	apiKey   string
	style    string
	lang     string
	maxBytes int64
	http     *http.Client
}

// NewClient builds the Geoapify static map client.
func NewClient(cfg Config) *Client {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	style := strings.TrimSpace(cfg.Style)
	if style == "" {
		style = DefaultStyle
	}
	lang := strings.TrimSpace(cfg.Lang)
	if lang == "" {
		lang = DefaultLang
	}
	if lang == "-" {
		lang = ""
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
		style:    style,
		lang:     lang,
		maxBytes: maxBytes,
		http: &http.Client{
			Timeout: timeout,
			// Never follow a redirect — identical reasoning to the 2GIS client:
			// the API key travels in the query string, and Go's default policy
			// would both re-send the request to whatever the 3xx points at and
			// attach a Referer header carrying the FULL previous URL (key
			// included) to a third party we never chose.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Name implements staticmap.Provider. It is part of the cache key, so switching
// providers invalidates the previous vendor's cached pictures.
func (c *Client) Name() string { return "geoapify" }

// Render fetches one static map image.
//
// TODO(verify): проверить на реальном ключе Geoapify — (1) как именно
// выглядит маркер "type:material" без icon (в документации icon необязателен,
// но живой картинки без ключа мы не видели); (2) какой HTTP-код приходит при
// исчерпании дневного лимита кредитов: здесь предполагается 429, а Geoapify в
// FAQ пишет, что ключи при превышении сразу не блокируют, — если вместо 429
// придёт 4xx, случай попадёт в ErrProviderRejected и будет виден в логах как
// «проверьте ключ/тариф».
func (c *Client) Render(ctx context.Context, req staticmap.RenderRequest) (staticmap.Image, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(req), nil)
	if err != nil {
		// Cannot carry the URL: NewRequest's error text may quote it.
		return staticmap.Image{}, fmt.Errorf("geoapify static map: build request: %w", staticmap.ErrProviderUnavailable)
	}
	httpReq.Header.Set("Accept", "image/png,image/*")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Deliberately NOT wrapping err: *url.Error stringifies the full URL,
		// which contains the API key.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return staticmap.Image{}, fmt.Errorf("geoapify static map: request timed out: %w", staticmap.ErrProviderUnavailable)
		}
		return staticmap.Image{}, fmt.Errorf("geoapify static map: request failed: %w", staticmap.ErrProviderUnavailable)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// A redirect we deliberately did not follow. The Location header is NOT
		// read or reported.
		return staticmap.Image{}, fmt.Errorf("geoapify static map: unexpected redirect (status %d), not followed: %w",
			resp.StatusCode, staticmap.ErrProviderUnavailable)
	case resp.StatusCode == http.StatusTooManyRequests:
		return staticmap.Image{}, fmt.Errorf("geoapify static map: status 429: %w", staticmap.ErrProviderRateLimited)
	case resp.StatusCode >= 500:
		return staticmap.Image{}, fmt.Errorf("geoapify static map: status %d: %w", resp.StatusCode, staticmap.ErrProviderUnavailable)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// 400/401/403: a bad parameter or a bad/revoked key. Definite refusal —
		// the status is reported, the body is NOT echoed (Geoapify answers 4xx
		// with a JSON that quotes the request back at us, key included).
		return staticmap.Image{}, fmt.Errorf("geoapify static map: status %d: %w", resp.StatusCode, staticmap.ErrProviderRejected)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "image/") {
		// A 200 that is not a picture (an error document, an HTML captcha page
		// from something in front of the API) is not something the app can show.
		return staticmap.Image{}, fmt.Errorf("geoapify static map: non-image response: %w", staticmap.ErrProviderUnavailable)
	}

	// Bounded read: an unbounded ReadAll against a misbehaving endpoint is a
	// memory hazard, and a body larger than the cap cannot be a legitimate
	// render of a whitelisted size.
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return staticmap.Image{}, fmt.Errorf("geoapify static map: read response: %w", staticmap.ErrProviderUnavailable)
	}
	if int64(len(body)) > c.maxBytes || len(body) == 0 {
		return staticmap.Image{}, fmt.Errorf("geoapify static map: unusable image body (%d bytes): %w", len(body), staticmap.ErrProviderUnavailable)
	}

	return staticmap.Image{Bytes: body, ContentType: mediaType(ct)}, nil
}

// url builds the request URL.
//
// Assembled by hand rather than through url.Values.Encode(), for the same
// reason as in the 2GIS client: Geoapify's parameter syntax uses ":" and ";"
// structurally ("center=lonlat:76.9,43.2", "marker=lonlat:…;type:material"),
// and Encode percent-escapes ":". Hand-assembly keeps the literal syntax the
// docs specify. It is safe because every component is either a validated
// integer from a whitelist (see staticmap.ParseParams), a float we format
// ourselves, or an operator-set constant. The API key and the style/lang values
// come from the environment, so those ARE escaped.
func (c *Client) url(req staticmap.RenderRequest) string {
	center := coord(req.Lon) + "," + coord(req.Lat) // Geoapify order: lon,lat

	var b strings.Builder
	b.WriteString(c.baseURL)
	b.WriteString("?style=")
	b.WriteString(escapeValue(c.style))
	b.WriteString("&width=")
	b.WriteString(strconv.Itoa(req.Width))
	b.WriteString("&height=")
	b.WriteString(strconv.Itoa(req.Height))
	// The API defaults to JPEG ("much faster for bigger map images"); we ask for
	// PNG so the preview stays crisp at card sizes and matches what the 2GIS
	// provider returned, i.e. the transport layer's Content-Type does not change
	// when the vendor does.
	b.WriteString("&format=png")
	b.WriteString("&center=lonlat:")
	b.WriteString(center)
	b.WriteString("&zoom=")
	b.WriteString(strconv.Itoa(req.Zoom))
	if req.Scale == 2 {
		// scaleFactor=1 is the provider default and is not sent explicitly.
		b.WriteString("&scaleFactor=2")
	}
	b.WriteString("&marker=lonlat:")
	b.WriteString(center)
	b.WriteString(";type:")
	b.WriteString(markerType)
	b.WriteString(";color:")
	b.WriteString(markerColor)
	b.WriteString(";size:")
	b.WriteString(strconv.Itoa(markerSize))
	if c.lang != "" {
		b.WriteString("&lang=")
		b.WriteString(escapeValue(c.lang))
	}
	b.WriteString("&apiKey=")
	b.WriteString(escapeValue(c.apiKey))
	return b.String()
}

// coord formats a coordinate with six decimals (~11 cm) — more than enough for
// a venue pin and stable, so the same venue always produces the same URL.
func coord(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// escapeValue percent-escapes a query value. Written out instead of
// url.QueryEscape because that encodes a space as "+" and we want the strict
// RFC 3986 unreserved set here; the point is to be explicit that these — and
// only these — values are untrusted-by-shape.
func escapeValue(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		ch := v[i]
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

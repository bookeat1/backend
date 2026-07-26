package staticmap

import (
	"fmt"
	"strconv"
	"strings"

	"backend-core/internal/domain"
)

// The render surface is a WHITELIST, not a range check, and that is the whole
// point of this endpoint's input handling.
//
// Every image we serve costs a paid provider request, and every distinct
// (size, scale, zoom) triple is a distinct cache entry. A free-form
// "?width=&height=&zoom=" would let one caller both bill us for arbitrarily
// large renders and blow the cache out of the water by walking the parameter
// space — with no legitimate client ever needing it, because the app has a
// fixed number of map placements. So: three sizes (list card, detail card,
// full-width), two scales, five zooms. Anything else is a 422, never a clamp —
// silently serving a different picture than asked for hides client bugs.

// Params is a validated, whitelisted render size/zoom combination.
type Params struct {
	Width  int
	Height int
	Scale  int
	Zoom   int
}

// size is one entry of the size whitelist.
type size struct {
	name   string
	width  int
	height int
}

// sizes are the only image dimensions this endpoint will ever ask a provider
// for, named after the app placement they exist for.
//
// The ceiling is set so that even the largest size at @2x stays inside 2GIS's
// documented 120–1280 width / 90–1280 height limits (640x360@2x = 1280x720).
// That is deliberately conservative: 2GIS's reference states the limits for the
// `s` parameter and describes @2x as "the same visual size, more clarity",
// which reads as "the limit applies to the logical size" — but we have no key
// to confirm it with, and a whitelist entry that turns out to be rejected by
// the provider is a broken feature for every caller that picks it.
// TODO(verify): проверить на реальном ключе 2GIS, применяется ли лимит
// 120–1280 к значению `s` или к итоговым пикселям с @2x. Если к значению `s`,
// сюда можно вернуть более крупный пресет (например 880x450).
var sizes = []size{
	{name: "card", width: 320, height: 180},
	{name: "detail", width: 480, height: 270},
	{name: "wide", width: 640, height: 360},
}

// zooms are the only zoom levels served. A venue preview is a "where is this
// building" picture: below 14 the venue is a dot in a city, above 18 is past
// 2GIS's documented maximum.
var zooms = []int{14, 15, 16, 17, 18}

// Defaults used when a query parameter is absent. Absent is always legal —
// the mobile client can call the endpoint with no query string at all.
const (
	defaultSizeName = "detail"
	defaultZoom     = 16
	defaultScale    = 1
)

// SizeNames lists the accepted size values, for error messages and docs.
func SizeNames() []string {
	out := make([]string, 0, len(sizes))
	for _, s := range sizes {
		out = append(out, s.name)
	}
	return out
}

// ParseParams validates the three query parameters into Params. Empty strings
// mean "absent" and fall back to the defaults. Every failure is
// domain.ErrValidation (422) and names the offending parameter — the message
// never echoes the caller's raw value back into the response.
func ParseParams(sizeName, scale, zoom string) (Params, error) {
	p := Params{Scale: defaultScale, Zoom: defaultZoom}

	name := strings.TrimSpace(sizeName)
	if name == "" {
		name = defaultSizeName
	}
	found := false
	for _, s := range sizes {
		if s.name == name {
			p.Width, p.Height, found = s.width, s.height, true
			break
		}
	}
	if !found {
		return Params{}, fmt.Errorf("%w: size must be one of %s",
			domain.ErrValidation, strings.Join(SizeNames(), ", "))
	}

	if v := strings.TrimSpace(scale); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 1 && n != 2) {
			return Params{}, fmt.Errorf("%w: scale must be 1 or 2", domain.ErrValidation)
		}
		p.Scale = n
	}

	if v := strings.TrimSpace(zoom); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Params{}, fmt.Errorf("%w: zoom must be an integer", domain.ErrValidation)
		}
		ok := false
		for _, z := range zooms {
			if z == n {
				ok = true
				break
			}
		}
		if !ok {
			return Params{}, fmt.Errorf("%w: zoom must be one of %v", domain.ErrValidation, zooms)
		}
		p.Zoom = n
	}

	return p, nil
}

// Package staticmap serves a map preview of a restaurant as an image rendered
// BY US, from a provider key that never leaves the server.
//
// Why a proxy at all: a map provider key shipped inside a mobile app is not a
// secret. Anyone can pull the installed bundle apart and read it out in
// minutes, and every request made with the stolen key is billed to us. So the
// app asks the backend for a picture, and the backend — not the phone — holds
// the key.
//
// Why the request is keyed by RESTAURANT ID and not by raw lat/lon: taking
// coordinates from the client would make this an open image proxy for any point
// on Earth. Anyone could point it at arbitrary coordinates and have us pay a
// provider for renders that have nothing to do with BookEat — the exact abuse
// the key was moved server-side to prevent. A restaurant id bounds the whole
// endpoint to the finite set of venues in our catalog, which is also what makes
// caching effective (a few hundred cache keys, not an unbounded coordinate
// space). The server looks the coordinates up itself; the client cannot
// influence them.
package staticmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// UseCase renders (and caches) a restaurant's map preview.
type UseCase struct {
	restaurants RestaurantCoords
	provider    Provider // nil = no provider configured on this deployment
	cache       Cache
	flights     *flightGroup
	log         *slog.Logger
}

// New builds the usecase. provider may be nil — a deployment without a map key
// is a supported, normal state: the endpoint still exists and answers a clean
// 503/map_not_configured instead of the service refusing to boot.
func New(restaurants RestaurantCoords, provider Provider, cache Cache, log *slog.Logger) *UseCase {
	if cache == nil {
		cache = NewMemoryCache(DefaultCacheTTL, DefaultCacheMaxBytes)
	}
	if log == nil {
		log = slog.Default()
	}
	return &UseCase{
		restaurants: restaurants,
		provider:    provider,
		cache:       cache,
		flights:     newFlightGroup(),
		log:         log,
	}
}

// Configured reports whether a map provider is wired on this deployment.
func (u *UseCase) Configured() bool { return u.provider != nil }

// Render returns the restaurant's map preview.
//
// Order of operations is deliberate:
//
//  1. cache first, so a hit costs neither a database round-trip nor a paid
//     provider request;
//  2. then the "is a provider even configured" check, so an unconfigured
//     deployment answers instantly and identically for every venue;
//  3. then the coordinate lookup, then the provider.
//
// Every failure is a domain sentinel carrying a specific domain.ErrorCode, so
// the client can tell "this venue has no coordinates" (permanent) from "the
// provider is down" (retry later) from "nobody gave us a key yet". Nothing here
// can produce a 500: a broken provider is not a broken service.
func (u *UseCase) Render(ctx context.Context, restaurantID uuid.UUID, p Params) (Image, error) {
	key := u.cacheKey(restaurantID, p)
	if img, ok := u.cache.Get(key); ok {
		return img, nil
	}
	if u.provider == nil {
		return Image{}, domain.WithCode(domain.CodeMapNotConfigured,
			fmt.Errorf("static map: no provider configured: %w", domain.ErrUnavailable))
	}

	// Collapse a cold-cache stampede: when a venue's screen is opened by many
	// guests at once, exactly ONE of them pays for the provider request and the
	// rest wait for its result. Without this the "cache the result" saving
	// disappears precisely in the burst it matters for.
	img, err := u.flights.do(key, func() (Image, error) {
		return u.fetch(ctx, restaurantID, p)
	})
	if err != nil {
		return Image{}, err
	}
	u.cache.Set(key, img)
	return img, nil
}

// fetch resolves the coordinates and asks the provider. Runs once per cache
// key per burst (see flightGroup).
func (u *UseCase) fetch(ctx context.Context, restaurantID uuid.UUID, p Params) (Image, error) {
	rest, err := u.restaurants.GetByID(ctx, restaurantID)
	if err != nil {
		// Includes domain.ErrNotFound for an unknown id — mapped to 404 by the
		// transport layer, same as every other restaurant-scoped route.
		return Image{}, err
	}
	if !rest.IsActive {
		// A hidden venue must not be discoverable through a side channel: the
		// map endpoint answers exactly like the catalog does for it.
		return Image{}, fmt.Errorf("static map: restaurant is not active: %w", domain.ErrNotFound)
	}
	lat, lon, ok := coordinates(rest)
	if !ok {
		return Image{}, domain.WithCode(domain.CodeMapNoCoordinates,
			fmt.Errorf("static map: restaurant has no usable coordinates: %w", domain.ErrNotFound))
	}

	raw, err := u.provider.Render(ctx, RenderRequest{
		Lat: lat, Lon: lon,
		Width: p.Width, Height: p.Height, Scale: p.Scale, Zoom: p.Zoom,
	})
	if err != nil {
		return Image{}, u.classify(restaurantID, err)
	}
	if len(raw.Bytes) == 0 {
		// Defensive: never hand the app a zero-byte "image", which renders as a
		// broken tile instead of its no-map placeholder.
		return Image{}, domain.WithCode(domain.CodeMapProviderUnavailable,
			fmt.Errorf("static map: provider returned an empty image: %w", domain.ErrUnavailable))
	}
	if raw.ETag == "" {
		raw.ETag = ETagOf(raw.Bytes)
	}
	return raw, nil
}

// classify turns a provider sentinel into the domain sentinel + code the client
// sees. The provider error itself is logged (it never carries the key — see the
// Provider contract) and never returned to the caller verbatim.
func (u *UseCase) classify(restaurantID uuid.UUID, err error) error {
	switch {
	case errors.Is(err, ErrProviderRateLimited):
		u.log.Warn("static map provider rate limited",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("provider", u.provider.Name()))
		return domain.WithCode(domain.CodeMapProviderRateLimited,
			fmt.Errorf("static map: provider rate limited: %w", domain.ErrUnavailable))
	case errors.Is(err, ErrProviderRejected):
		// A 4xx is a configuration problem (revoked key, bad plan), not traffic.
		// Loud, because retrying will not fix it.
		u.log.Error("static map provider rejected the request — check the API key/plan",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("provider", u.provider.Name()),
			slog.String("error", err.Error()))
		return domain.WithCode(domain.CodeMapProviderUnavailable,
			fmt.Errorf("static map: provider rejected the request: %w", domain.ErrUnavailable))
	default:
		u.log.Warn("static map provider unavailable",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("provider", u.provider.Name()),
			slog.String("error", err.Error()))
		return domain.WithCode(domain.CodeMapProviderUnavailable,
			fmt.Errorf("static map: provider unavailable: %w", domain.ErrUnavailable))
	}
}

// cacheKey identifies one rendered picture.
//
// It deliberately does NOT include the coordinates, even though they are the
// input to the render: including them would force a database lookup on every
// request just to build the key, which defeats the point of caching the image
// at all. Restaurant coordinates change roughly never, and when they do the
// stale preview self-heals within the cache TTL. The provider name IS in the
// key so switching vendors never serves the old vendor's pictures.
func (u *UseCase) cacheKey(id uuid.UUID, p Params) string {
	provider := "none"
	if u.provider != nil {
		provider = u.provider.Name()
	}
	return provider + "|" + id.String() + "|" +
		strconv.Itoa(p.Width) + "x" + strconv.Itoa(p.Height) + "@" + strconv.Itoa(p.Scale) +
		"|z" + strconv.Itoa(p.Zoom)
}

// coordinates extracts a usable lat/lon, rejecting NULL columns and values
// outside the valid ranges (bad legacy rows exist; a nonsense coordinate would
// otherwise become a paid provider request for a point in the ocean).
func coordinates(r *domain.RestaurantAggregate) (lat, lon float64, ok bool) {
	if r.Latitude == nil || r.Longitude == nil {
		return 0, 0, false
	}
	lat, lon = *r.Latitude, *r.Longitude
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	if lat == 0 && lon == 0 {
		// Null Island: in practice an unset pair stored as zeroes, not a venue.
		return 0, 0, false
	}
	return lat, lon, true
}

// ETagOf is the strong validator for an image body.
func ETagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// CacheTTL reports the TTL the cache is configured with, for the HTTP max-age.
// Falls back to the default for a Cache implementation that does not expose one.
func CacheTTL(c Cache) time.Duration {
	if t, ok := c.(interface{ TTL() time.Duration }); ok {
		return t.TTL()
	}
	return DefaultCacheTTL
}

// TTL exposes the usecase's cache TTL to the transport layer.
func (u *UseCase) TTL() time.Duration { return CacheTTL(u.cache) }

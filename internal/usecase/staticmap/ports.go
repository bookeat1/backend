package staticmap

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Provider renders one static map image. It is the seam that keeps the map
// vendor — and the vendor's billable API key — behind the server: 2GIS today,
// Google Static Maps or an in-house tile renderer tomorrow, without a single
// line changing in the usecase or the transport layer.
//
// Implementations live in internal/infrastructure/staticmap/<vendor>/ and are
// responsible for exactly two things: turning a RenderRequest into that
// vendor's URL shape, and classifying the vendor's answer into one of the three
// sentinels below. They must never let the API key escape into an error, a log
// line or a response body.
type Provider interface {
	// Name identifies the vendor. It is part of the cache key, so switching
	// providers invalidates cached images instead of serving stale ones from
	// the previous vendor.
	Name() string
	// Render fetches one image. Errors must wrap ErrProviderUnavailable,
	// ErrProviderRateLimited or ErrProviderRejected.
	Render(ctx context.Context, req RenderRequest) (Image, error)
}

// Provider outcome sentinels. They exist so the usecase can map a vendor's
// answer to a client-visible code without knowing anything about that vendor's
// HTTP dialect.
var (
	// ErrProviderUnavailable — timeout, connection failure, 5xx, or a 200 whose
	// body is not a usable image. Transient.
	ErrProviderUnavailable = errors.New("static map provider unavailable")
	// ErrProviderRateLimited — the provider answered 429. Transient, but also
	// an ops signal: we are hitting the plan's quota.
	ErrProviderRateLimited = errors.New("static map provider rate limited")
	// ErrProviderRejected — a definite 4xx refusal: a bad/revoked key, a
	// parameter the provider does not accept. Retrying the same request cannot
	// fix it; a human has to look at the configuration.
	ErrProviderRejected = errors.New("static map provider rejected the request")
)

// RenderRequest is one fully bounded, already validated map render. Every field
// has been checked against the whitelists in Params before it gets here, so a
// Provider may format it into a URL without re-validating and without any risk
// that a caller-supplied string reaches the vendor.
type RenderRequest struct {
	Lat    float64
	Lon    float64
	Width  int // pixels, from the size whitelist
	Height int // pixels, from the size whitelist
	Scale  int // 1 or 2 (HiDPI)
	Zoom   int // from the zoom whitelist
}

// Image is a rendered map picture plus everything the HTTP layer needs to serve
// it: the media type the provider actually returned and a strong validator so a
// repeat request can be answered with 304.
type Image struct {
	Bytes       []byte
	ContentType string
	ETag        string
}

// Cache stores rendered images across requests. The default implementation is
// in-process (NewMemoryCache); the port exists so a multi-instance deployment
// can swap in Redis/S3 without touching the usecase.
//
// Implementations must be safe for concurrent use and must enforce their own
// TTL and memory bound — the usecase never evicts.
type Cache interface {
	// Get returns the cached image for key. ok is false on a miss or an expired
	// entry.
	Get(key string) (img Image, ok bool)
	// Set stores img under key. A cache that refuses the write (too big, out of
	// budget) must simply not store it — never fail the request.
	Set(key string, img Image)
}

// RestaurantCoords is the minimal slice of domain.RestaurantRepository this
// usecase needs: resolve a restaurant id to its coordinates. Declared here (not
// imported from another usecase) per the repo's interface-segregation rule; it
// is satisfied as-is by the Postgres restaurant repository, so no new SQL was
// added for this feature.
type RestaurantCoords interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error)
}

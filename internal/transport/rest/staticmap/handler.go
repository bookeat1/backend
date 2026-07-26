// Package staticmap exposes the public restaurant map-preview endpoint.
//
// Unlike every other handler in this repo it answers with BYTES, not with a
// response.Envelope: the app puts the URL straight into an <Image>, so the
// success path must be a real image with real HTTP caching headers. Failures
// still go through response.HandleError and are ordinary JSON envelopes with a
// machine-readable code — the client distinguishes them by status/code and
// falls back to its existing "no map" placeholder.
package staticmap

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/staticmap"
)

// renderer is the slice of the staticmap usecase this handler needs.
type renderer interface {
	Render(ctx context.Context, restaurantID uuid.UUID, p uc.Params) (uc.Image, error)
	TTL() time.Duration
}

// Handler serves the restaurant map preview.
type Handler struct {
	maps renderer
}

// NewHandler builds the handler.
func NewHandler(maps renderer) *Handler { return &Handler{maps: maps} }

// RegisterPublic mounts the guest-reachable map route. Deliberately mounted on
// the plain public group, not on the OptionalAuth one: the picture is identical
// for every caller, so reading a token would buy nothing and only add a
// per-request user lookup to a route that is meant to be cheap.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/map", h.get)
}

// get returns the restaurant's static map image.
//
// @Summary     Restaurant map preview (server-side proxy)
// @Description Returns a PNG map preview of the restaurant, rendered server-side so the map provider key never reaches the client. Size and zoom are whitelisted. Failures return a JSON envelope whose "code" tells them apart: map_no_coordinates (404), map_not_configured (503), map_provider_rate_limited (503), map_provider_unavailable (503).
// @Tags        restaurants
// @Produce     png
// @Param       id    path  string false "Restaurant id"
// @Param       size  query string false "Image preset" Enums(card, detail, wide) default(detail)
// @Param       scale query int    false "1 or 2 (HiDPI)" default(1)
// @Param       zoom  query int    false "14..18" default(16)
// @Success     200 {file} binary "PNG image"
// @Failure     404 {object} response.Envelope "unknown restaurant or no coordinates"
// @Failure     422 {object} response.Envelope "size/scale/zoom outside the whitelist"
// @Failure     503 {object} response.Envelope "provider not configured or unavailable"
// @Router      /api/v1/restaurants/{id}/map [get]
func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.HandleError(c.Writer, domain.ErrNotFound)
		return
	}
	params, err := uc.ParseParams(c.Query("size"), c.Query("scale"), c.Query("zoom"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}

	img, err := h.maps.Render(c.Request.Context(), id, params)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}

	maxAge := int(h.maps.TTL().Seconds())
	if maxAge <= 0 {
		maxAge = int(uc.DefaultCacheTTL.Seconds())
	}
	// Public: the image carries nothing caller-specific, so CDNs and the app's
	// own HTTP cache are welcome to keep it. immutable is deliberately NOT set —
	// a venue's coordinates can be corrected, and we want that to propagate once
	// max-age lapses.
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	c.Header("ETag", img.ETag)
	if match := c.GetHeader("If-None-Match"); match != "" && match == img.ETag {
		c.Status(http.StatusNotModified)
		return
	}
	ct := img.ContentType
	if ct == "" {
		ct = "image/png"
	}
	c.Data(http.StatusOK, ct, img.Bytes)
}

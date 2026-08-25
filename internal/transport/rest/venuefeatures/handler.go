// Package venuefeatures exposes the venue-feature («удобства») dictionary over
// HTTP: one public read for the app and the venue panel, the platform-only
// management routes, and the venue's own "these are my features" write.
package venuefeatures

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/venuefeatures"
)

type Handler struct{ uc uc.UseCase }

func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterPublic mounts the unauthenticated dictionary read. The app's filter
// sheet builds its «Удобства» section from it instead of a hardcoded list of
// seven ids that the server had never heard of — which is precisely how that
// filter came to change nothing at all.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/venue-features", h.list)
}

// RegisterAdminGlobal mounts the platform-only management routes. MUST be
// mounted on a RequireRole(domain.RoleAdmin) group: the dictionary belongs to
// the platform, a venue only picks from it. The usecase re-checks the role.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/venue-features", h.adminList)
	rg.POST("/admin/venue-features", h.create)
	rg.PATCH("/admin/venue-features/:featureID", h.update)
	rg.DELETE("/admin/venue-features/:featureID", h.hide)
}

// RegisterRestaurantScoped mounts the venue's own feature set. Mount on a
// RequireRestaurantManager(..., "id") group; the usecase re-checks
// restaurant.manage at the resolved venue.
func (h *Handler) RegisterRestaurantScoped(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/features", h.venueList)
	rg.PUT("/restaurants/:id/features", h.venueSet)
}

// actorFrom builds the usecase Actor. An anonymous caller is allowed here with
// an empty actor: the public list is anonymous by design, and every mutation
// path is behind auth middleware already.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
}

func (h *Handler) list(c *gin.Context) { h.respondList(c, false) }

// adminList is the dictionary as its OWNER sees it: hidden entries included,
// for the same reason the admin catalog listing includes hidden venues —
// otherwise hiding a feature makes it unrecoverable.
func (h *Handler) adminList(c *gin.Context) { h.respondList(c, true) }

func (h *Handler) respondList(c *gin.Context, includeInactive bool) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c), includeInactive)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponses(items, reqlocale.Resolve(c)))
}

func (h *Handler) create(c *gin.Context) {
	var req saveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := h.uc.Create(c.Request.Context(), actorFrom(c), req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, toResponse(*out, reqlocale.Resolve(c)))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := featureID(c)
	if !ok {
		return
	}
	var req saveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := h.uc.Update(c.Request.Context(), actorFrom(c), id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponse(*out, reqlocale.Resolve(c)))
}

// hide is DELETE, but it never deletes: a feature referenced by a venue cannot
// go away without taking that data with it, so the verb means
// `is_active = false`. The response carries the entry back with is_active
// false, so the caller can see what actually happened.
func (h *Handler) hide(c *gin.Context) {
	id, ok := featureID(c)
	if !ok {
		return
	}
	out, err := h.uc.SetActive(c.Request.Context(), actorFrom(c), id, false)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponse(*out, reqlocale.Resolve(c)))
}

func (h *Handler) venueList(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	items, err := h.uc.ForRestaurant(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponses(items, reqlocale.Resolve(c)))
}

// venueSet replaces the whole set (PUT, not PATCH): "my features are these"
// is the only statement a venue can make that is unambiguous — an add/remove
// pair would race two managers editing the same venue into a set neither of
// them chose.
func (h *Handler) venueSet(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req setVenueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	ids, err := req.ids()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items, err := h.uc.SetForRestaurant(c.Request.Context(), actorFrom(c), id, ids)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponses(items, reqlocale.Resolve(c)))
}

// featureID parses the dictionary id path parameter, writing 422 on garbage —
// the same code the catalog routes use for an unparseable uuid.
func featureID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("featureID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

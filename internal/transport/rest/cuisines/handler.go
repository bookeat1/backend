// Package cuisines exposes the cuisine dictionary over HTTP: one public read
// for the app and the venue panel, the platform-only management routes, and
// the venue's own "these are my cuisines" write.
package cuisines

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/cuisines"
)

type Handler struct{ uc uc.UseCase }

func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterPublic mounts the unauthenticated dictionary read. The app builds
// its "Выберите кухню" row from it instead of scraping cuisine strings out of
// a catalog page, and the venue panel builds its checkbox list from the same
// route — one list, one order, everywhere.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/cuisines", h.list)
}

// RegisterAdminGlobal mounts the platform-only management routes. MUST be
// mounted on a RequireRole(domain.RoleAdmin) group: the dictionary belongs to
// the platform, a venue only picks from it (ADR-022). The usecase re-checks
// the role anyway.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/cuisines", h.adminList)
	rg.POST("/admin/cuisines", h.create)
	rg.PATCH("/admin/cuisines/:cuisineID", h.update)
	rg.DELETE("/admin/cuisines/:cuisineID", h.hide)
}

// RegisterRestaurantScoped mounts the venue's own cuisine set. Mount on a
// RequireRestaurantManager(..., "id") group; the usecase re-checks
// restaurant.manage at the resolved venue.
func (h *Handler) RegisterRestaurantScoped(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/cuisines", h.venueList)
	rg.PUT("/restaurants/:id/cuisines", h.venueSet)
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

func (h *Handler) list(c *gin.Context) {
	h.respondList(c, false)
}

// adminList is the dictionary as its OWNER sees it: hidden entries included,
// for the same reason the admin catalog listing includes hidden venues —
// otherwise hiding a cuisine makes it unrecoverable.
func (h *Handler) adminList(c *gin.Context) {
	h.respondList(c, true)
}

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
	id, ok := cuisineID(c)
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

// hide is DELETE, but it never deletes: a cuisine referenced by a venue or by
// a guest's preferences cannot go away without taking that data with it, so
// the verb means `is_active = false`. The response carries the entry back with
// is_active false, so the caller can see what actually happened.
func (h *Handler) hide(c *gin.Context) {
	id, ok := cuisineID(c)
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

// venueSet replaces the whole set (PUT, not PATCH): "my cuisines are these"
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

// cuisineID parses the dictionary id path parameter, writing 422 on garbage —
// the same code the catalog routes use for an unparseable uuid.
func cuisineID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("cuisineID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

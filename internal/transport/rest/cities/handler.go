// Package cities exposes the city dictionary over HTTP: the public read the
// app has always called, and the platform-only management routes.
package cities

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/cities"
)

type Handler struct{ uc uc.UseCase }

func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// formatFull is the opt-in that switches GET /cities from the legacy array of
// names to the full dictionary entries.
const formatFull = "full"

// RegisterPublic mounts the unauthenticated dictionary read on the SAME path
// the catalog handler used to serve (GET /cities). The route moved packages,
// its default answer did not.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/cities", h.list)
}

// RegisterAdminGlobal mounts the platform-only management routes. MUST be
// mounted on a RequireRole(domain.RoleAdmin) group: the dictionary belongs to
// the platform, a venue only points at an entry (ADR-023). The usecase
// re-checks the role anyway.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/cities", h.adminList)
	rg.POST("/admin/cities", h.create)
	rg.PATCH("/admin/cities/:cityID", h.update)
	rg.DELETE("/admin/cities/:cityID", h.hide)
	rg.PUT("/admin/cities/order", h.reorder)
	rg.POST("/admin/cities/:cityID/aliases", h.addAlias)
}

// actorFrom builds the usecase Actor. An anonymous caller gets an empty actor:
// the public list is anonymous by design, and every mutation is behind auth
// middleware already.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
}

// list serves the public dictionary, active entries only, in dictionary order.
//
// THE DEFAULT BODY IS FROZEN. Without ?format=full it is the bare array of
// Russian names this route has returned since the catalog first shipped, and
// the build currently in the store parses exactly that. The dictionary — ids,
// codes, translations — is served on the same route under ?format=full, so a
// new client gets everything and an old one notices nothing.
func (h *Handler) list(c *gin.Context) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c), false)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	if c.Query("format") == formatFull {
		response.OK(c.Writer, toResponses(items, reqlocale.Resolve(c)))
		return
	}
	response.OK(c.Writer, toNames(items))
}

// adminList is the dictionary as its OWNER sees it: hidden entries included,
// always in full form. Without this, hiding a city would make it unrecoverable.
func (h *Handler) adminList(c *gin.Context) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c), true)
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
	id, ok := cityID(c)
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

// hide is DELETE, but it never deletes: venues reference the city by id and
// carry its name as a live string, so the verb means `is_active = false`. The
// response carries the entry back with is_active false, so the caller can see
// what actually happened.
func (h *Handler) hide(c *gin.Context) {
	id, ok := cityID(c)
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

func (h *Handler) reorder(c *gin.Context) {
	var req reorderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	ids, err := req.ids()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items, err := h.uc.Reorder(c.Request.Context(), actorFrom(c), ids)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponses(items, reqlocale.Resolve(c)))
}

// addAlias teaches the dictionary that another spelling means this city. It is
// the manual answer to a venue that arrived from the legacy system with a city
// string nobody recognised: name the spelling once, and the database trigger
// links that venue — and every future one — on its next write.
func (h *Handler) addAlias(c *gin.Context) {
	id, ok := cityID(c)
	if !ok {
		return
	}
	var req aliasRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := h.uc.AddAlias(c.Request.Context(), actorFrom(c), id, req.Alias)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toResponse(*out, reqlocale.Resolve(c)))
}

// cityID parses the dictionary id path parameter, writing 422 on garbage — the
// same code the catalog routes use for an unparseable uuid.
func cityID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("cityID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

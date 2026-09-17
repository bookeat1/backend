// Package foodieoptions exposes the "Фуди-профиль" option dictionary over
// HTTP: one public read for the mobile wizard, and the platform-only
// management routes (spec foodie-profile-admin-dictionaries-20260916.md).
// Modeled directly on transport/rest/cuisines — same route shape, same
// actorFrom/HandleError plumbing.
package foodieoptions

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/foodieoptions"
)

type Handler struct{ uc uc.UseCase }

func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterPublic mounts the unauthenticated dictionary read. The mobile
// wizard (and mobile web, same bundle) builds its four steps from it instead
// of the store-shipped constant lists it used to carry — see FE-M1 in the
// parent spec.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/foodie-profile/options", h.list)
}

// RegisterAdminGlobal mounts the platform-only management routes. MUST be
// mounted on a RequireRole(domain.RoleAdmin) group — the usecase re-checks
// the role anyway (defense in depth, same as cuisines).
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/foodie-profile/options", h.adminList)
	rg.POST("/admin/foodie-profile/options", h.create)
	rg.PATCH("/admin/foodie-profile/options/:optionID", h.update)
	rg.DELETE("/admin/foodie-profile/options/:optionID", h.hide)
}

func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
}

// list is the wizard's read: anonymous, active options only.
//
// @Summary  List the foodie-profile options a guest can pick
// @Tags     foodie-profile
// @Produce  json
// @Success  200 {object} response.Envelope{data=publicOptionsResponse}
// @Router   /foodie-profile/options [get]
func (h *Handler) list(c *gin.Context) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c), false)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toPublicOptions(items, reqlocale.Resolve(c)))
}

// adminList is the dictionary as the PLATFORM sees it: hidden entries
// included, plus the admin-only fields (is_active, cuisine link,
// affects_matching, timestamps) — otherwise hiding an option makes it
// unrecoverable, same reasoning as cuisines.adminList.
//
// @Summary  List every foodie-profile option, including hidden ones
// @Tags     foodie-profile-admin
// @Produce  json
// @Success  200 {object} response.Envelope{data=adminOptionsResponse}
// @Failure  403 {object} response.Envelope
// @Security BearerAuth
// @Router   /admin/foodie-profile/options [get]
func (h *Handler) adminList(c *gin.Context) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c), true)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminOptions(items, reqlocale.Resolve(c)))
}

// create adds a new option.
//
// @Summary  Create a foodie-profile option
// @Tags     foodie-profile-admin
// @Accept   json
// @Produce  json
// @Param    body body saveRequest true "New option"
// @Success  201 {object} response.Envelope{data=adminOptionResponse}
// @Failure  403 {object} response.Envelope
// @Failure  409 {object} response.Envelope "code or name already used for this kind"
// @Failure  422 {object} response.Envelope
// @Security BearerAuth
// @Router   /admin/foodie-profile/options [post]
func (h *Handler) create(c *gin.Context) {
	var req saveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out, err := h.uc.Create(c.Request.Context(), actorFrom(c), in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, toAdminOption(*out, reqlocale.Resolve(c)))
}

// update applies a partial edit. code/kind may be resent unchanged but not
// changed (422 "code/kind is immutable").
//
// @Summary  Edit a foodie-profile option
// @Tags     foodie-profile-admin
// @Accept   json
// @Produce  json
// @Param    optionID path string true "Option id"
// @Param    body body saveRequest true "Fields to change"
// @Success  200 {object} response.Envelope{data=adminOptionResponse}
// @Failure  403 {object} response.Envelope
// @Failure  404 {object} response.Envelope
// @Failure  409 {object} response.Envelope "code or name already used for this kind"
// @Failure  422 {object} response.Envelope "code/kind is immutable, cannot hide the last active option of its kind, no_diet cannot be hidden, ..."
// @Security BearerAuth
// @Router   /admin/foodie-profile/options/{optionID} [patch]
func (h *Handler) update(c *gin.Context) {
	id, ok := optionID(c)
	if !ok {
		return
	}
	var req saveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out, err := h.uc.Update(c.Request.Context(), actorFrom(c), id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminOption(*out, reqlocale.Resolve(c)))
}

// hide is DELETE, but it never deletes: user_foodie_* rows can reference a
// hidden code with no FK to enforce otherwise, so the verb means
// `is_active = false`. Refused (422) for domain.FoodieDietExclusiveID
// ("no_diet") and for the last active option of its kind — same "hide never
// hard-deletes" rule as cuisines.hide.
//
// @Summary  Hide a foodie-profile option
// @Tags     foodie-profile-admin
// @Produce  json
// @Param    optionID path string true "Option id"
// @Success  200 {object} response.Envelope{data=adminOptionResponse}
// @Failure  403 {object} response.Envelope
// @Failure  404 {object} response.Envelope
// @Failure  422 {object} response.Envelope "no_diet cannot be hidden / cannot hide the last active option of its kind"
// @Security BearerAuth
// @Router   /admin/foodie-profile/options/{optionID} [delete]
func (h *Handler) hide(c *gin.Context) {
	id, ok := optionID(c)
	if !ok {
		return
	}
	out, err := h.uc.SetActive(c.Request.Context(), actorFrom(c), id, false)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminOption(*out, reqlocale.Resolve(c)))
}

func optionID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("optionID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// Package platformpages exposes the footer's editable text pages over HTTP:
// the public, unauthenticated read the site uses to render "Как это
// работает"/"Оферта"/etc., and the superadmin-only cabinet screen that edits
// them.
package platformpages

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/platformpages"
)

// publicMaxAge is how long a client (and any CDN in front of us) may reuse an
// answer. 60s matches the spec's "правка видна на сайте не позже чем через
// минуту" promise — a shorter value would just add load for no visible gain,
// a longer one would miss the promise.
const publicMaxAge = 60

// Handler serves platform_pages.
type Handler struct{ uc uc.UseCase }

// NewHandler builds the handler.
func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterPublic mounts the anonymous read. Plain public group, no auth at
// all — this is footer content read by a guest who has not signed in, same
// posture as the cuisine/city dictionaries.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/pages/:slug", h.getPublic)
}

// RegisterAdminGlobal mounts the cabinet's three routes. MUST be mounted on a
// RequireRole(domain.RoleAdmin) group — this switch can put the wrong text
// under a live legal page (offer, privacy policy) in front of every guest at
// once. The usecase re-checks the role anyway.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/pages", h.adminList)
	rg.GET("/admin/pages/:slug", h.adminGet)
	rg.PUT("/admin/pages/:slug", h.adminUpdate)
}

// getPublic serves one page to the site.
//
// @Summary     Read a published site page
// @Description Public, unauthenticated, cacheable. 404 for an unknown slug AND for a page that exists but is not published — the two are indistinguishable on the wire on purpose. Title/body are resolved to the requested language (?lang= or Accept-Language), falling back to Russian.
// @Tags        pages
// @Produce     json
// @Param       slug path string true "one of about, jobs, contacts, how-it-works, cancellation, offer, privacy"
// @Success     200 {object} response.Envelope{data=publicResponse}
// @Failure     404 {object} response.Envelope "unknown or unpublished slug"
// @Router      /api/v1/pages/{slug} [get]
func (h *Handler) getPublic(c *gin.Context) {
	p, err := h.uc.GetPublished(c.Request.Context(), c.Param("slug"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(publicMaxAge))
	response.OK(c.Writer, toPublicResponse(*p, lang))
}

// adminList returns all seven pages as their owner sees them.
//
// @Summary     List every site page
// @Tags        pages
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope{data=[]adminResponse}
// @Failure     403 {object} response.Envelope "not a superadmin"
// @Router      /api/v1/admin/pages [get]
func (h *Handler) adminList(c *gin.Context) {
	items, err := h.uc.ListAdmin(c.Request.Context(), actorFrom(c))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminResponses(items))
}

// adminGet returns one page as its owner sees it, translations included.
//
// @Summary     Read one site page for editing
// @Tags        pages
// @Produce     json
// @Security    BearerAuth
// @Param       slug path string true "one of about, jobs, contacts, how-it-works, cancellation, offer, privacy"
// @Success     200 {object} response.Envelope{data=adminResponse}
// @Failure     403 {object} response.Envelope "not a superadmin"
// @Failure     404 {object} response.Envelope "unknown slug"
// @Router      /api/v1/admin/pages/{slug} [get]
func (h *Handler) adminGet(c *gin.Context) {
	p, err := h.uc.GetAdmin(c.Request.Context(), actorFrom(c), c.Param("slug"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminResponse(*p))
}

// adminUpdate applies a partial write to one page.
//
// @Summary     Edit one site page
// @Description PATCH semantics on PUT: absent fields keep their stored value, and the *_i18n objects are partial translation patches. published=true with an empty body is refused (code page_body_empty).
// @Tags        pages
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       slug path string true "one of about, jobs, contacts, how-it-works, cancellation, offer, privacy"
// @Param       body body updateRequest true "Fields to change"
// @Success     200 {object} response.Envelope{data=adminResponse}
// @Failure     403 {object} response.Envelope "not a superadmin"
// @Failure     404 {object} response.Envelope "unknown slug"
// @Failure     422 {object} response.Envelope "empty title, oversized field, or published with an empty body (code page_body_empty)"
// @Router      /api/v1/admin/pages/{slug} [put]
func (h *Handler) adminUpdate(c *gin.Context) {
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := h.uc.Update(c.Request.Context(), actorFrom(c), c.Param("slug"), req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toAdminResponse(*out))
}

// actorFrom builds the usecase actor. An anonymous caller gets an empty
// actor — the admin routes sit behind auth middleware already, and the
// usecase refuses an empty role.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
}

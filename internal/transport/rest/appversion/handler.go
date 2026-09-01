// Package appversion exposes the mobile update gate over HTTP: the public
// "should this build update?" check every app launch makes, and the
// platform-only screen that sets the thresholds and the wording.
package appversion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/appversion"
)

// checkMaxAge is how long a client (and any CDN in front of us) may reuse an
// answer. Five minutes is the deliberate trade: the check runs once per launch,
// so caching keeps a cold-start stampede off the database, and switching the
// forced-update threshold on still reaches everyone within minutes rather than
// at the next app release.
const checkMaxAge = 300

// versionQueryMaxLen bounds the version parameter before it is looked at. The
// route is unauthenticated, so the size of what we are willing to read is
// decided before, not after, parsing.
const versionQueryMaxLen = 64

// Handler serves the update gate.
type Handler struct{ uc uc.UseCase }

// NewHandler builds the handler.
func NewHandler(u uc.UseCase) *Handler { return &Handler{uc: u} }

// RegisterPublic mounts the anonymous check. Deliberately on the plain public
// group and NOT on the OptionalAuth one: the answer depends on the build, never
// on who is holding the phone, and this route is called before anybody has
// signed in — reading a token would only add a user lookup to the first request
// of every cold start.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/app/version-check", h.check)
}

// RegisterAdminGlobal mounts the management routes. MUST be mounted on a
// RequireRole(domain.RoleAdmin) group: this is the switch that can put a
// blocking screen in front of every guest at once. The usecase re-checks the
// role anyway.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/app-update-policies", h.adminList)
	rg.PUT("/admin/app-update-policies/:platform", h.adminSave)
}

// check answers one launching client.
//
// @Summary     Does this build have to update?
// @Description Public, unauthenticated, cacheable. The MODE is the server's: "none" (do nothing), "recommended" (dismissible prompt) or "required" (blocking screen). Every uncertain input — an unconfigured platform, an empty or unparsable version — answers "none", so this endpoint can never lock a guest out by accident. Titles and messages come back as full {ru,kk,en} objects; the client picks the language, which is why the answer does not depend on Accept-Language and can be cached by URL alone.
// @Tags        app
// @Produce     json
// @Param       platform query string true  "ios or android"
// @Param       version  query string false "The build's marketing version, e.g. 1.5.1. Garbage or absent answers action=none."
// @Success     200 {object} response.Envelope{data=checkResponse}
// @Failure     422 {object} response.Envelope "platform missing or not ios/android (code app_platform_unknown)"
// @Router      /api/v1/app/version-check [get]
func (h *Handler) check(c *gin.Context) {
	platform, ok := domain.ParseStorePlatform(c.Query("platform"))
	if !ok {
		// The one refusal on this route. A missing/unknown platform is a client
		// bug, not a guest's fault, and answering "none" for it would hide the
		// bug forever; the app treats any non-200 as "do nothing" anyway.
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity,
			domain.CodeAppPlatformUnknown, "platform must be ios or android")
		return
	}
	version := c.Query("version")
	if len(version) > versionQueryMaxLen {
		// Truncating rather than refusing keeps the "garbage answers none"
		// promise: an over-long value cannot parse, so it lands on none.
		version = version[:versionQueryMaxLen]
	}

	d, err := h.uc.Check(c.Request.Context(), platform, version)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}

	body := toCheckResponse(d)
	// Public: the answer carries nothing caller-specific — the whole payload is
	// a function of the two query parameters, which are already part of the URL.
	c.Header("Cache-Control", "public, max-age="+strconv.Itoa(checkMaxAge))
	if etag := etagOf(body); etag != "" {
		c.Header("ETag", etag)
		if match := c.GetHeader("If-None-Match"); match == etag {
			c.Status(http.StatusNotModified)
			return
		}
	}
	response.OK(c.Writer, body)
}

// adminList returns both platforms' policies as their owner edits them: every
// field, translations included, never resolved to one language.
//
// @Summary     Read the mobile update policies
// @Tags        app
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope{data=[]policyResponse}
// @Failure     403 {object} response.Envelope "not a superadmin"
// @Router      /api/v1/admin/app-update-policies [get]
func (h *Handler) adminList(c *gin.Context) {
	items, err := h.uc.List(c.Request.Context(), actorFrom(c))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toPolicyResponses(items))
}

// adminSave applies a partial update to one platform's policy.
//
// @Summary     Set one platform's mobile update policy
// @Description PATCH semantics on PUT: absent fields keep their stored value, and the *_i18n objects are partial translation patches. An empty threshold means "no threshold" — that is how a forced update is switched OFF.
// @Tags        app
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       platform path string true "ios or android"
// @Param       body body saveRequest true "Fields to change"
// @Success     200 {object} response.Envelope{data=policyResponse}
// @Failure     403 {object} response.Envelope "not a superadmin"
// @Failure     422 {object} response.Envelope "unknown platform, unparsable version, missing wording or store URL"
// @Router      /api/v1/admin/app-update-policies/{platform} [put]
func (h *Handler) adminSave(c *gin.Context) {
	platform, ok := domain.ParseStorePlatform(c.Param("platform"))
	if !ok {
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity,
			domain.CodeAppPlatformUnknown, "platform must be ios or android")
		return
	}
	var req saveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := h.uc.Save(c.Request.Context(), actorFrom(c), platform, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, toPolicyResponse(*out))
}

// actorFrom builds the usecase actor. An anonymous caller gets an empty actor —
// the management routes sit behind auth middleware already, and the usecase
// refuses an empty role.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}
}

// etagOf hashes the serialized answer. Derived from the BODY rather than from
// updated_at because the body also depends on the caller's version: two clients
// on different builds read the same row and must not share a validator.
func etagOf(body checkResponse) string {
	b, err := json.Marshal(body)
	if err != nil {
		// Cannot happen for this struct; an empty ETag simply disables
		// revalidation rather than failing the request.
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/auth"
)

// TelegramHandler serves the venue mini app's three sign-in endpoints (spec
// §5.2 A–C). It is a handler of its own rather than three more methods on
// Handler because it depends on a different usecase and switches itself off
// wholesale when the bot is not configured.
type TelegramHandler struct{ uc *uc.MiniAppUseCase }

// NewTelegramHandler wires the mini app sign-in.
func NewTelegramHandler(u *uc.MiniAppUseCase) *TelegramHandler { return &TelegramHandler{uc: u} }

// RegisterRoutes mounts the three endpoints on the PUBLIC group: none of them
// can require a bearer token, because obtaining one is what they are for.
// Unlink also accepts a bearer and reads it through OptionalAuth-style lookup on
// the context — present when the group runs Auth, absent here, which is why the
// handler falls back to identifying the caller by initData.
//
// POST /auth/telegram/link MUST be listed as TierStrict in
// middleware.routeTiers: it reaches the same password check as /auth/login, and
// on the default tier it would be a more generous door to the same secret.
func (h *TelegramHandler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/auth/telegram")
	g.POST("/miniapp", h.signIn)
	g.POST("/link", h.link)
	g.POST("/unlink", h.unlink)
}

// miniAppSignInRequest is the ordinary open of the mini app.
type miniAppSignInRequest struct {
	InitData string `json:"init_data" binding:"required"`
}

// miniAppLinkRequest is the first sign-in. It is NEVER logged: the blob carries
// a real name and a Telegram id, and the body carries a password.
type miniAppLinkRequest struct {
	InitData string `json:"init_data" binding:"required"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// miniAppUnlinkRequest — init_data is optional here: an authenticated caller may
// sign out on their bearer alone.
type miniAppUnlinkRequest struct {
	InitData string `json:"init_data"`
}

type miniAppVenueResponse struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Role string    `json:"role"`
}

type miniAppUserResponse struct {
	ID       uuid.UUID `json:"id"`
	FullName string    `json:"full_name"`
	Role     string    `json:"role"`
}

// miniAppSessionResponse is the single success shape of both sign-in endpoints,
// so the mini app parses one thing whether it just typed a password or not.
type miniAppSessionResponse struct {
	tokenPairResponse
	User        miniAppUserResponse    `json:"user"`
	Restaurants []miniAppVenueResponse `json:"restaurants"`
}

func fromSession(c *gin.Context, s *uc.MiniAppSession) miniAppSessionResponse {
	lang := reqlocale.Resolve(c)
	venues := make([]miniAppVenueResponse, 0, len(s.Venues))
	for _, v := range s.Venues {
		venues = append(venues, miniAppVenueResponse{
			ID:   v.RestaurantID,
			Name: v.NameI18n.Resolve(lang, v.Name),
			Role: v.Role,
		})
	}
	return miniAppSessionResponse{
		tokenPairResponse: fromPair(s.Pair),
		User: miniAppUserResponse{
			ID: s.User.ID, FullName: s.User.FullName, Role: string(s.User.Role),
		},
		Restaurants: venues,
	}
}

// configured answers 404 and reports false when the restaurants bot is not set
// up. Same rule as the inbound webhook: an endpoint that cannot verify a
// signature must not exist, because the only other option is accepting an
// unverified one.
func (h *TelegramHandler) configured(c *gin.Context) bool {
	if h.uc != nil && h.uc.Configured() {
		return true
	}
	response.Error(c.Writer, http.StatusNotFound, "not found")
	return false
}

// signIn opens the mini app with the remembered link, no password.
// @Summary     Telegram mini app sign-in
// @Description Signs a venue employee in from the Telegram mini app using the link
// @Description created by an earlier password sign-in. Branch on the error "code":
// @Description init_data_invalid / init_data_expired (401 — reopen the app from the
// @Description bot), link_required (403 — show the email + password form),
// @Description staff_not_found (403 — the account is no longer staff anywhere).
// @Description A 404 means the restaurants bot is not configured on this deployment.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body miniAppSignInRequest true "Signed initData"
// @Success     200 {object} response.Envelope{data=miniAppSessionResponse}
// @Failure     401 {object} response.Envelope "init_data_invalid | init_data_expired"
// @Failure     403 {object} response.Envelope "link_required | staff_not_found"
// @Failure     404 {object} response.Envelope "restaurants bot not configured"
// @Router      /api/v1/auth/telegram/miniapp [post]
func (h *TelegramHandler) signIn(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	var req miniAppSignInRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity, domain.CodeValidation, "init_data required")
		return
	}
	s, err := h.uc.SignIn(c.Request.Context(), req.InitData)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromSession(c, s))
}

// link is the first sign-in: password now, never again on this device.
// @Summary     Telegram mini app first sign-in
// @Description Verifies the initData signature, then the email and password, then the
// @Description venue membership — in that order — and remembers the Telegram account
// @Description so later opens need no password. Rate-limited on the strict tier, the
// @Description same as POST /auth/login. Error codes: init_data_invalid,
// @Description init_data_expired, invalid_credentials (401, one code for a wrong
// @Description address and a wrong password alike), staff_not_found (403 — the
// @Description credentials are correct but the account works at no venue; no link is
// @Description created).
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body miniAppLinkRequest true "Signed initData and credentials"
// @Success     200 {object} response.Envelope{data=miniAppSessionResponse}
// @Failure     401 {object} response.Envelope "init_data_invalid | init_data_expired | invalid_credentials"
// @Failure     403 {object} response.Envelope "staff_not_found"
// @Failure     404 {object} response.Envelope "restaurants bot not configured"
// @Failure     429 {object} response.Envelope "too many attempts"
// @Router      /api/v1/auth/telegram/link [post]
func (h *TelegramHandler) link(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	var req miniAppLinkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// The binding error is NOT echoed back: for this body it would quote the
		// field values, and one of them is a password.
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity, domain.CodeValidation,
			"init_data, email and password required")
		return
	}
	s, err := h.uc.Link(c.Request.Context(), req.InitData, req.Email, req.Password)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromSession(c, s))
}

// unlink is «Выйти».
// @Summary     Telegram mini app sign-out
// @Description Revokes the Telegram link and every refresh token of the account, so
// @Description the next open of the mini app asks for the password again. Accepts a
// @Description signed initData body, a bearer token, or both; with both, the bearer
// @Description must name the same account the link points at.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body miniAppUnlinkRequest false "Signed initData (optional with a bearer)"
// @Success     204 "signed out"
// @Failure     401 {object} response.Envelope "init_data_invalid | init_data_expired"
// @Failure     403 {object} response.Envelope "the bearer does not own this link"
// @Failure     404 {object} response.Envelope "restaurants bot not configured"
// @Router      /api/v1/auth/telegram/unlink [post]
func (h *TelegramHandler) unlink(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	var req miniAppUnlinkRequest
	// An empty body is allowed: a bearer-only sign-out carries nothing.
	_ = c.ShouldBindJSON(&req)

	var userID uuid.UUID
	if au, ok := middleware.GetAuthUser(c.Request.Context()); ok {
		userID = au.ID
	}
	if req.InitData == "" && userID == uuid.Nil {
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity, domain.CodeValidation,
			"init_data or a bearer token required")
		return
	}
	if err := h.uc.Unlink(c.Request.Context(), req.InitData, userID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	c.Status(http.StatusNoContent)
}

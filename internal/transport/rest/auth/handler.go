// Package auth exposes the authentication HTTP endpoints.
package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/auth"
)

type Handler struct {
	auth uc.Facade
	otp  uc.OTPUseCase
}

func NewHandler(auth uc.Facade, otp uc.OTPUseCase) *Handler {
	return &Handler{auth: auth, otp: otp}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/auth")
	g.POST("/signup", h.signup)
	g.POST("/login", h.login)
	g.POST("/otp/request", h.otpRequest)
	g.POST("/otp/verify", h.otpVerify)
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
}

// signup registers a new user and returns an access/refresh token pair.
// @Summary     Sign up
// @Description Registers a new user with email + password and returns a token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body signupRequest true "New account details"
// @Success     201 {object} response.Envelope{data=tokenPairResponse}
// @Failure     409 {object} response.Envelope "email already registered"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/signup [post]
func (h *Handler) signup(c *gin.Context) {
	var req signupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.auth.Signup(c.Request.Context(), req.Email, req.Password, req.FullName)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, fromPair(pair))
}

// login authenticates a user by email and password.
// @Summary     Log in
// @Description Authenticates by email + password and returns a token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body loginRequest true "Credentials"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid credentials"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/login [post]
func (h *Handler) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.auth.Login(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

// otpRequest generates and sends a one-time code to a phone number.
// @Summary     Request an OTP code
// @Description Generates a one-time code and delivers it to the phone. Rate-limited
// @Description (per-minute and per-hour); over the limit returns 422. The response
// @Description "code" field is populated only when AUTH_OTP_DEV_EXPOSE=true.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body otpRequestRequest true "Phone number"
// @Success     200 {object} response.Envelope{data=otpRequestedResponse}
// @Failure     422 {object} response.Envelope "validation failed / rate limited"
// @Router      /api/v1/auth/otp/request [post]
func (h *Handler) otpRequest(c *gin.Context) {
	var req otpRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	code, err := h.otp.RequestOTP(c.Request.Context(), req.Phone)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, otpRequestedResponse{Sent: true, Code: code})
}

// otpVerify checks a one-time code and returns a token pair on success.
// @Summary     Verify an OTP code
// @Description Verifies the latest active code for the phone. On success, finds or
// @Description creates the user and returns a token pair. Wrong/expired codes and
// @Description too many attempts return 401.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body otpVerifyRequest true "Phone and code"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid or expired code"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/otp/verify [post]
func (h *Handler) otpVerify(c *gin.Context) {
	var req otpVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.otp.VerifyOTP(c.Request.Context(), req.Phone, req.Code)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

// refresh exchanges a refresh token for a new token pair.
// @Summary     Refresh tokens
// @Description Exchanges a valid refresh token for a new access/refresh token pair.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body refreshRequest true "Refresh token"
// @Success     200 {object} response.Envelope{data=tokenPairResponse}
// @Failure     401 {object} response.Envelope "invalid or expired refresh token"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/refresh [post]
func (h *Handler) refresh(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pair, err := h.auth.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromPair(pair))
}

// logout revokes a refresh token.
// @Summary     Log out
// @Description Revokes the given refresh token so it can no longer be used.
// @Tags        auth
// @Accept      json
// @Produce     json
// @Param       body body refreshRequest true "Refresh token to revoke"
// @Success     200 {object} response.Envelope{data=object} "{\"data\":{\"ok\":true}}"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/auth/logout [post]
func (h *Handler) logout(c *gin.Context) {
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.auth.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"ok": true})
}

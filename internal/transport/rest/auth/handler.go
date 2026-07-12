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

// Package users exposes the current-user profile HTTP endpoints. Routes must be
// registered on a group already protected by middleware.Auth.
package users

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	authuc "backend-core/internal/usecase/auth"
	uc "backend-core/internal/usecase/users"
)

type Handler struct {
	facade uc.Facade
	// otp is the phone-OTP usecase, borrowed so a signed-in user can change
	// their number verified by a code sent to the NEW number. The flow is kept
	// out of the profile facade (updateMe carries no phone) because it is an
	// OTP flow, not a plain field edit.
	otp authuc.OTPUseCase
}

func NewHandler(f uc.Facade, otp authuc.OTPUseCase) *Handler {
	return &Handler{facade: f, otp: otp}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/users")
	g.GET("/me", h.me)
	g.PATCH("/me", h.updateMe)
	g.DELETE("/me", h.deleteMe)
	g.POST("/me/phone/otp/request", h.requestPhoneChange)
	g.POST("/me/phone/otp/verify", h.verifyPhoneChange)
}

// me returns the authenticated user's profile.
// @Summary     Get current user
// @Description Returns the profile of the authenticated user.
// @Tags        users
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope{data=userResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/users/me [get]
func (h *Handler) me(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := h.facade.Me(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	cuisineIDs, err := h.facade.CuisinePreferences(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u, cuisineIDs))
}

// updateMe applies a partial update to the authenticated user's profile.
// @Summary     Update current user
// @Description Partially updates the authenticated user's profile. Only the
// @Description provided fields are changed; omitted fields are left untouched.
// @Tags        users
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body updateMeRequest true "Fields to update (all optional)"
// @Success     200 {object} response.Envelope{data=userResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "validation failed"
// @Router      /api/v1/users/me [patch]
func (h *Handler) updateMe(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req updateMeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	u, err := h.facade.UpdateMe(c.Request.Context(), au.ID, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	cuisineIDs, err := h.facade.CuisinePreferences(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u, cuisineIDs))
}

// deleteMe soft-deletes and anonymizes the authenticated user's own account.
// Idempotent: calling it again on an already-deleted account still returns 200.
// @Summary     Delete current user's account
// @Description Soft-deletes and anonymizes the authenticated user's account.
// @Description Bookings/payments keep their reference to the (now anonymized)
// @Description user id; they are never deleted or altered. Idempotent.
// @Tags        users
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/users/me [delete]
func (h *Handler) deleteMe(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.facade.DeleteMe(c.Request.Context(), au.ID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

// requestPhoneChange sends an OTP to the NEW number the caller wants to move to.
// @Summary     Request a phone-change OTP
// @Description Sends a one-time code to a NEW phone number the authenticated user
// @Description wants to switch to. Shares the same per-number rate limit as login
// @Description OTP. The "code" field is populated only when AUTH_OTP_DEV_EXPOSE=true.
// @Description Returns 422 (otp_invalid_phone / phone_unchanged / rate limited) or
// @Description 409 (phone_in_use) when the number already belongs to another account.
// @Tags        users
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body phoneChangeRequestRequest true "New phone number"
// @Success     200 {object} response.Envelope{data=phoneChangeRequestedResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     409 {object} response.Envelope "number already in use"
// @Failure     422 {object} response.Envelope "validation failed / rate limited / same number"
// @Router      /api/v1/users/me/phone/otp/request [post]
func (h *Handler) requestPhoneChange(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req phoneChangeRequestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	code, err := h.otp.RequestPhoneChangeOTP(c.Request.Context(), au.ID, req.NewPhone)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, phoneChangeRequestedResponse{Sent: true, Code: code})
}

// verifyPhoneChange verifies the OTP sent to the new number and switches it.
// @Summary     Verify a phone-change OTP
// @Description Verifies the code delivered to the new number and moves the
// @Description authenticated user to it (setting phone_verified_at). No new token
// @Description pair is issued — the caller stays signed in. Returns the updated
// @Description user, same shape as PATCH /users/me. A wrong/expired code returns
// @Description 401 (otp_invalid / otp_too_many_attempts); a number that raced into
// @Description use returns 409 (phone_in_use); same-number returns 422.
// @Tags        users
// @Accept      json
// @Produce     json
// @Security    BearerAuth
// @Param       body body phoneChangeVerifyRequest true "New phone and code"
// @Success     200 {object} response.Envelope{data=userResponse}
// @Failure     401 {object} response.Envelope "unauthorized / invalid or expired code"
// @Failure     409 {object} response.Envelope "number already in use"
// @Failure     422 {object} response.Envelope "validation failed / same number"
// @Router      /api/v1/users/me/phone/otp/verify [post]
func (h *Handler) verifyPhoneChange(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req phoneChangeVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	u, err := h.otp.VerifyPhoneChange(c.Request.Context(), au.ID, req.NewPhone, req.Code)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	cuisineIDs, err := h.facade.CuisinePreferences(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, fromDomain(u, cuisineIDs))
}

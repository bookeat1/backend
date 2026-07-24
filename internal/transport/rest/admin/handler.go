// Package admin exposes the restaurant admin-panel HTTP endpoints. Every route
// is tenant-scoped to the restaurant in the path (:id) and RBAC-guarded inside
// usecase/admin: the transport layer only builds the Actor and parses ids, the
// usecase decides — per (actor, restaurant) — whether the action is allowed.
//
// The group is additionally mounted behind middleware.RequireRestaurantManager
// as defense-in-depth (a non-staff caller never reaches a handler), but that
// middleware only proves membership; the fine-grained owner/manager/hostess
// gate (e.g. a hostess may run the stop list but not edit the menu) lives in
// the usecase's RBAC matrix.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/admin"
)

// Handler serves the admin-panel endpoints.
type Handler struct{ panel *uc.UseCase }

// NewHandler wires the admin usecase into a handler.
func NewHandler(panel *uc.UseCase) *Handler { return &Handler{panel: panel} }

// RegisterRoutes mounts every admin-panel route under /admin/restaurants/:id.
// Mount on a group running middleware.Auth (+ RequireRestaurantManager, "id").
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	// Restaurant profile.
	rg.GET("/admin/restaurants/:id/profile", h.getProfile)
	rg.PUT("/admin/restaurants/:id/profile", h.updateProfile)

	// Payment settings: the money-path free-cancellation window.
	rg.PUT("/admin/restaurants/:id/payment-settings/free-cancel-window", h.setFreeCancelWindow)

	// Notification settings: the venue's Telegram alert chat.
	rg.GET("/admin/restaurants/:id/notification-settings/telegram", h.getTelegramSettings)
	rg.PUT("/admin/restaurants/:id/notification-settings/telegram", h.setTelegramChat)
	rg.DELETE("/admin/restaurants/:id/notification-settings/telegram", h.clearTelegramChat)

	// Menu.
	rg.GET("/admin/restaurants/:id/menu", h.listMenu)
	rg.GET("/admin/restaurants/:id/menu-categories", h.listCategories)
	rg.POST("/admin/restaurants/:id/menu-items", h.createMenuItem)
	rg.PATCH("/admin/restaurants/:id/menu-items/:itemId", h.updateMenuItem)
	rg.DELETE("/admin/restaurants/:id/menu-items/:itemId", h.deleteMenuItem)
	rg.PATCH("/admin/restaurants/:id/menu-items/:itemId/availability", h.setMenuItemAvailability)

	// Stop-list (fast bulk availability).
	rg.POST("/admin/restaurants/:id/stop-list", h.setStopList)

	// Schedule.
	rg.GET("/admin/restaurants/:id/schedule", h.getSchedule)
	rg.PUT("/admin/restaurants/:id/working-hours", h.setWorkingHours)
	rg.PUT("/admin/restaurants/:id/schedule/overrides", h.setScheduleOverride)
	rg.DELETE("/admin/restaurants/:id/schedule/overrides/:date", h.deleteScheduleOverride)

	// Bookings.
	rg.GET("/admin/restaurants/:id/bookings", h.listBookings)
	rg.POST("/admin/restaurants/:id/bookings/:bookingId/confirm", h.confirmBooking)
	rg.POST("/admin/restaurants/:id/bookings/:bookingId/reject", h.rejectBooking)
	rg.POST("/admin/restaurants/:id/bookings/:bookingId/cancel", h.cancelBooking)
	rg.POST("/admin/restaurants/:id/bookings/:bookingId/no-show", h.noShowBooking)

	// Guests.
	rg.GET("/admin/restaurants/:id/guests", h.listGuests)
}

// ---- Restaurant profile ----------------------------------------------------

func (h *Handler) getProfile(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	a, err := h.panel.GetProfile(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, profileToResponse(a))
}

func (h *Handler) updateProfile(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req profileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	a, err := h.panel.UpdateProfile(c.Request.Context(), actor, rid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, profileToResponse(a))
}

// setFreeCancelWindow updates the venue's money-path free-cancellation window
// (minutes). owner/manager (restaurant.manage), enforced in the usecase.
func (h *Handler) setFreeCancelWindow(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req freeCancelWindowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.FreeCancelWindowMinutes == nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "free_cancel_window_minutes is required")
		return
	}
	if err := h.panel.SetFreeCancelWindow(c.Request.Context(), actor, rid, *req.FreeCancelWindowMinutes); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, freeCancelWindowResponse{FreeCancelWindowMinutes: *req.FreeCancelWindowMinutes})
}

// ---- Notification settings (Telegram) --------------------------------------

// getTelegramSettings returns whether the venue has a Telegram alert chat
// connected and whether the channel is enabled. owner/manager
// (restaurant.manage), enforced in the usecase.
func (h *Handler) getTelegramSettings(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	s, err := h.panel.GetTelegramSettings(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, telegramSettingsResponse{
		Connected:      s.ChatID != "",
		TelegramChatID: s.ChatID,
		Enabled:        s.Enabled,
	})
}

// setTelegramChat connects the venue's Telegram alert chat. For increment 1 the
// staff paste the chat id directly. owner/manager (restaurant.manage).
func (h *Handler) setTelegramChat(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req telegramChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.panel.SetTelegramChatID(c.Request.Context(), actor, rid, req.TelegramChatID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, telegramSettingsResponse{
		Connected:      true,
		TelegramChatID: req.TelegramChatID,
		Enabled:        true,
	})
}

// clearTelegramChat disconnects the venue's Telegram alert chat. Idempotent.
// owner/manager (restaurant.manage).
func (h *Handler) clearTelegramChat(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	if err := h.panel.ClearTelegramChatID(c.Request.Context(), actor, rid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "cleared"})
}

// ---- Menu ------------------------------------------------------------------

func (h *Handler) listMenu(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var lang *string
	if v := c.Query("lang"); v != "" {
		lang = &v
	}
	items, err := h.panel.ListMenu(c.Request.Context(), actor, rid, lang)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]menuItemResponse, 0, len(items))
	for i := range items {
		out = append(out, menuItemToResponse(&items[i]))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) listCategories(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	cats, err := h.panel.ListCategories(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]menuCategoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) createMenuItem(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req menuItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m, err := h.panel.CreateMenuItem(c.Request.Context(), actor, rid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, menuItemToResponse(m))
}

func (h *Handler) updateMenuItem(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	itemID, ok := pathUUID(c, "itemId")
	if !ok {
		return
	}
	var req menuItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m, err := h.panel.UpdateMenuItem(c.Request.Context(), actor, rid, itemID, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, menuItemToResponse(m))
}

func (h *Handler) deleteMenuItem(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	itemID, ok := pathUUID(c, "itemId")
	if !ok {
		return
	}
	if err := h.panel.DeleteMenuItem(c.Request.Context(), actor, rid, itemID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

func (h *Handler) setMenuItemAvailability(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	itemID, ok := pathUUID(c, "itemId")
	if !ok {
		return
	}
	var req availabilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.panel.SetMenuItemAvailability(c.Request.Context(), actor, rid, itemID, req.IsAvailable); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

func (h *Handler) setStopList(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req stopListRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ids, err := req.itemIDs()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	n, err := h.panel.SetStopList(c.Request.Context(), actor, rid, ids, req.Available)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"updated": n})
}

// ---- Schedule --------------------------------------------------------------

func (h *Handler) getSchedule(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	s, err := h.panel.GetSchedule(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, scheduleToResponse(s))
}

func (h *Handler) setWorkingHours(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req workingHoursRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.panel.SetWorkingHours(c.Request.Context(), actor, rid, req.toDomain(rid)); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

func (h *Handler) setScheduleOverride(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req scheduleOverrideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	o, err := h.panel.SetScheduleOverride(c.Request.Context(), actor, rid, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, overrideToResponse(*o))
}

func (h *Handler) deleteScheduleOverride(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	in, err := scheduleOverrideRequest{Date: c.Param("date")}.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	if err := h.panel.DeleteScheduleOverride(c.Request.Context(), actor, rid, in.Date); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deleted"})
}

// ---- Bookings --------------------------------------------------------------

func (h *Handler) listBookings(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	f, err := bookingFilter(c)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items, total, err := h.panel.ListBookings(c.Request.Context(), actor, rid, f)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]bookingResponse, 0, len(items))
	for _, b := range items {
		out = append(out, bookingToResponse(b))
	}
	page, perPage := f.Page, f.PerPage
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 20
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) confirmBooking(c *gin.Context) {
	actor, rid, bid, ok := actorRIDBooking(c)
	if !ok {
		return
	}
	var req reasonRequest
	_ = c.ShouldBindJSON(&req) // body optional
	b, err := h.panel.ConfirmBooking(c.Request.Context(), actor, rid, bid, req.Reason)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

func (h *Handler) rejectBooking(c *gin.Context) {
	actor, rid, bid, ok := actorRIDBooking(c)
	if !ok {
		return
	}
	var req reasonRequest
	_ = c.ShouldBindJSON(&req)
	b, err := h.panel.RejectBooking(c.Request.Context(), actor, rid, bid, req.Reason)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

func (h *Handler) cancelBooking(c *gin.Context) {
	actor, rid, bid, ok := actorRIDBooking(c)
	if !ok {
		return
	}
	var req cancelRequest
	_ = c.ShouldBindJSON(&req)
	b, err := h.panel.CancelBooking(c.Request.Context(), actor, rid, bid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

func (h *Handler) noShowBooking(c *gin.Context) {
	actor, rid, bid, ok := actorRIDBooking(c)
	if !ok {
		return
	}
	var req reasonRequest
	_ = c.ShouldBindJSON(&req)
	b, err := h.panel.NoShowBooking(c.Request.Context(), actor, rid, bid, req.Reason)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

// ---- Guests ----------------------------------------------------------------

func (h *Handler) listGuests(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	guests, err := h.panel.ListGuests(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]guestResponse, 0, len(guests))
	for _, g := range guests {
		out = append(out, guestToResponse(g))
	}
	response.OK(c.Writer, out)
}

// ---- helpers ---------------------------------------------------------------

// actorFrom builds the admin Actor from the authenticated principal, writing
// 401 when the request never passed middleware.Auth.
func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

func pathUUID(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func actorAndRID(c *gin.Context) (uc.Actor, uuid.UUID, bool) {
	actor, ok := actorFrom(c)
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	rid, ok := pathUUID(c, "id")
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	return actor, rid, true
}

func actorRIDBooking(c *gin.Context) (uc.Actor, uuid.UUID, uuid.UUID, bool) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return uc.Actor{}, uuid.Nil, uuid.Nil, false
	}
	bid, ok := pathUUID(c, "bookingId")
	if !ok {
		return uc.Actor{}, uuid.Nil, uuid.Nil, false
	}
	return actor, rid, bid, true
}

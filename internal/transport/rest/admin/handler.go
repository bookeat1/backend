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
	"strings"

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

	// Payment settings: the pre-order policy (offer pre-payment + optional minimum).
	rg.GET("/admin/restaurants/:id/payment-settings/preorder", h.getPreorderSettings)
	rg.PUT("/admin/restaurants/:id/payment-settings/preorder", h.setPreorderSettings)

	// Payment settings: which of an acquirer's accounts this venue's money is
	// routed to (Kaspi: the company inside our Kaspi service). Readable by the
	// venue, writable by the platform only — see uc.SetAcquirerAccount.
	rg.GET("/admin/restaurants/:id/payment-settings/acquirer-account", h.getAcquirerAccount)
	rg.PUT("/admin/restaurants/:id/payment-settings/acquirer-account", h.setAcquirerAccount)

	// Notification settings: the venue's Telegram alert chat.
	rg.GET("/admin/restaurants/:id/notification-settings/telegram", h.getTelegramSettings)
	rg.PUT("/admin/restaurants/:id/notification-settings/telegram", h.setTelegramChat)
	rg.DELETE("/admin/restaurants/:id/notification-settings/telegram", h.clearTelegramChat)
	rg.GET("/admin/restaurants/:id/notification-settings/whatsapp", h.getWhatsAppSettings)
	rg.PUT("/admin/restaurants/:id/notification-settings/whatsapp", h.setWhatsAppPhone)
	rg.DELETE("/admin/restaurants/:id/notification-settings/whatsapp", h.clearWhatsAppPhone)

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

// getPreorderSettings returns the venue's current pre-order policy. owner/manager
// (restaurant.manage), enforced in the usecase.
func (h *Handler) getPreorderSettings(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	v, err := h.panel.GetPreorderSettings(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, preorderSettingsResponse{Enabled: v.Enabled, MinAmountMinor: v.MinAmountMinor})
}

// setPreorderSettings updates the venue's pre-order policy: whether it requires
// pre-payment for pre-ordered dishes and its optional minimum. owner/manager
// (restaurant.manage), enforced in the usecase.
func (h *Handler) setPreorderSettings(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req preorderSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in := uc.PreorderSettingsInput{Enabled: req.Enabled, MinAmountMinor: req.MinAmountMinor}
	if err := h.panel.SetPreorderSettings(c.Request.Context(), actor, rid, in); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, preorderSettingsResponse{Enabled: req.Enabled, MinAmountMinor: req.MinAmountMinor})
}

// ---- Payment settings (acquirer account) -----------------------------------

// getAcquirerAccount reports which of the acquirer's accounts this venue's
// money is routed to. The provider is a required query parameter (?provider=kaspi):
// a venue may be onboarded to more than one acquirer, and defaulting it would
// answer about an acquirer the caller did not ask about. owner/manager, enforced
// in the usecase.
func (h *Handler) getAcquirerAccount(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	provider := domain.PaymentProvider(strings.TrimSpace(c.Query("provider")))
	if provider == "" {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "provider query parameter is required")
		return
	}
	account, err := h.panel.GetAcquirerAccount(c.Request.Context(), actor, rid, provider)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, acquirerAccountToResponse(account))
}

// setAcquirerAccount points a venue's money at one of the acquirer's accounts.
// SUPERADMIN ONLY (enforced in the usecase): this value decides whose account a
// guest's money lands in.
func (h *Handler) setAcquirerAccount(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req acquirerAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	account, err := h.panel.SetAcquirerAccount(c.Request.Context(), actor, rid, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, acquirerAccountToResponse(account))
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

// getWhatsAppSettings returns whether the venue has a WhatsApp number connected
// for booking alerts. owner/manager (restaurant.manage), enforced in the usecase.
func (h *Handler) getWhatsAppSettings(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	s, err := h.panel.GetWhatsAppSettings(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, whatsAppSettingsResponse{
		Connected:     s.Phone != "",
		WhatsAppPhone: s.Phone,
		Enabled:       s.Enabled,
	})
}

// setWhatsAppPhone connects the venue's WhatsApp number. Answers with the
// NORMALIZED number the server stored, not the one that was typed — otherwise
// the panel would keep showing "8 701 …" while inbound presses are matched
// against "+7 701 …", and the difference would only surface as silence.
func (h *Handler) setWhatsAppPhone(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	var req whatsAppPhoneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	stored, err := h.panel.SetWhatsAppPhone(c.Request.Context(), actor, rid, req.WhatsAppPhone)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, whatsAppSettingsResponse{
		Connected:     true,
		WhatsAppPhone: stored,
		Enabled:       true,
	})
}

// clearWhatsAppPhone disconnects the venue's WhatsApp number. Idempotent.
func (h *Handler) clearWhatsAppPhone(c *gin.Context) {
	actor, rid, ok := actorAndRID(c)
	if !ok {
		return
	}
	if err := h.panel.ClearWhatsAppPhone(c.Request.Context(), actor, rid); err != nil {
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
	// ?lang= is accepted and ignored on purpose: the cabinet must always show
	// every dish it can edit, and it renders the *_i18n maps itself. Filtering
	// the editor's rows by language is exactly the bug the guest menu had.
	items, err := h.panel.ListMenu(c.Request.Context(), actor, rid)
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
	// Состав предзаказа берём ОДНИМ запросом на всю страницу: у хостес этот
	// список открыт весь вечер, и сотня отдельных запросов на сотню броней
	// превратила бы его в самый дорогой экран кабинета.
	ids := make([]uuid.UUID, 0, len(items))
	for _, b := range items {
		ids = append(ids, b.ID)
	}
	preorders, err := h.panel.ListBookingPreorders(c.Request.Context(), actor, rid, ids)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]bookingResponse, 0, len(items))
	for _, b := range items {
		r := bookingToResponse(b)
		r.Preorder = preorderToResponse(preorders[b.ID])
		out = append(out, r)
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

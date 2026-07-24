// Package tickets exposes the HTTP endpoints for event-ticket purchasing.
//
// Guest routes (buy, view own, cancel) mount on a group running
// middleware.OptionalAuth: a logged-in guest is attached, an anonymous buyer is
// let through (the transport builds a zero Actor, the usecase enforces contact
// scoping) — the same access model as the payments checkout, since a ticket
// purchase creates a payment. Admin routes (tickets sold for an event) mount on
// a group running middleware.Auth; the RBAC gate (PermRestaurantManage at the
// event's own restaurant) is resolved inside usecase/tickets, so transport only
// builds the Actor and parses ids. Acquirer webhooks are handled by the existing
// payments handler (ticket payments share the same webhook flow).
package tickets

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/tickets"
)

const (
	idempotencyHeader = "Idempotency-Key"
	defaultPerPage    = 20
	maxPerPage        = 100
	// callbackPath must equal the payments handler's freedomPayCallbackPath so a
	// ticket payment's webhook lands on the same registered route.
	callbackPath = "/webhooks/payments/freedompay"
)

// Handler serves the ticket endpoints.
type Handler struct {
	purchase      uc.PurchaseUseCase
	refund        uc.RefundUseCase
	admin         uc.AdminUseCase
	mine          uc.MyTicketsUseCase
	publicBaseURL string
}

// NewHandler wires the ticket usecases into a handler.
func NewHandler(purchase uc.PurchaseUseCase, refund uc.RefundUseCase, admin uc.AdminUseCase, mine uc.MyTicketsUseCase, publicBaseURL string) *Handler {
	base := publicBaseURL
	if n := len(base); n > 0 && base[n-1] == '/' {
		base = base[:n-1]
	}
	return &Handler{purchase: purchase, refund: refund, admin: admin, mine: mine, publicBaseURL: base}
}

// RegisterGuestRoutes mounts buy / view-own / cancel. Mount on an OptionalAuth
// group.
func (h *Handler) RegisterGuestRoutes(rg *gin.RouterGroup) {
	rg.POST("/events/:eventId/tickets", h.purchaseTickets)
	rg.POST("/tickets/:ticketId/refund", h.refundTicket)
	rg.GET("/me/tickets", h.listMine)
}

// RegisterAdminRoutes mounts the tickets-sold cabinet views. Mount on an Auth
// group; authorization is enforced in the usecase.
func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/events/:eventId/tickets", h.listSold)
	rg.GET("/admin/events/:eventId/tickets/summary", h.summary)
}

type purchaseRequest struct {
	Quantity   int    `json:"quantity"`
	GuestName  string `json:"guest_name"`
	GuestPhone string `json:"guest_phone"`
	GuestEmail string `json:"guest_email"`
	ReturnURL  string `json:"return_url"`
}

func (h *Handler) purchaseTickets(c *gin.Context) {
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	var req purchaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if h.publicBaseURL == "" {
		response.HandleError(c.Writer, domain.ErrValidation)
		return
	}
	res, err := h.purchase.Purchase(c.Request.Context(), actorFrom(c), uc.PurchaseInput{
		EventID:        eid,
		Quantity:       req.Quantity,
		GuestName:      req.GuestName,
		GuestPhone:     phone.Normalize(req.GuestPhone),
		GuestEmail:     req.GuestEmail,
		IdempotencyKey: c.GetHeader(idempotencyHeader),
		ReturnURL:      req.ReturnURL,
		CallbackURL:    h.publicBaseURL + callbackPath,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, purchaseToResponse(res))
}

type refundRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) refundTicket(c *gin.Context) {
	tid, ok := pathUUID(c, "ticketId", "invalid ticket id")
	if !ok {
		return
	}
	var req refundRequest
	_ = c.ShouldBindJSON(&req) // body optional
	var reason *string
	if req.Reason != "" {
		reason = &req.Reason
	}
	t, err := h.refund.Refund(c.Request.Context(), actorFrom(c), tid, uc.RefundInput{
		Reason:         reason,
		IdempotencyKey: c.GetHeader(idempotencyHeader),
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, ticketToResponse(*t))
}

func (h *Handler) listMine(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	page, perPage := pagination(c)
	items, total, err := h.mine.ListMine(c.Request.Context(), au.ID, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]ticketResponse, 0, len(items))
	for _, t := range items {
		out = append(out, ticketToResponse(t))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) listSold(c *gin.Context) {
	actor, ok := staffActor(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	statuses := parseStatuses(c.Query("status"))
	page, perPage := pagination(c)
	items, total, err := h.admin.ListSold(c.Request.Context(), actor, eid, statuses, page, perPage)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]ticketResponse, 0, len(items))
	for _, t := range items {
		out = append(out, ticketToResponse(t))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) summary(c *gin.Context) {
	actor, ok := staffActor(c)
	if !ok {
		return
	}
	eid, ok := pathUUID(c, "eventId", "invalid event id")
	if !ok {
		return
	}
	counts, err := h.admin.Counts(c.Request.Context(), actor, eid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, countsToResponse(counts))
}

// actorFrom builds an Actor from whatever OptionalAuth attached, or the zero
// Actor (anonymous guest). Never rejects — the usecase enforces scoping.
func actorFrom(c *gin.Context) uc.Actor {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		return uc.Actor{}
	}
	id := au.ID
	return uc.Actor{UserID: &id, Role: domain.Role(au.Role)}
}

// staffActor requires an authenticated principal (the admin routes run under
// middleware.Auth), 401 otherwise.
func staffActor(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	id := au.ID
	return uc.Actor{UserID: &id, Role: domain.Role(au.Role)}, true
}

func pathUUID(c *gin.Context, param, msg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, msg)
		return uuid.Nil, false
	}
	return id, true
}

func pagination(c *gin.Context) (page, perPage int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

func parseStatuses(raw string) []domain.TicketStatus {
	if raw == "" {
		return nil
	}
	var out []domain.TicketStatus
	for _, part := range splitComma(raw) {
		s := domain.TicketStatus(part)
		if s.Valid() {
			out = append(out, s)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

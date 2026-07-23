// Package bookings exposes the reservation HTTP endpoints: the public
// availability calendar, the guest's own bookings, and the venue cabinet.
//
// Authorization split (spec §7):
//
//   - the availability endpoint is public;
//   - restaurant-scoped routes (/restaurants/:id/bookings, .../blacklist) are
//     mounted behind middleware.RequireRestaurantManager, which resolves the
//     venue from the path;
//   - booking-scoped routes (/bookings/:id/...) cannot use that middleware —
//     the path carries a booking id, not a restaurant id. They are authorized
//     inside the usecase, which loads the booking and resolves the caller's
//     relation to it: the owner, a manager of that venue, an admin, or nobody.
//     A guest asking for someone else's booking gets 404 (no enumeration
//     oracle); a manager of another venue gets 403.
package bookings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/bookings"
)

// maxBodyBytes caps the JSON body of a booking request. The body is read in
// full (it is hashed for idempotency), so it must be bounded.
const maxBodyBytes = 256 << 10

// idempotencyHeader is the retry token required on POST /bookings (spec §7).
const idempotencyHeader = "Idempotency-Key"

// Handler serves the booking endpoints.
type Handler struct {
	facade     uc.Facade
	create     uc.CreateUseCase
	idempotent uc.IdempotentCreateUseCase
	status     uc.StatusUseCase
	update     uc.UpdateUseCase
	avail      uc.AvailabilityUseCase
	blacklist  uc.BlacklistUseCase
	policy     uc.PolicyUseCase
	external   uc.ExternalReservationUseCase
}

// NewHandler wires the booking usecases into a handler.
func NewHandler(
	facade uc.Facade,
	create uc.CreateUseCase,
	idempotent uc.IdempotentCreateUseCase,
	status uc.StatusUseCase,
	update uc.UpdateUseCase,
	avail uc.AvailabilityUseCase,
	blacklist uc.BlacklistUseCase,
	policy uc.PolicyUseCase,
	external uc.ExternalReservationUseCase,
) *Handler {
	return &Handler{
		facade: facade, create: create, idempotent: idempotent, status: status,
		update: update, avail: avail, blacklist: blacklist, policy: policy,
		external: external,
	}
}

// RegisterPublic mounts the unauthenticated availability calendar. The storefront
// needs it before login.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/availability", h.availability)
}

// RegisterRoutes mounts the authenticated, booking-scoped routes. Mount on a
// group running middleware.Auth. Guest and staff actions share the group on
// purpose: which of the two the caller is depends on the booking, not on the
// route, and only the usecase can tell (see the package comment).
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/bookings", h.createMine)
	rg.GET("/bookings", h.listMine)
	rg.GET("/bookings/:id", h.get)
	rg.GET("/bookings/:id/history", h.history)
	rg.POST("/bookings/:id/cancel", h.cancel)
	rg.GET("/bookings/:id/messages", h.listMessages)
	rg.POST("/bookings/:id/messages", h.postMessage)
	rg.POST("/bookings/:id/messages/read", h.markMessagesRead)
	rg.GET("/bookings/:id/survey", h.getSurvey)
	rg.POST("/bookings/:id/survey", h.submitSurvey)

	// Venue actions on one booking. Authorized in the usecase against the
	// booking's restaurant — a guest hitting these gets 403.
	rg.PATCH("/bookings/:id", h.patch)
	rg.POST("/bookings/:id/confirm", h.confirm)
	rg.POST("/bookings/:id/reject", h.reject)
	rg.POST("/bookings/:id/arrive", h.arrive)
	rg.POST("/bookings/:id/complete", h.complete)
	rg.POST("/bookings/:id/no-show", h.noShow)
	rg.POST("/bookings/:id/waitlist", h.waitlist)
}

// RegisterRestaurantScoped mounts the venue cabinet. Mount on a group running
// middleware.RequireRestaurantManager(..., "id").
func (h *Handler) RegisterRestaurantScoped(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/bookings", h.listByRestaurant)
	rg.POST("/restaurants/:id/bookings", h.createByStaff)
	rg.GET("/restaurants/:id/blacklist", h.listBlacklist)
	rg.POST("/restaurants/:id/blacklist", h.addBlacklist)
	rg.DELETE("/restaurants/:id/blacklist/:entryID", h.removeBlacklist)
	rg.GET("/restaurants/:id/booking-policy", h.getBookingPolicy)
	rg.PATCH("/restaurants/:id/booking-policy", h.patchBookingPolicy)

	// External occupancy holds: slots taken through the venue's OTHER funnels
	// (phone, walk-in, POS/Kwaaka). Staff-facing today, the same seam a future
	// POS webhook writes into. Enforced by booking.manage inside the usecase.
	rg.GET("/restaurants/:id/external-reservations", h.listExternalHolds)
	rg.POST("/restaurants/:id/external-reservations", h.createExternalHold)
	rg.DELETE("/restaurants/:id/external-reservations/:holdID", h.removeExternalHold)
}

// getBookingPolicy returns the venue's stored policy overrides plus the
// effective policy they resolve to.
func (h *Handler) getBookingPolicy(c *gin.Context) {
	actor, rid, ok := actorAndID(c)
	if !ok {
		return
	}
	view, err := h.policy.Get(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, policyToResponse(rid.String(), view))
}

// patchBookingPolicy edits the venue's level-2 policy overrides (spec §4.2):
// this is the only way a restaurant can turn off its own auto-confirmation or
// lengthen its confirmation SLA. Omitted fields are left untouched.
func (h *Handler) patchBookingPolicy(c *gin.Context) {
	actor, rid, ok := actorAndID(c)
	if !ok {
		return
	}
	var req bookingPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	view, err := h.policy.Update(c.Request.Context(), actor, rid, req.toDomain())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, policyToResponse(rid.String(), view))
}

func (h *Handler) availability(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	guests, err := strconv.Atoi(c.DefaultQuery("guests", "2"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid guests")
		return
	}
	day, err := h.avail.Day(c.Request.Context(), id, c.Query("date"), guests)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, availabilityToResponse(day))
}

// createMine is the guest self-service booking. The owner, the source and the
// placement fields are set here, not taken from the body: a guest must not be
// able to book on behalf of someone else, pass themselves off as the venue, or
// pin a table (spec §4.2 — manual placement is staff-only).
func (h *Handler) createMine(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	body, ok := readBody(c)
	if !ok {
		return
	}
	var req createBookingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid JSON body")
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	uid := actor.UserID
	in.UserID = &uid
	in.Source = domain.SourceApp
	in.TableIDs = nil
	in.Force = false

	details, err := h.idempotent.CreateIdempotent(c.Request.Context(), actor, uc.IdempotencyKey{
		Key:         c.GetHeader(idempotencyHeader),
		RequestHash: hashBody(body),
	}, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// A replay returns 201 with the original payload: the resource did come
	// into existence as a result of this (logical) request.
	response.Created(c.Writer, detailsToResponse(details))
}

// createByStaff is the manual booking taken at the venue (phone, walk-in). The
// restaurant comes from the path — the middleware already proved the caller
// manages it — so a body-supplied restaurant_id can never widen access.
func (h *Handler) createByStaff(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req createBookingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	req.RestaurantID = rid.String()
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	if req.Source == nil || *req.Source == "" {
		in.Source = domain.SourceAdmin
	}
	details, err := h.create.Create(c.Request.Context(), actor, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, detailsToResponse(details))
}

func (h *Handler) listMine(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	f, err := bookingFilter(c)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items, total, err := h.facade.ListMine(c.Request.Context(), actor, f)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	writePage(c, items, total, f)
}

func (h *Handler) listByRestaurant(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	f, err := bookingFilter(c)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items, total, err := h.facade.ListByRestaurant(c.Request.Context(), actor, rid, f)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	writePage(c, items, total, f)
}

func (h *Handler) get(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	details, err := h.facade.Get(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, detailsToResponse(details))
}

func (h *Handler) history(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	changes, err := h.facade.History(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]statusChangeResponse, 0, len(changes))
	for _, ch := range changes {
		out = append(out, statusChangeToResponse(ch))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) patch(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	var req updateBookingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	details, err := h.update.Update(c.Request.Context(), actor, id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, detailsToResponse(details))
}

func (h *Handler) cancel(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	var req cancelRequest
	if !bindOptionalJSON(c, &req) {
		return
	}
	b, err := h.status.Cancel(c.Request.Context(), actor, id,
		uc.CancelInput{ReasonCode: req.ReasonCode, Reason: req.Reason})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

func (h *Handler) confirm(c *gin.Context) { h.transition(c, h.status.Confirm) }
func (h *Handler) reject(c *gin.Context)  { h.transition(c, h.status.Reject) }
func (h *Handler) noShow(c *gin.Context)  { h.transition(c, h.status.NoShow) }
func (h *Handler) waitlist(c *gin.Context) {
	h.transition(c, h.status.Waitlist)
}

func (h *Handler) arrive(c *gin.Context) {
	h.transition(c, func(ctx context.Context, actor uc.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
		return h.status.Arrive(ctx, actor, id)
	})
}

func (h *Handler) complete(c *gin.Context) {
	h.transition(c, func(ctx context.Context, actor uc.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
		return h.status.Complete(ctx, actor, id)
	})
}

// transition is the shared body of the status endpoints: parse, call, envelope.
func (h *Handler) transition(c *gin.Context, fn transitionFunc) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	var req reasonRequest
	if !bindOptionalJSON(c, &req) {
		return
	}
	b, err := fn(c.Request.Context(), actor, id, req.Reason)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, bookingToResponse(*b))
}

func (h *Handler) listMessages(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	msgs, err := h.facade.Messages(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]messageResponse, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageToResponse(m))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) postMessage(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	var req messageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	m, err := h.facade.PostMessage(c.Request.Context(), actor, id, req.Message)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, messageToResponse(*m))
}

func (h *Handler) markMessagesRead(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	n, err := h.facade.MarkMessagesRead(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"marked_read": n})
}

func (h *Handler) getSurvey(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	s, err := h.facade.Survey(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, surveyToResponse(s))
}

func (h *Handler) submitSurvey(c *gin.Context) {
	actor, id, ok := actorAndID(c)
	if !ok {
		return
	}
	var req surveyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s, err := h.facade.SubmitSurvey(c.Request.Context(), actor, id, req.toInput())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, surveyToResponse(s))
}

func (h *Handler) listBlacklist(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	entries, err := h.blacklist.List(c.Request.Context(), actor, rid)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]blacklistResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, blacklistToResponse(e))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) addBlacklist(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req blacklistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	e, err := h.blacklist.Add(c.Request.Context(), actor, rid, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, blacklistToResponse(*e))
}

func (h *Handler) removeBlacklist(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	eid, ok := pathID(c, "entryID")
	if !ok {
		return
	}
	if err := h.blacklist.Remove(c.Request.Context(), actor, rid, eid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "removed"})
}

// transitionFunc is the shape of every StatusUseCase method: some take a
// reason, some ignore it.
type transitionFunc func(ctx context.Context, actor uc.Actor, id uuid.UUID, reason *string) (*domain.Booking, error)

// actorFrom builds the usecase Actor from the authenticated principal. It
// writes 401 and reports false when the request never passed middleware.Auth.
func actorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

// pathID parses a uuid path parameter, writing 422 on failure.
func pathID(c *gin.Context, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func actorAndID(c *gin.Context) (uc.Actor, uuid.UUID, bool) {
	actor, ok := actorFrom(c)
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	id, ok := pathID(c, "id")
	if !ok {
		return uc.Actor{}, uuid.Nil, false
	}
	return actor, id, true
}

// bindOptionalJSON decodes a body that may legitimately be absent (a cancel or
// a confirm without a reason). An empty body is fine; a malformed one is not.
func bindOptionalJSON(c *gin.Context, dst any) bool {
	body, ok := readBody(c)
	if !ok {
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid JSON body")
		return false
	}
	return true
}

// readBody reads the whole request body under a size cap. The bytes are needed
// verbatim for the idempotency hash, so they cannot be streamed into the
// decoder.
func readBody(c *gin.Context) ([]byte, bool) {
	if c.Request.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "cannot read request body")
		return nil, false
	}
	if len(body) > maxBodyBytes {
		response.Error(c.Writer, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	return body, true
}

// hashBody is the idempotency fingerprint of a request: the raw bytes, so that
// "the same request" means byte-identical, not "equal after our own defaulting".
func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func writePage(c *gin.Context, items []domain.Booking, total int, f domain.BookingFilter) {
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

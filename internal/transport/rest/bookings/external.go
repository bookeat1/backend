package bookings

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/bookings"
)

// externalHoldRequest is the body of POST /restaurants/{id}/external-reservations:
// staff record occupancy that came in through another funnel (phone, walk-in,
// POS). table_id omitted / null = a whole-venue block for the window.
type externalHoldRequest struct {
	TableID     *string   `json:"table_id"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	Source      *string   `json:"source"`
	ExternalRef *string   `json:"external_ref"`
	Note        *string   `json:"note"`
}

func (r externalHoldRequest) toInput() (uc.ExternalHoldInput, error) {
	in := uc.ExternalHoldInput{
		StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		ExternalRef: r.ExternalRef, Note: r.Note,
	}
	tableID, err := parseOptionalUUID(r.TableID, "table_id")
	if err != nil {
		return uc.ExternalHoldInput{}, err
	}
	in.TableID = tableID
	if r.Source != nil {
		in.Source = domain.ExternalSource(*r.Source)
	}
	return in, nil
}

type externalHoldResponse struct {
	ID           string    `json:"id"`
	RestaurantID string    `json:"restaurant_id"`
	TableID      *string   `json:"table_id"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	Source       string    `json:"source"`
	ExternalRef  *string   `json:"external_ref"`
	Note         *string   `json:"note"`
	CreatedBy    *string   `json:"created_by"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
}

func externalHoldToResponse(r domain.ExternalReservation) externalHoldResponse {
	out := externalHoldResponse{
		ID:           r.ID.String(),
		RestaurantID: r.RestaurantID.String(),
		StartsAt:     r.StartsAt,
		EndsAt:       r.EndsAt,
		Source:       string(r.Source),
		ExternalRef:  r.ExternalRef,
		Note:         r.Note,
		Active:       r.Active,
		CreatedAt:    r.CreatedAt,
	}
	if r.TableID != nil {
		s := r.TableID.String()
		out.TableID = &s
	}
	if r.CreatedBy != nil {
		s := r.CreatedBy.String()
		out.CreatedBy = &s
	}
	return out
}

// createExternalHold records outside occupancy so the availability engine and
// booking creation stop reselling the slot. Mounted behind
// RequireRestaurantManager; the usecase additionally enforces booking.manage.
func (h *Handler) createExternalHold(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req externalHoldRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	res, err := h.external.Create(c.Request.Context(), actor, rid, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, externalHoldToResponse(*res))
}

// listExternalHolds returns the active holds overlapping [from, to). Both query
// params are required RFC3339 timestamps — this is a cabinet view over a chosen
// window, not an unbounded dump.
func (h *Handler) listExternalHolds(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	from, err := time.Parse(time.RFC3339, c.Query("from"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "from must be an RFC3339 timestamp")
		return
	}
	to, err := time.Parse(time.RFC3339, c.Query("to"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "to must be an RFC3339 timestamp")
		return
	}
	holds, err := h.external.List(c.Request.Context(), actor, rid, from, to)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]externalHoldResponse, 0, len(holds))
	for _, hld := range holds {
		out = append(out, externalHoldToResponse(hld))
	}
	response.OK(c.Writer, out)
}

// removeExternalHold deletes a hold; its enforcement rows cascade away, freeing
// the slot for both the availability engine and new bookings.
func (h *Handler) removeExternalHold(c *gin.Context) {
	actor, ok := actorFrom(c)
	if !ok {
		return
	}
	rid, ok := pathID(c, "id")
	if !ok {
		return
	}
	hid, ok := pathID(c, "holdID")
	if !ok {
		return
	}
	if err := h.external.Delete(c.Request.Context(), actor, rid, hid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "removed"})
}

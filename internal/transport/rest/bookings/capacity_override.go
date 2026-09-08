package bookings

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
)

// capacityOverrideResponse is one deliberate overbooking as the venue cabinet
// sees it. Every number the owner needs to judge the event is here, so the
// client does not have to join anything to render the list.
type capacityOverrideResponse struct {
	ID        string `json:"id"`
	BookingID string `json:"booking_id"`
	// Who did it. actor_type is "manager" or "admin" — a guest can never appear
	// here, at the usecase level and again as a DB CHECK.
	ActorUserID string `json:"actor_user_id"`
	ActorType   string `json:"actor_type"`
	Guests      int    `json:"guests"`
	// DeclaredSeats is the venue's capacity AS IT WAS at that moment, not as it
	// is today: the audit has to keep saying what the room was.
	DeclaredSeats int `json:"declared_seats"`
	// SeatsOver / PeakSeatsTaken describe the worst quarter-hour of the visit:
	// "12 people in a 10-seat room" is seats_over=2, peak_seats_taken=12.
	SeatsOver       int       `json:"seats_over"`
	PeakBucketStart time.Time `json:"peak_bucket_start"`
	PeakSeatsTaken  int       `json:"peak_seats_taken"`
	CreatedAt       time.Time `json:"created_at"`
}

func capacityOverrideToResponse(o domain.BookingCapacityOverride) capacityOverrideResponse {
	return capacityOverrideResponse{
		ID: o.ID.String(), BookingID: o.BookingID.String(),
		ActorUserID: o.ActorUserID.String(), ActorType: string(o.ActorType),
		Guests: o.Guests, DeclaredSeats: o.DeclaredSeats, SeatsOver: o.SeatsOver,
		PeakBucketStart: o.PeakBucketStart, PeakSeatsTaken: o.PeakSeatsTaken,
		CreatedAt: o.CreatedAt,
	}
}

// listCapacityOverrides answers "who seated a party beyond our capacity, and
// when". Both query params are required RFC3339 timestamps and they bound the
// OVERLOADED moment, not the moment the booking was entered — the owner asks
// about last Saturday evening, not about the Tuesday it was booked on.
func (h *Handler) listCapacityOverrides(c *gin.Context) {
	actor, rid, ok := actorAndID(c)
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
	rows, err := h.overrides.List(c.Request.Context(), actor, rid, from, to)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]capacityOverrideResponse, 0, len(rows))
	for _, o := range rows {
		out = append(out, capacityOverrideToResponse(o))
	}
	response.OK(c.Writer, out)
}

package bookings

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/bookings"
)

// createBookingRequest is the body of POST /bookings and of the manager's
// POST /restaurants/{id}/bookings. Fields the caller is not allowed to set are
// not stripped here but in the handler, where the caller's role is known.
type createBookingRequest struct {
	RestaurantID string               `json:"restaurant_id"`
	UserID       *string              `json:"user_id"`
	Name         string               `json:"name"`
	Phone        string               `json:"phone"`
	Email        string               `json:"email"`
	Guests       int                  `json:"guests"`
	StartsAt     time.Time            `json:"starts_at"`
	Notes        *string              `json:"notes"`
	Source       *string              `json:"source"`
	PromotionID  *string              `json:"promotion_id"`
	EventID      *string              `json:"event_id"`
	Items        []bookingItemRequest `json:"items"`
	TableIDs     []string             `json:"table_ids"`
	Force        bool                 `json:"force"`
}

type bookingItemRequest struct {
	MenuItemID *string `json:"menu_item_id"`
	Name       string  `json:"name"`
	PriceMinor int64   `json:"price_minor"`
	Currency   string  `json:"currency"`
	Quantity   int     `json:"quantity"`
	Comment    *string `json:"comment"`
}

func (r createBookingRequest) toInput() (uc.CreateInput, error) {
	in := uc.CreateInput{
		Name: r.Name, Phone: r.Phone, Email: r.Email, Guests: r.Guests,
		StartsAt: r.StartsAt, Notes: r.Notes, Force: r.Force,
		Source: domain.SourceApp,
	}
	var err error
	if in.RestaurantID, err = parseUUID(r.RestaurantID, "restaurant_id"); err != nil {
		return uc.CreateInput{}, err
	}
	if in.UserID, err = parseOptionalUUID(r.UserID, "user_id"); err != nil {
		return uc.CreateInput{}, err
	}
	if in.PromotionID, err = parseOptionalUUID(r.PromotionID, "promotion_id"); err != nil {
		return uc.CreateInput{}, err
	}
	if in.EventID, err = parseOptionalUUID(r.EventID, "event_id"); err != nil {
		return uc.CreateInput{}, err
	}
	if r.Source != nil && *r.Source != "" {
		in.Source = domain.BookingSource(*r.Source)
	}
	if in.TableIDs, err = parseUUIDs(r.TableIDs, "table_ids"); err != nil {
		return uc.CreateInput{}, err
	}
	for _, it := range r.Items {
		menuID, err := parseOptionalUUID(it.MenuItemID, "menu_item_id")
		if err != nil {
			return uc.CreateInput{}, err
		}
		in.Items = append(in.Items, uc.ItemInput{
			MenuItemID: menuID, Name: it.Name, PriceMinor: it.PriceMinor,
			Currency: it.Currency, Quantity: it.Quantity, Comment: it.Comment,
		})
	}
	return in, nil
}

// updateBookingRequest is the body of PATCH /bookings/{id}. Absent fields are
// left untouched; table_ids present (even empty) replaces the placement.
type updateBookingRequest struct {
	StartsAt *time.Time `json:"starts_at"`
	Guests   *int       `json:"guests"`
	Notes    *string    `json:"notes"`
	TableIDs *[]string  `json:"table_ids"`
	Force    bool       `json:"force"`
}

func (r updateBookingRequest) toInput() (uc.UpdateInput, error) {
	in := uc.UpdateInput{StartsAt: r.StartsAt, Guests: r.Guests, Notes: r.Notes, Force: r.Force}
	if r.TableIDs != nil {
		ids, err := parseUUIDs(*r.TableIDs, "table_ids")
		if err != nil {
			return uc.UpdateInput{}, err
		}
		if ids == nil {
			ids = []uuid.UUID{} // explicit "no tables", not "unchanged"
		}
		in.TableIDs = ids
	}
	return in, nil
}

type cancelRequest struct {
	ReasonCode *string `json:"reason_code"`
	Reason     *string `json:"reason"`
}

type reasonRequest struct {
	Reason *string `json:"reason"`
}

type messageRequest struct {
	Message string `json:"message"`
}

type surveyRequest struct {
	RatingOverall  int     `json:"rating_overall"`
	RatingFood     int     `json:"rating_food"`
	RatingService  int     `json:"rating_service"`
	RatingAmbience int     `json:"rating_ambience"`
	NPS            int     `json:"nps"`
	Comment        *string `json:"comment"`
	Dismissed      bool    `json:"dismissed"`
}

func (r surveyRequest) toInput() uc.SurveyInput {
	return uc.SurveyInput{
		RatingOverall: r.RatingOverall, RatingFood: r.RatingFood,
		RatingService: r.RatingService, RatingAmbience: r.RatingAmbience,
		NPS: r.NPS, Comment: r.Comment, Dismissed: r.Dismissed,
	}
}

type blacklistRequest struct {
	UserID *string `json:"user_id"`
	Phone  string  `json:"phone"`
	Email  string  `json:"email"`
	Reason *string `json:"reason"`
}

func (r blacklistRequest) toInput() (uc.BlacklistInput, error) {
	userID, err := parseOptionalUUID(r.UserID, "user_id")
	if err != nil {
		return uc.BlacklistInput{}, err
	}
	return uc.BlacklistInput{UserID: userID, Phone: r.Phone, Email: r.Email, Reason: r.Reason}, nil
}

// bookingFilter builds a listing filter from the query string. Unparseable
// values are rejected rather than silently ignored: a client filtering by a
// misspelled status must not get the unfiltered list back.
func bookingFilter(c *gin.Context) (domain.BookingFilter, error) {
	var f domain.BookingFilter
	for _, raw := range c.QueryArray("status") {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				f.Statuses = append(f.Statuses, domain.BookingStatus(s))
			}
		}
	}
	from, err := parseTimeQuery(c.Query("from"))
	if err != nil {
		return f, fmt.Errorf("%w: from must be a date or RFC3339 timestamp", domain.ErrValidation)
	}
	to, err := parseTimeQuery(c.Query("to"))
	if err != nil {
		return f, fmt.Errorf("%w: to must be a date or RFC3339 timestamp", domain.ErrValidation)
	}
	f.From, f.To = from, to
	// A bare ?date=YYYY-MM-DD is the venue calendar's default view: one day,
	// half-open, so the (restaurant_id, starts_at) index is usable.
	if d := c.Query("date"); d != "" && f.From == nil && f.To == nil {
		day, err := time.Parse(uc.DateLayout, d)
		if err != nil {
			return f, fmt.Errorf("%w: date must be YYYY-MM-DD", domain.ErrValidation)
		}
		next := day.AddDate(0, 0, 1)
		f.From, f.To = &day, &next
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))
	return f, nil
}

// parseTimeQuery accepts either a calendar date or a full RFC3339 timestamp.
func parseTimeQuery(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	t, err := time.Parse(uc.DateLayout, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func parseUUID(v, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: invalid %s", domain.ErrValidation, field)
	}
	return id, nil
}

func parseOptionalUUID(v *string, field string) (*uuid.UUID, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil, nil
	}
	id, err := parseUUID(*v, field)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func parseUUIDs(vs []string, field string) ([]uuid.UUID, error) {
	if len(vs) == 0 {
		return nil, nil
	}
	out := make([]uuid.UUID, 0, len(vs))
	for _, v := range vs {
		id, err := parseUUID(v, field)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// bookingPolicyRequest is the body of PATCH /restaurants/{id}/booking-policy.
// Every field is a pointer so an absent JSON key ("don't touch this column")
// is distinguishable from an explicit zero ("no buffer", "auto-confirm off") —
// the difference between inheriting the global default and overriding it with 0.
type bookingPolicyRequest struct {
	Timezone               *string `json:"timezone"`
	BookingDurationMinutes *int    `json:"booking_duration_minutes"`
	BookingBufferMinutes   *int    `json:"booking_buffer_minutes"`
	BookingLeadMinutes     *int    `json:"booking_lead_minutes"`
	BookingHorizonDays     *int    `json:"booking_horizon_days"`
	CancelDeadlineMinutes  *int    `json:"cancel_deadline_minutes"`
	ConfirmSLAMinutes      *int    `json:"confirm_sla_minutes"`
	MaxGuestsPerBooking    *int    `json:"max_guests_per_booking"`
	AutoConfirm            *bool   `json:"auto_confirm"`
}

// Validate rejects a body that would patch nothing. Range checks live in the
// usecase (single source of truth, reachable from any transport).
func (r bookingPolicyRequest) Validate() error {
	if r.Timezone == nil && r.BookingDurationMinutes == nil && r.BookingBufferMinutes == nil &&
		r.BookingLeadMinutes == nil && r.BookingHorizonDays == nil && r.CancelDeadlineMinutes == nil &&
		r.ConfirmSLAMinutes == nil && r.MaxGuestsPerBooking == nil && r.AutoConfirm == nil {
		return fmt.Errorf("%w: no policy fields provided", domain.ErrValidation)
	}
	return nil
}

func (r bookingPolicyRequest) toDomain() domain.BookingPolicyOverride {
	return domain.BookingPolicyOverride{
		Timezone:               r.Timezone,
		BookingDurationMinutes: r.BookingDurationMinutes,
		BookingBufferMinutes:   r.BookingBufferMinutes,
		BookingLeadMinutes:     r.BookingLeadMinutes,
		BookingHorizonDays:     r.BookingHorizonDays,
		CancelDeadlineMinutes:  r.CancelDeadlineMinutes,
		ConfirmSLAMinutes:      r.ConfirmSLAMinutes,
		MaxGuestsPerBooking:    r.MaxGuestsPerBooking,
		AutoConfirm:            r.AutoConfirm,
	}
}

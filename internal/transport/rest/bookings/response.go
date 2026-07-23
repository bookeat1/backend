package bookings

import (
	"time"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/bookings"
)

type bookingResponse struct {
	ID                     string     `json:"id"`
	RestaurantID           string     `json:"restaurant_id"`
	UserID                 *string    `json:"user_id"`
	Name                   string     `json:"name"`
	Phone                  string     `json:"phone"`
	Email                  string     `json:"email"`
	Guests                 int        `json:"guests"`
	StartsAt               time.Time  `json:"starts_at"`
	EndsAt                 time.Time  `json:"ends_at"`
	Status                 string     `json:"status"`
	Source                 string     `json:"source"`
	Notes                  *string    `json:"notes"`
	PromotionID            *string    `json:"promotion_id"`
	EventID                *string    `json:"event_id"`
	CreatedByAdmin         bool       `json:"created_by_admin"`
	ForcedPlacement        bool       `json:"forced_placement"`
	ConfirmedAt            *time.Time `json:"confirmed_at"`
	ArrivedAt              *time.Time `json:"arrived_at"`
	CancelledAt            *time.Time `json:"cancelled_at"`
	CancelledBy            *string    `json:"cancelled_by"`
	CancellationReasonCode *string    `json:"cancellation_reason_code"`
	CancellationReason     *string    `json:"cancellation_reason"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type bookingDetailsResponse struct {
	bookingResponse
	Items  []bookingItemResponse  `json:"items"`
	Tables []bookingTableResponse `json:"tables"`
}

type bookingItemResponse struct {
	ID         string  `json:"id"`
	MenuItemID *string `json:"menu_item_id"`
	Name       string  `json:"name"`
	PriceMinor int64   `json:"price_minor"`
	Currency   string  `json:"currency"`
	Quantity   int     `json:"quantity"`
	TotalMinor int64   `json:"total_minor"`
	Status     string  `json:"status"`
	Comment    *string `json:"comment"`
}

// bookingTableResponse exposes the occupied window, buffer included — that is
// the interval stored in booking_tables.slot, not the guest-facing visit.
type bookingTableResponse struct {
	ID        string    `json:"id"`
	TableID   string    `json:"table_id"`
	SlotStart time.Time `json:"slot_start"`
	SlotEnd   time.Time `json:"slot_end"`
	Active    bool      `json:"active"`
}

type messageResponse struct {
	ID         string     `json:"id"`
	BookingID  string     `json:"booking_id"`
	SenderType string     `json:"sender_type"`
	SenderID   *string    `json:"sender_id"`
	Message    string     `json:"message"`
	IsRead     bool       `json:"is_read"`
	ReadAt     *time.Time `json:"read_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type surveyResponse struct {
	ID             string    `json:"id"`
	BookingID      *string   `json:"booking_id"`
	RestaurantID   string    `json:"restaurant_id"`
	RatingOverall  int       `json:"rating_overall"`
	RatingFood     int       `json:"rating_food"`
	RatingService  int       `json:"rating_service"`
	RatingAmbience int       `json:"rating_ambience"`
	NPS            int       `json:"nps"`
	Comment        *string   `json:"comment"`
	Dismissed      bool      `json:"dismissed"`
	CreatedAt      time.Time `json:"created_at"`
}

type slotResponse struct {
	StartsAt   time.Time `json:"starts_at"`
	EndsAt     time.Time `json:"ends_at"`
	Available  bool      `json:"available"`
	FreeTables int       `json:"free_tables"`
	Reason     string    `json:"reason,omitempty"`
}

type availabilityResponse struct {
	RestaurantID    string         `json:"restaurant_id"`
	Date            string         `json:"date"`
	Timezone        string         `json:"timezone"`
	Guests          int            `json:"guests"`
	DurationMinutes int            `json:"duration_minutes"`
	Slots           []slotResponse `json:"slots"`
}

type blacklistResponse struct {
	ID              string    `json:"id"`
	RestaurantID    *string   `json:"restaurant_id"`
	UserID          *string   `json:"user_id"`
	PhoneNormalized *string   `json:"phone_normalized"`
	Email           *string   `json:"email"`
	Reason          *string   `json:"reason"`
	IsActive        bool      `json:"is_active"`
	CreatedAt       time.Time `json:"created_at"`
}

type statusChangeResponse struct {
	ID         string    `json:"id"`
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	ActorType  string    `json:"actor_type"`
	ActorID    *string   `json:"actor_id"`
	Reason     *string   `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
}

func bookingToResponse(b domain.Booking) bookingResponse {
	var cancelledBy *string
	if b.CancelledBy != nil {
		s := string(*b.CancelledBy)
		cancelledBy = &s
	}
	return bookingResponse{
		ID: b.ID.String(), RestaurantID: b.RestaurantID.String(), UserID: idPtr(b.UserID),
		Name: b.Name, Phone: b.Phone, Email: b.Email, Guests: b.Guests,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, Status: string(b.Status), Source: string(b.Source),
		Notes: b.Notes, PromotionID: idPtr(b.PromotionID), EventID: idPtr(b.EventID),
		CreatedByAdmin: b.CreatedByAdmin, ForcedPlacement: b.ForcedPlacement,
		ConfirmedAt: b.ConfirmedAt, ArrivedAt: b.ArrivedAt, CancelledAt: b.CancelledAt,
		CancelledBy: cancelledBy, CancellationReasonCode: b.CancellationReasonCode,
		CancellationReason: b.CancellationReason,
		CreatedAt:          b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

func detailsToResponse(d *uc.BookingDetails) bookingDetailsResponse {
	out := bookingDetailsResponse{
		bookingResponse: bookingToResponse(d.Booking),
		Items:           make([]bookingItemResponse, 0, len(d.Items)),
		Tables:          make([]bookingTableResponse, 0, len(d.Tables)),
	}
	for _, it := range d.Items {
		out.Items = append(out.Items, bookingItemResponse{
			ID: it.ID.String(), MenuItemID: idPtr(it.MenuItemID), Name: it.ItemName,
			PriceMinor: it.PriceMinor, Currency: it.Currency, Quantity: it.Quantity,
			TotalMinor: it.TotalMinor(), Status: string(it.Status), Comment: it.Comment,
		})
	}
	for _, t := range d.Tables {
		out.Tables = append(out.Tables, bookingTableResponse{
			ID: t.ID.String(), TableID: t.TableID.String(),
			SlotStart: t.SlotStart, SlotEnd: t.SlotEnd, Active: t.Active,
		})
	}
	return out
}

func messageToResponse(m domain.BookingMessage) messageResponse {
	return messageResponse{
		ID: m.ID.String(), BookingID: m.BookingID.String(), SenderType: string(m.SenderType),
		SenderID: idPtr(m.SenderID), Message: m.Message, IsRead: m.IsRead,
		ReadAt: m.ReadAt, CreatedAt: m.CreatedAt,
	}
}

func surveyToResponse(s *domain.RestaurantSurvey) surveyResponse {
	return surveyResponse{
		ID: s.ID.String(), BookingID: idPtr(s.BookingID), RestaurantID: s.RestaurantID.String(),
		RatingOverall: s.RatingOverall, RatingFood: s.RatingFood, RatingService: s.RatingService,
		RatingAmbience: s.RatingAmbience, NPS: s.NPS, Comment: s.Comment,
		Dismissed: s.Dismissed, CreatedAt: s.CreatedAt,
	}
}

func availabilityToResponse(d *uc.DayAvailability) availabilityResponse {
	out := availabilityResponse{
		RestaurantID: d.RestaurantID.String(), Date: d.Date, Timezone: d.Timezone,
		Guests: d.Guests, DurationMinutes: d.DurationMinutes,
		Slots: make([]slotResponse, 0, len(d.Slots)),
	}
	for _, s := range d.Slots {
		out.Slots = append(out.Slots, slotResponse{
			StartsAt: s.StartsAt, EndsAt: s.EndsAt, Available: s.Available,
			FreeTables: s.FreeTables, Reason: s.Reason,
		})
	}
	return out
}

func blacklistToResponse(e domain.BlacklistEntry) blacklistResponse {
	return blacklistResponse{
		ID: e.ID.String(), RestaurantID: idPtr(e.RestaurantID), UserID: idPtr(e.UserID),
		PhoneNormalized: e.PhoneNormalized, Email: e.Email, Reason: e.Reason,
		IsActive: e.IsActive, CreatedAt: e.CreatedAt,
	}
}

func statusChangeToResponse(c domain.BookingStatusChange) statusChangeResponse {
	var from *string
	if c.FromStatus != nil {
		s := string(*c.FromStatus)
		from = &s
	}
	return statusChangeResponse{
		ID: c.ID.String(), FromStatus: from, ToStatus: string(c.ToStatus),
		ActorType: string(c.ActorType), ActorID: idPtr(c.ActorID),
		Reason: c.Reason, CreatedAt: c.CreatedAt,
	}
}

// idPtr renders an optional uuid as an optional string.
func idPtr[T interface{ String() string }](v *T) *string {
	if v == nil {
		return nil
	}
	s := (*v).String()
	return &s
}

// bookingPolicyResponse carries both levels of the policy: `effective` is what
// the engine will actually apply, `overrides` is what this venue stores (a null
// field means "inherit the global default"). Clients need both to render an
// editor without guessing which values are inherited.
type bookingPolicyResponse struct {
	RestaurantID string                     `json:"restaurant_id"`
	Effective    effectiveBookingPolicy     `json:"effective"`
	Overrides    bookingPolicyOverrideBlock `json:"overrides"`
}

type effectiveBookingPolicy struct {
	Timezone              string `json:"timezone"`
	DurationMinutes       int    `json:"duration_minutes"`
	BufferMinutes         int    `json:"buffer_minutes"`
	LeadMinutes           int    `json:"lead_minutes"`
	HorizonDays           int    `json:"horizon_days"`
	CancelDeadlineMinutes int    `json:"cancel_deadline_minutes"`
	ConfirmSLAMinutes     int    `json:"confirm_sla_minutes"`
	MaxGuestsPerBooking   int    `json:"max_guests_per_booking"`
	AutoConfirm           bool   `json:"auto_confirm"`
}

type bookingPolicyOverrideBlock struct {
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

func policyToResponse(restaurantID string, v *uc.PolicyView) bookingPolicyResponse {
	e, o := v.Effective, v.Override
	return bookingPolicyResponse{
		RestaurantID: restaurantID,
		Effective: effectiveBookingPolicy{
			Timezone:              e.Timezone,
			DurationMinutes:       int(e.Duration / time.Minute),
			BufferMinutes:         int(e.Buffer / time.Minute),
			LeadMinutes:           int(e.Lead / time.Minute),
			HorizonDays:           e.HorizonDays,
			CancelDeadlineMinutes: int(e.CancelDeadline / time.Minute),
			ConfirmSLAMinutes:     int(e.ConfirmSLA / time.Minute),
			MaxGuestsPerBooking:   e.MaxGuestsPerBooking,
			AutoConfirm:           e.AutoConfirm,
		},
		Overrides: bookingPolicyOverrideBlock{
			Timezone:               o.Timezone,
			BookingDurationMinutes: o.BookingDurationMinutes,
			BookingBufferMinutes:   o.BookingBufferMinutes,
			BookingLeadMinutes:     o.BookingLeadMinutes,
			BookingHorizonDays:     o.BookingHorizonDays,
			CancelDeadlineMinutes:  o.CancelDeadlineMinutes,
			ConfirmSLAMinutes:      o.ConfirmSLAMinutes,
			MaxGuestsPerBooking:    o.MaxGuestsPerBooking,
			AutoConfirm:            o.AutoConfirm,
		},
	}
}

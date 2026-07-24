package admin

import (
	"time"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/admin"
)

// restaurantProfileResponse is the venue's own profile as shown in the admin
// panel. It intentionally surfaces the editorial/platform flags (is_active,
// is_premium, …) as READ-ONLY display fields — the panel can show them but the
// PUT handler never accepts them (see admin.ProfileInput).
type restaurantProfileResponse struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	NameI18n         map[string]string `json:"name_i18n,omitempty"`
	Description      string            `json:"description"`
	DescriptionI18n  map[string]string `json:"description_i18n,omitempty"`
	Address          string            `json:"address"`
	AddressI18n      map[string]string `json:"address_i18n,omitempty"`
	OpeningHours     string            `json:"opening_hours"`
	OpeningHoursI18n map[string]string `json:"opening_hours_i18n,omitempty"`
	Phone            string            `json:"phone"`
	Email            string            `json:"email"`
	City             string            `json:"city"`
	PriceCategory    string            `json:"price_category"`
	IsActive         bool              `json:"is_active"`
	IsPremium        *bool             `json:"is_premium"`
}

func profileToResponse(a *domain.RestaurantAggregate) restaurantProfileResponse {
	return restaurantProfileResponse{
		ID:               a.ID.String(),
		Name:             a.Name,
		NameI18n:         a.NameI18n,
		Description:      a.Description,
		DescriptionI18n:  a.DescriptionI18n,
		Address:          a.Address,
		AddressI18n:      a.AddressI18n,
		OpeningHours:     a.OpeningHours,
		OpeningHoursI18n: a.OpeningHoursI18n,
		Phone:            a.Phone,
		Email:            a.Email,
		City:             string(a.City),
		PriceCategory:    string(a.PriceCategory),
		IsActive:         a.IsActive,
		IsPremium:        a.IsPremium,
	}
}

type menuItemResponse struct {
	ID           string   `json:"id"`
	RestaurantID string   `json:"restaurant_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Price        string   `json:"price"`
	ImageURL     *string  `json:"image_url"`
	IsAvailable  bool     `json:"is_available"`
	Category     *string  `json:"category"`
	Subcategory  *string  `json:"subcategory"`
	PortionSize  *string  `json:"portion_size"`
	DisplayOrder *int     `json:"display_order"`
	Tags         []string `json:"tags"`
}

func menuItemToResponse(m *domain.MenuItem) menuItemResponse {
	tags := make([]string, 0, len(m.Tags))
	for _, t := range m.Tags {
		tags = append(tags, t.Tag)
	}
	return menuItemResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(), Name: m.Name,
		Description: m.Description, Price: m.Price, ImageURL: m.ImageURL,
		IsAvailable: m.IsAvailable, Category: m.Category, Subcategory: m.Subcategory,
		PortionSize: m.PortionSize, DisplayOrder: m.DisplayOrder, Tags: tags,
	}
}

type menuCategoryResponse struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	ParentID     *string `json:"parent_id"`
	DisplayOrder int     `json:"display_order"`
}

func categoryToResponse(c domain.MenuCategory) menuCategoryResponse {
	var parent *string
	if c.ParentID != nil {
		s := c.ParentID.String()
		parent = &s
	}
	return menuCategoryResponse{ID: c.ID.String(), Name: c.Name, ParentID: parent, DisplayOrder: c.DisplayOrder}
}

type workingHoursResponse struct {
	DayOfWeek int     `json:"day_of_week"`
	IsOpen    bool    `json:"is_open"`
	OpenTime  *string `json:"open_time"`
	CloseTime *string `json:"close_time"`
}

type scheduleOverrideResponse struct {
	Date      string  `json:"date"` // YYYY-MM-DD
	IsClosed  bool    `json:"is_closed"`
	OpenTime  *string `json:"open_time"`
	CloseTime *string `json:"close_time"`
	Note      *string `json:"note"`
}

func overrideToResponse(o domain.ScheduleOverride) scheduleOverrideResponse {
	return scheduleOverrideResponse{
		Date:      o.Date.Format(dateLayout),
		IsClosed:  o.IsClosed,
		OpenTime:  o.OpenTime,
		CloseTime: o.CloseTime,
		Note:      o.Note,
	}
}

type scheduleResponse struct {
	WorkingHours []workingHoursResponse     `json:"working_hours"`
	Overrides    []scheduleOverrideResponse `json:"overrides"`
}

func scheduleToResponse(s *uc.Schedule) scheduleResponse {
	wh := make([]workingHoursResponse, 0, len(s.WorkingHours))
	for _, h := range s.WorkingHours {
		wh = append(wh, workingHoursResponse{
			DayOfWeek: h.DayOfWeek, IsOpen: h.IsOpen, OpenTime: h.OpenTime, CloseTime: h.CloseTime,
		})
	}
	ov := make([]scheduleOverrideResponse, 0, len(s.Overrides))
	for _, o := range s.Overrides {
		ov = append(ov, overrideToResponse(o))
	}
	return scheduleResponse{WorkingHours: wh, Overrides: ov}
}

type bookingResponse struct {
	ID                 string     `json:"id"`
	RestaurantID       string     `json:"restaurant_id"`
	UserID             *string    `json:"user_id"`
	Name               string     `json:"name"`
	Phone              string     `json:"phone"`
	Email              string     `json:"email"`
	Guests             int        `json:"guests"`
	StartsAt           time.Time  `json:"starts_at"`
	EndsAt             time.Time  `json:"ends_at"`
	Status             string     `json:"status"`
	Source             string     `json:"source"`
	Notes              *string    `json:"notes"`
	CancelledBy        *string    `json:"cancelled_by"`
	CancellationReason *string    `json:"cancellation_reason"`
	ConfirmedAt        *time.Time `json:"confirmed_at"`
	CreatedAt          time.Time  `json:"created_at"`
}

func bookingToResponse(b domain.Booking) bookingResponse {
	var uid *string
	if b.UserID != nil {
		s := b.UserID.String()
		uid = &s
	}
	var cancelledBy *string
	if b.CancelledBy != nil {
		s := string(*b.CancelledBy)
		cancelledBy = &s
	}
	return bookingResponse{
		ID: b.ID.String(), RestaurantID: b.RestaurantID.String(), UserID: uid,
		Name: b.Name, Phone: b.Phone, Email: b.Email, Guests: b.Guests,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, Status: string(b.Status), Source: string(b.Source),
		Notes: b.Notes, CancelledBy: cancelledBy, CancellationReason: b.CancellationReason,
		ConfirmedAt: b.ConfirmedAt, CreatedAt: b.CreatedAt,
	}
}

type guestResponse struct {
	UserID          *string   `json:"user_id"`
	Name            string    `json:"name"`
	Phone           string    `json:"phone"`
	PhoneNormalized string    `json:"phone_normalized"`
	Email           string    `json:"email"`
	BookingsCount   int       `json:"bookings_count"`
	VisitsCount     int       `json:"visits_count"`
	FirstBookingAt  time.Time `json:"first_booking_at"`
	LastBookingAt   time.Time `json:"last_booking_at"`
}

func guestToResponse(g domain.RestaurantGuest) guestResponse {
	var uid *string
	if g.UserID != nil {
		s := g.UserID.String()
		uid = &s
	}
	return guestResponse{
		UserID: uid, Name: g.Name, Phone: g.Phone, PhoneNormalized: g.PhoneNormalized,
		Email: g.Email, BookingsCount: g.BookingsCount, VisitsCount: g.VisitsCount,
		FirstBookingAt: g.FirstBookingAt, LastBookingAt: g.LastBookingAt,
	}
}

// freeCancelWindowResponse echoes the stored window back to the caller.
type freeCancelWindowResponse struct {
	FreeCancelWindowMinutes int `json:"free_cancel_window_minutes"`
}

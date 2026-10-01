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
	Date                   string  `json:"date"` // YYYY-MM-DD
	IsClosed               bool    `json:"is_closed"`
	OpenTime               *string `json:"open_time"`
	CloseTime              *string `json:"close_time"`
	Note                   *string `json:"note"`
	BookingPaymentRequired bool    `json:"booking_payment_required"`
	DepositAmountMinor     *int64  `json:"deposit_amount_minor"`
}

func overrideToResponse(o domain.ScheduleOverride) scheduleOverrideResponse {
	return scheduleOverrideResponse{
		Date:                   o.Date.Format(dateLayout),
		IsClosed:               o.IsClosed,
		OpenTime:               o.OpenTime,
		CloseTime:              o.CloseTime,
		Note:                   o.Note,
		BookingPaymentRequired: o.BookingPaymentRequired,
		DepositAmountMinor:     o.DepositAmountMinor,
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
	ArrivedAt          *time.Time `json:"arrived_at"`
	CreatedAt          time.Time  `json:"created_at"`
	// Предзаказ гостя. Пустой массив, а не null: список блюд у брони без
	// предзаказа существует, он просто пуст, и клиенту не нужно разбирать
	// два разных «ничего».
	Preorder []preorderItemResponse `json:"preorder"`
}

// preorderItemResponse — одна строка предзаказа так, как её видит заведение:
// что готовить, сколько порций и почём. Цена уже зафиксирована в момент
// заказа, поэтому берётся из самой строки, а не из текущего меню.
type preorderItemResponse struct {
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	PriceMinor int64  `json:"price_minor"`
	TotalMinor int64  `json:"total_minor"`
	Comment    string `json:"comment,omitempty"`
}

func preorderToResponse(items []domain.BookingItem) []preorderItemResponse {
	out := make([]preorderItemResponse, 0, len(items))
	for _, i := range items {
		comment := ""
		if i.Comment != nil {
			comment = *i.Comment
		}
		out = append(out, preorderItemResponse{
			Name:       i.ItemName,
			Quantity:   i.Quantity,
			PriceMinor: i.PriceMinor,
			TotalMinor: i.PriceMinor * int64(i.Quantity),
			Comment:    comment,
		})
	}
	return out
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
		ConfirmedAt: b.ConfirmedAt, ArrivedAt: b.ArrivedAt, CreatedAt: b.CreatedAt,
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

// preorderSettingsResponse reports the venue's pre-order policy. Enabled is the
// RAW stored value: null = inherits enabled_global (never collapsed to false).
// MinAmountMinor is null when no floor is set.
type preorderSettingsResponse struct {
	Enabled                  *bool  `json:"enabled"`
	EnabledGlobal            bool   `json:"enabled_global"`
	PaymentsEnabledEffective bool   `json:"payments_enabled_effective"`
	MinAmountMinor           *int64 `json:"min_amount_minor"`
}

func preorderSettingsToResponse(v uc.PreorderSettingsView) preorderSettingsResponse {
	return preorderSettingsResponse{
		Enabled: v.Enabled, EnabledGlobal: v.EnabledGlobal,
		PaymentsEnabledEffective: v.PaymentsEnabledEffective, MinAmountMinor: v.MinAmountMinor,
	}
}

// whatsAppSettingsResponse reports the venue's WhatsApp alert configuration.
// `connected` is derived, not stored: a channel with no number is not connected
// however enabled it claims to be, and the panel should say so.
type whatsAppSettingsResponse struct {
	Connected     bool   `json:"connected"`
	WhatsAppPhone string `json:"whatsapp_phone"`
	Enabled       bool   `json:"enabled"`
}

// telegramSettingsResponse reports the venue's Telegram alert configuration.
type telegramSettingsResponse struct {
	Connected      bool   `json:"connected"`
	TelegramChatID string `json:"telegram_chat_id"`
	Enabled        bool   `json:"enabled"`
}

// acquirerAccountResponse is the panel's view of a venue↔acquirer mapping.
// connected=false is the ordinary state of a venue nobody has onboarded to
// that acquirer yet.
type acquirerAccountResponse struct {
	Provider   string `json:"provider"`
	Connected  bool   `json:"connected"`
	AccountRef string `json:"account_ref"`
	IsActive   bool   `json:"is_active"`
}

func acquirerAccountToResponse(a uc.AcquirerAccount) acquirerAccountResponse {
	return acquirerAccountResponse{
		Provider:   string(a.Provider),
		Connected:  a.AccountRef != "",
		AccountRef: a.AccountRef,
		IsActive:   a.IsActive,
	}
}

type paymentMethodsResponse struct {
	PaymentsEnabled *bool    `json:"payments_enabled"`
	Methods         []string `json:"methods"`
	// PaymentsEnabledGlobal (read-only) is the platform's PAYMENTS_ENABLED
	// value; when PaymentsEnabled is null the venue inherits this. Ignored on
	// PUT — see request.go's paymentMethodsRequest, which has no such field.
	PaymentsEnabledGlobal bool `json:"payments_enabled_global"`
	KaspiAccountBound     bool `json:"kaspi_account_bound"`
}

func paymentMethodsToResponse(v uc.PaymentMethodsSettings) paymentMethodsResponse {
	ms := make([]string, 0, len(v.Methods))
	for _, m := range v.Methods {
		ms = append(ms, string(m))
	}
	return paymentMethodsResponse{
		PaymentsEnabled:       v.PaymentsEnabled,
		Methods:               ms,
		PaymentsEnabledGlobal: v.PaymentsEnabledGlobal,
		KaspiAccountBound:     v.KaspiAccountBound,
	}
}

// kwaakaOrdersResponse is a venue's Kwaaka kitchen-order settings.
//
// global_orders_enabled is the MASTER switch (env KWAAKA_ORDERS_ENABLED): when
// false the platform sends NOTHING for any venue, whatever orders_enabled says.
// will_send is the verdict that already accounts for it; blockers names what
// stops it (global_switch_off, venue_not_linked, not_configured, venue_disabled,
// pool_empty, pool_stale). Read-only fields are ignored on PUT.
type kwaakaOrdersResponse struct {
	GlobalOrdersEnabled bool `json:"global_orders_enabled"`
	// DefaultLeadMinutes is what a null lead_minutes resolves to (KWAAKA_KITCHEN_LEAD).
	DefaultLeadMinutes int `json:"default_lead_minutes"`
	// KwaakaRestaurantID is the venue's CURRENT Kwaaka linkage (null = not
	// linked); PoolKwaakaRestaurantID is the one the stored pool was chosen for.
	KwaakaRestaurantID     *string `json:"kwaaka_restaurant_id"`
	PoolKwaakaRestaurantID *string `json:"pool_kwaaka_restaurant_id"`
	Configured             bool    `json:"configured"`
	OrdersEnabled          bool    `json:"orders_enabled"`
	LeadMinutes            *int    `json:"lead_minutes"`
	EnabledAt              *string `json:"enabled_at"`
	UpdatedAt              *string `json:"updated_at"`
	UpdatedBy              *string `json:"updated_by"`

	Pool      []kwaakaPoolTableResponse `json:"pool"`
	PoolStale bool                      `json:"pool_stale"`
	WillSend  bool                      `json:"will_send"`
	Blockers  []string                  `json:"blockers"`

	// PosChecked is true when a live GET /tables answered; pos_tables then lists
	// what may be put in the pool. Absent/false is not an error.
	PosChecked bool                `json:"pos_checked"`
	PosTables  []kwaakaPosTableDTO `json:"pos_tables"`
}

type kwaakaPoolTableResponse struct {
	KwaakaTableID string `json:"kwaaka_table_id"`
	Position      int    `json:"position"`
	Label         string `json:"label"`
	// InPos: false = the table is gone from the venue's POS (every send to it
	// would be rejected); null = the POS was not asked or did not answer.
	InPos *bool `json:"in_pos"`
}

type kwaakaPosTableDTO struct {
	ID              string `json:"id"`
	Number          int    `json:"number"`
	Name            string `json:"name"`
	SeatingCapacity int    `json:"seating_capacity"`
	SectionName     string `json:"section_name"`
}

func kwaakaOrdersToResponse(v uc.KwaakaOrdersView) kwaakaOrdersResponse {
	ts := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.UTC().Format(time.RFC3339)
		return &s
	}
	nonEmpty := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out := kwaakaOrdersResponse{
		GlobalOrdersEnabled: v.GlobalEnabled, DefaultLeadMinutes: v.DefaultLeadMinutes,
		KwaakaRestaurantID: nonEmpty(v.VenueKwaakaID), PoolKwaakaRestaurantID: nonEmpty(v.SettingsKwaakaID),
		Configured: v.Configured, OrdersEnabled: v.OrdersEnabled, LeadMinutes: v.LeadMinutes,
		EnabledAt: ts(v.EnabledAt), UpdatedAt: ts(v.UpdatedAt),
		Pool: make([]kwaakaPoolTableResponse, 0, len(v.Pool)), PoolStale: v.PoolStale,
		WillSend: v.WillSend, Blockers: v.Blockers,
		PosChecked: v.POSChecked, PosTables: make([]kwaakaPosTableDTO, 0, len(v.POSTables)),
	}
	if v.UpdatedBy != nil {
		s := v.UpdatedBy.String()
		out.UpdatedBy = &s
	}
	for _, t := range v.Pool {
		out.Pool = append(out.Pool, kwaakaPoolTableResponse{
			KwaakaTableID: t.KwaakaTableID, Position: t.Position, Label: t.Label, InPos: t.InPOS})
	}
	for _, t := range v.POSTables {
		out.PosTables = append(out.PosTables, kwaakaPosTableDTO{
			ID: t.ID, Number: t.Number, Name: t.Name, SeatingCapacity: t.SeatingCapacity, SectionName: t.SectionName})
	}
	return out
}

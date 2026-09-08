package admin

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	adminuc "backend-core/internal/usecase/admin"
	bookinguc "backend-core/internal/usecase/bookings"
	menuuc "backend-core/internal/usecase/menu"
)

// dateLayout is the calendar-date format used for schedule overrides and the
// bookings ?date= filter.
const dateLayout = "2006-01-02"

// profileRequest carries the venue-editable profile fields. Editorial/platform
// flags are deliberately not present — they cannot be set through this panel.
type profileRequest struct {
	Name *string `json:"name"`
	// The `*_i18n` objects are PARTIAL translation updates — see
	// domain.I18nPatch and the same fields on saveRestaurantRequest: a named
	// language is written, a null one removed, an unmentioned one kept, and a
	// `ru` key writes the plain field it belongs to.
	NameI18n         map[string]*string `json:"name_i18n"`
	Description      *string            `json:"description"`
	DescriptionI18n  map[string]*string `json:"description_i18n"`
	Address          *string            `json:"address"`
	AddressI18n      map[string]*string `json:"address_i18n"`
	Phone            *string            `json:"phone"`
	Email            *string            `json:"email"`
	OpeningHours     *string            `json:"opening_hours"`
	OpeningHoursI18n map[string]*string `json:"opening_hours_i18n"`
}

func (r profileRequest) toInput() adminuc.ProfileInput {
	return adminuc.ProfileInput{
		Name:             r.Name,
		NameI18n:         domain.I18nPatch(r.NameI18n),
		Description:      r.Description,
		DescriptionI18n:  domain.I18nPatch(r.DescriptionI18n),
		Address:          r.Address,
		AddressI18n:      domain.I18nPatch(r.AddressI18n),
		Phone:            r.Phone,
		Email:            r.Email,
		OpeningHours:     r.OpeningHours,
		OpeningHoursI18n: domain.I18nPatch(r.OpeningHoursI18n),
	}
}

// menuItemRequest mirrors the menu package's item payload — the admin panel
// reuses the same menu.ItemInput so validation stays in one place.
type menuItemRequest struct {
	Name            *string           `json:"name"`
	NameI18n        map[string]string `json:"name_i18n"`
	Description     *string           `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n"`
	Price           *string           `json:"price"`
	ImageURL        *string           `json:"image_url"`
	IsAvailable     *bool             `json:"is_available"`
	Category        *string           `json:"category"`
	CategoryI18n    map[string]string `json:"category_i18n"`
	Subcategory     *string           `json:"subcategory"`
	SubcategoryI18n map[string]string `json:"subcategory_i18n"`
	PortionSize     *string           `json:"portion_size"`
	PortionSizeI18n map[string]string `json:"portion_size_i18n"`
	// Language: null or "ru" only. A dish row is the base row; translations
	// live in the *_i18n maps. Any other value is a 422 with code
	// menu_item_language_not_base, because the guest listing serves base rows
	// and such a dish would be invisible to every guest while looking normal
	// here.
	Language     *string   `json:"language"`
	DisplayOrder *int      `json:"display_order"`
	Tags         *[]string `json:"tags"`
}

func (r menuItemRequest) toInput() menuuc.ItemInput {
	return menuuc.ItemInput{
		Name: r.Name, NameI18n: domain.I18n(r.NameI18n), Description: r.Description,
		DescriptionI18n: domain.I18n(r.DescriptionI18n), Price: r.Price, ImageURL: r.ImageURL,
		IsAvailable: r.IsAvailable, Category: r.Category, CategoryI18n: domain.I18n(r.CategoryI18n),
		Subcategory: r.Subcategory, SubcategoryI18n: domain.I18n(r.SubcategoryI18n),
		PortionSize: r.PortionSize, PortionSizeI18n: domain.I18n(r.PortionSizeI18n),
		Language: r.Language, DisplayOrder: r.DisplayOrder, Tags: r.Tags,
	}
}

type availabilityRequest struct {
	IsAvailable bool `json:"is_available"`
}

// stopListRequest is the fast "we ran out" bulk toggle.
type stopListRequest struct {
	ItemIDs   []string `json:"item_ids"`
	Available bool     `json:"available"`
}

func (r stopListRequest) itemIDs() ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(r.ItemIDs))
	for _, s := range r.ItemIDs {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid item id %q", domain.ErrValidation, s)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// workingHoursRequest replaces the venue's whole weekly schedule.
type workingHoursRequest struct {
	WorkingHours []workingHoursEntry `json:"working_hours"`
}

type workingHoursEntry struct {
	DayOfWeek int     `json:"day_of_week"`
	IsOpen    bool    `json:"is_open"`
	OpenTime  *string `json:"open_time"`
	CloseTime *string `json:"close_time"`
}

func (r workingHoursRequest) toDomain(restaurantID uuid.UUID) []domain.WorkingHours {
	out := make([]domain.WorkingHours, 0, len(r.WorkingHours))
	for _, e := range r.WorkingHours {
		out = append(out, domain.WorkingHours{
			RestaurantID: restaurantID, DayOfWeek: e.DayOfWeek, IsOpen: e.IsOpen,
			OpenTime: e.OpenTime, CloseTime: e.CloseTime,
		})
	}
	return out
}

// scheduleOverrideRequest upserts one special-day override.
type scheduleOverrideRequest struct {
	Date      string  `json:"date"` // YYYY-MM-DD
	IsClosed  bool    `json:"is_closed"`
	OpenTime  *string `json:"open_time"`
	CloseTime *string `json:"close_time"`
	Note      *string `json:"note"`
	// BookingPaymentRequired marks the day as PAID; DepositAmountMinor is the
	// required deposit in int64 minor units (validated in the usecase).
	BookingPaymentRequired bool   `json:"booking_payment_required"`
	DepositAmountMinor     *int64 `json:"deposit_amount_minor"`
}

func (r scheduleOverrideRequest) toInput() (adminuc.ScheduleOverrideInput, error) {
	d, err := time.Parse(dateLayout, strings.TrimSpace(r.Date))
	if err != nil {
		return adminuc.ScheduleOverrideInput{}, fmt.Errorf("%w: date must be YYYY-MM-DD", domain.ErrValidation)
	}
	return adminuc.ScheduleOverrideInput{
		Date: d, IsClosed: r.IsClosed, OpenTime: r.OpenTime, CloseTime: r.CloseTime, Note: r.Note,
		BookingPaymentRequired: r.BookingPaymentRequired, DepositAmountMinor: r.DepositAmountMinor,
	}, nil
}

// reasonRequest is the optional {"reason": "..."} body on booking transitions.
type reasonRequest struct {
	Reason *string `json:"reason"`
}

// cancelRequest is the optional body on a venue cancellation.
type cancelRequest struct {
	ReasonCode *string `json:"reason_code"`
	Reason     *string `json:"reason"`
}

func (r cancelRequest) toInput() bookinguc.CancelInput {
	return bookinguc.CancelInput{ReasonCode: r.ReasonCode, Reason: r.Reason}
}

// bookingFilter parses the venue calendar filters (?status=, ?from=, ?to=,
// ?date=, ?page=, ?per_page=). Mirrors the bookings package's parser; kept
// local because that one is unexported.
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
	if d := c.Query("date"); d != "" && f.From == nil && f.To == nil {
		day, err := domain.ParseCalendarDate(d)
		if err != nil {
			return f, err
		}
		// Unresolved on purpose — the venue's own zone decides which instants
		// this day covers, and that is resolved in the usecase. See the same
		// note in transport/rest/bookings.
		f.CalendarDate = &day
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))
	return f, nil
}

func parseTimeQuery(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	t, err := time.Parse(dateLayout, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// freeCancelWindowRequest sets the money-path free-cancellation window
// (minutes). A pointer so "0" (a valid, if aggressive, window) is told apart
// from an omitted field; the handler rejects a missing value.
type freeCancelWindowRequest struct {
	FreeCancelWindowMinutes *int `json:"free_cancel_window_minutes"`
}

// preorderSettingsRequest sets the venue's pre-order policy. min_amount_minor is
// a pointer so an omitted field CLEARS the floor (null) and an explicit 0 is
// distinguishable — both mean "no minimum" here, but the pointer keeps the DTO
// honest about intent. enabled defaults to false when omitted.
type preorderSettingsRequest struct {
	Enabled        bool   `json:"enabled"`
	MinAmountMinor *int64 `json:"min_amount_minor"`
}

// telegramChatRequest connects the venue's Telegram alert chat. The shape is
// validated in the usecase (numeric chat id or @username).
type telegramChatRequest struct {
	TelegramChatID string `json:"telegram_chat_id" binding:"required"`
}

// whatsAppPhoneRequest connects the venue's WhatsApp number for booking alerts.
// Deliberately free-form: staff paste the number the way they know it, and the
// usecase normalizes it to E.164 — refusing "8 701 …" here would be pedantry
// aimed at the person least able to fix it.
type whatsAppPhoneRequest struct {
	WhatsAppPhone string `json:"whatsapp_phone"`
}

// acquirerAccountRequest points a venue at one of an acquirer's accounts.
// account_ref is an ADDRESS (for Kaspi, the company id inside our Kaspi
// service), never a key — credentials are read from env and are not settable
// over HTTP.
type acquirerAccountRequest struct {
	Provider   string `json:"provider"`
	AccountRef string `json:"account_ref"`
	// IsActive suspends the mapping without losing it. Absent means false, so
	// the panel must send it explicitly when enabling a venue.
	IsActive bool `json:"is_active"`
}

func (r acquirerAccountRequest) toInput() adminuc.AcquirerAccount {
	return adminuc.AcquirerAccount{
		Provider:   domain.PaymentProvider(strings.TrimSpace(r.Provider)),
		AccountRef: r.AccountRef,
		IsActive:   r.IsActive,
	}
}

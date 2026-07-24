package admin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/restaurants"
)

// Actor is the authenticated staff caller. Role is the GLOBAL user role
// (RoleAdmin = superadmin bypass); the per-restaurant staff role that actually
// gates each action is resolved inside authorize via the permission checker.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

func (a Actor) bookingActor() bookings.Actor {
	return bookings.Actor{UserID: a.UserID, Role: a.Role}
}

// UseCase is the restaurant admin panel. Every method is scoped to one
// restaurantID and authorized against it before any data is touched.
type UseCase struct {
	perms        permissionChecker
	restaurants  restaurantStore
	menu         menuStore
	workingHours workingHoursStore
	overrides    domain.ScheduleOverrideRepository
	guests       guestStore
	bookingList  bookingLister
	bookingTx    bookingTransitioner
	paySettings  paymentSettingsWriter
	telegram     telegramSettings
}

// NewUseCase constructs the admin-panel usecase.
func NewUseCase(
	perms permissionChecker,
	rest restaurantStore,
	menu menuStore,
	workingHours workingHoursStore,
	overrides domain.ScheduleOverrideRepository,
	guests guestStore,
	bookingList bookingLister,
	bookingTx bookingTransitioner,
	paySettings paymentSettingsWriter,
	telegram telegramSettings,
) *UseCase {
	return &UseCase{
		perms: perms, restaurants: rest, menu: menu, workingHours: workingHours,
		overrides: overrides, guests: guests, bookingList: bookingList, bookingTx: bookingTx,
		paySettings: paySettings, telegram: telegram,
	}
}

// Bounds for the free-cancellation window (minutes), enforced here rather than
// as a DB CHECK beyond ">= 0": a week is already an unusually long window, and
// a negative value is meaningless. Mirrors usecase/bookings' policy bounds.
const (
	minFreeCancelWindowMinutes = 0
	maxFreeCancelWindowMinutes = 7 * 24 * 60
)

// SetFreeCancelWindow updates the venue's money-path free-cancellation window
// (restaurants.free_cancel_window_minutes): a deposit hold is released to the
// guest only when the booking is cancelled earlier than this before starts_at;
// a later cancellation or a no-show forfeits it to the venue. owner/manager
// (PermRestaurantManage).
func (u *UseCase) SetFreeCancelWindow(ctx context.Context, actor Actor, restaurantID uuid.UUID, minutes int) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	if minutes < minFreeCancelWindowMinutes || minutes > maxFreeCancelWindowMinutes {
		return fmt.Errorf("%w: free_cancel_window_minutes must be between %d and %d",
			domain.ErrValidation, minFreeCancelWindowMinutes, maxFreeCancelWindowMinutes)
	}
	return u.paySettings.UpdateFreeCancelWindow(ctx, restaurantID, minutes)
}

// telegramChatIDPattern validates the shape of a Telegram chat id staff paste
// in. Two accepted forms:
//   - a numeric chat id: an optional leading '-' (groups/supergroups/channels
//     are negative) followed by up to 20 digits (a private user chat is a small
//     positive id, a supergroup is like -1001234567890);
//   - an '@username' for a public channel/group (5-32 chars after '@',
//     letters/digits/underscore, must start with a letter).
//
// This is shape validation only — that the bot can actually reach the chat is
// proven at send time (a wrong chat surfaces as a 400/403 the notifier logs).
var telegramChatIDPattern = regexp.MustCompile(`^(-?\d{1,20}|@[A-Za-z][A-Za-z0-9_]{4,31})$`)

// SetTelegramChatID connects (or re-connects) the venue's Telegram alert chat:
// the notifications bot will post "Новая бронь" there on each new booking. For
// increment 1 staff paste the chat id directly (a "connect your Telegram"
// deep-link / @start flow is future work). owner/manager (PermRestaurantManage).
func (u *UseCase) SetTelegramChatID(ctx context.Context, actor Actor, restaurantID uuid.UUID, chatID string) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	chatID = strings.TrimSpace(chatID)
	if !telegramChatIDPattern.MatchString(chatID) {
		return fmt.Errorf("%w: telegram_chat_id must be a numeric chat id (e.g. -1001234567890) or an @username", domain.ErrValidation)
	}
	return u.telegram.SetTelegramChatID(ctx, restaurantID, chatID)
}

// ClearTelegramChatID disconnects the venue's Telegram alert chat, silencing the
// channel. Idempotent. owner/manager (PermRestaurantManage).
func (u *UseCase) ClearTelegramChatID(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	return u.telegram.ClearTelegramChatID(ctx, restaurantID)
}

// GetTelegramSettings returns the venue's current Telegram target + toggle, so
// the admin panel can show whether a chat is connected. owner/manager
// (PermRestaurantManage).
func (u *UseCase) GetTelegramSettings(ctx context.Context, actor Actor, restaurantID uuid.UUID) (domain.TelegramSettings, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return domain.TelegramSettings{}, err
	}
	return u.telegram.TelegramSettings(ctx, restaurantID)
}

// authorize is the single RBAC gate for every admin-panel action. A superadmin
// (global RoleAdmin) always passes; anyone else must hold perm AT restaurantID,
// resolved by (actor, restaurantID) — which is exactly what makes cross-tenant
// access impossible: a caller with no staff row at restaurantID holds no
// permission there, so an id from the request path can never be "trusted"
// on its own, it is only ever the key the permission is checked against.
func (u *UseCase) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID, perm domain.Permission) error {
	if actor.UserID == uuid.Nil {
		return fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := u.perms.HasPermission(ctx, actor.UserID, restaurantID, perm)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: insufficient permission for this restaurant", domain.ErrForbidden)
	}
	return nil
}

// ---- Restaurant profile ----------------------------------------------------

// ProfileInput carries ONLY the venue-editable profile fields. Editorial /
// platform-controlled columns (is_active, is_premium, is_new, is_popular,
// display_order, category_id, city, price_category, coordinates) are
// deliberately absent: a restaurant's own staff must not be able to mark their
// venue premium/popular or reactivate a deactivated venue through the panel.
// Those stay superadmin-only via the existing restaurants admin routes.
type ProfileInput struct {
	Name         *string
	NameI18n     domain.I18n
	Description  *string
	Address      *string
	Phone        *string
	Email        *string
	OpeningHours *string // free-text / JSON working-hours summary shown on the storefront
}

// GetProfile returns the venue's own profile aggregate. owner/manager.
func (u *UseCase) GetProfile(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*domain.RestaurantAggregate, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.restaurants.Get(ctx, restaurantID)
}

// UpdateProfile patches the venue's own profile. Only the fields in ProfileInput
// are mapped onto the restaurants.SaveInput; every other column is left nil so
// the facade's read-modify-write preserves it. owner/manager.
func (u *UseCase) UpdateProfile(ctx context.Context, actor Actor, restaurantID uuid.UUID, in ProfileInput) (*domain.RestaurantAggregate, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	save := restaurants.SaveInput{
		Name:         in.Name,
		NameI18n:     in.NameI18n,
		Description:  in.Description,
		Address:      in.Address,
		Phone:        in.Phone,
		Email:        in.Email,
		OpeningHours: in.OpeningHours,
	}
	return u.restaurants.Update(ctx, restaurantID, save)
}

// ---- Menu ------------------------------------------------------------------

// ListMenu returns the venue's menu items. owner/manager.
func (u *UseCase) ListMenu(ctx context.Context, actor Actor, restaurantID uuid.UUID, lang *string) ([]domain.MenuItem, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.menu.ListByRestaurant(ctx, restaurantID, lang)
}

// ListCategories returns the GLOBAL menu-category reference tree (read-only for
// the panel: categories are shared editorial data across venues, so a single
// venue never mutates them — items carry their own free-text category instead).
// owner/manager.
func (u *UseCase) ListCategories(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.MenuCategory, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.menu.Categories(ctx)
}

// CreateMenuItem adds an item to the venue's menu. owner/manager.
func (u *UseCase) CreateMenuItem(ctx context.Context, actor Actor, restaurantID uuid.UUID, in menu.ItemInput) (*domain.MenuItem, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.menu.Create(ctx, restaurantID, in)
}

// UpdateMenuItem edits an item. Ownership (item belongs to restaurantID) is
// enforced by menu.Facade. owner/manager.
func (u *UseCase) UpdateMenuItem(ctx context.Context, actor Actor, restaurantID, itemID uuid.UUID, in menu.ItemInput) (*domain.MenuItem, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.menu.Update(ctx, restaurantID, itemID, in)
}

// DeleteMenuItem removes an item. owner/manager.
func (u *UseCase) DeleteMenuItem(ctx context.Context, actor Actor, restaurantID, itemID uuid.UUID) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	return u.menu.Delete(ctx, restaurantID, itemID)
}

// SetMenuItemAvailability toggles one item's availability. owner/manager (the
// full menu-edit surface). The all-staff fast path is SetStopList below.
func (u *UseCase) SetMenuItemAvailability(ctx context.Context, actor Actor, restaurantID, itemID uuid.UUID, available bool) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	return u.menu.SetAvailable(ctx, restaurantID, itemID, available)
}

// SetStopList bulk-flips availability for a set of items — the fast "we ran out"
// action. owner/manager/hostess (PermMenuStopList). Tenant-scoped in SQL: item
// ids of another venue are silently ignored. Returns the count actually changed.
func (u *UseCase) SetStopList(ctx context.Context, actor Actor, restaurantID uuid.UUID, itemIDs []uuid.UUID, available bool) (int, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermMenuStopList); err != nil {
		return 0, err
	}
	if len(itemIDs) == 0 {
		return 0, fmt.Errorf("%w: item_ids required", domain.ErrValidation)
	}
	return u.menu.SetAvailableBulk(ctx, restaurantID, itemIDs, available)
}

// ---- Schedule --------------------------------------------------------------

// Schedule is the venue's full working-time configuration: its regular weekly
// hours plus the special-day overrides on top.
type Schedule struct {
	WorkingHours []domain.WorkingHours
	Overrides    []domain.ScheduleOverride
}

// GetSchedule returns regular hours + overrides. owner/manager.
func (u *UseCase) GetSchedule(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*Schedule, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	wh, err := u.workingHours.ListWorkingHours(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	ov, err := u.overrides.ListByRestaurant(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	return &Schedule{WorkingHours: wh, Overrides: ov}, nil
}

var timeOfDayRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// SetWorkingHours replaces the venue's regular weekly hours wholesale.
// owner/manager. Each row's day_of_week must be 0..6 and, when open, both
// times must be valid HH:MM.
func (u *UseCase) SetWorkingHours(ctx context.Context, actor Actor, restaurantID uuid.UUID, hours []domain.WorkingHours) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	seen := make(map[int]bool, len(hours))
	normalized := make([]domain.WorkingHours, 0, len(hours))
	for _, h := range hours {
		if h.DayOfWeek < 0 || h.DayOfWeek > 6 {
			return fmt.Errorf("%w: day_of_week must be 0..6", domain.ErrValidation)
		}
		if seen[h.DayOfWeek] {
			return fmt.Errorf("%w: duplicate day_of_week %d", domain.ErrValidation, h.DayOfWeek)
		}
		seen[h.DayOfWeek] = true
		if h.IsOpen {
			if h.OpenTime == nil || h.CloseTime == nil ||
				!timeOfDayRe.MatchString(*h.OpenTime) || !timeOfDayRe.MatchString(*h.CloseTime) {
				return fmt.Errorf("%w: open day needs valid open_time/close_time (HH:MM)", domain.ErrValidation)
			}
		} else {
			h.OpenTime, h.CloseTime = nil, nil
		}
		h.RestaurantID = restaurantID
		normalized = append(normalized, h)
	}
	return u.workingHours.ReplaceWorkingHours(ctx, restaurantID, normalized)
}

// ScheduleOverrideInput sets (or clears) a special-day override.
type ScheduleOverrideInput struct {
	Date      time.Time
	IsClosed  bool
	OpenTime  *string
	CloseTime *string
	Note      *string
	// BookingPaymentRequired marks this special day as PAID (a deposit is
	// required to book that date). When true, DepositAmountMinor must be set and
	// positive. When false the day is FREE (the restaurant's ordinary payment
	// settings apply) and DepositAmountMinor is ignored/cleared.
	BookingPaymentRequired bool
	// DepositAmountMinor is the required deposit in int64 MINOR units when the
	// day is paid. Nil on a free day.
	DepositAmountMinor *int64
}

// SetScheduleOverride upserts one special-day override. owner/manager. When not
// closed, both times are required and validated; when closed, times are cleared
// (matching the table's CHECK constraint, validated here first to return 422).
func (u *UseCase) SetScheduleOverride(ctx context.Context, actor Actor, restaurantID uuid.UUID, in ScheduleOverrideInput) (*domain.ScheduleOverride, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	if in.Date.IsZero() {
		return nil, fmt.Errorf("%w: date required", domain.ErrValidation)
	}
	o := &domain.ScheduleOverride{
		RestaurantID: restaurantID,
		Date:         in.Date,
		IsClosed:     in.IsClosed,
		Note:         in.Note,
	}
	if in.IsClosed {
		o.OpenTime, o.CloseTime = nil, nil
	} else {
		if in.OpenTime == nil || in.CloseTime == nil ||
			!timeOfDayRe.MatchString(*in.OpenTime) || !timeOfDayRe.MatchString(*in.CloseTime) {
			return nil, fmt.Errorf("%w: an open override needs valid open_time/close_time (HH:MM)", domain.ErrValidation)
		}
		o.OpenTime, o.CloseTime = in.OpenTime, in.CloseTime
	}
	// Paid special day: a deposit is required to book that date. Only meaningful
	// on an OPEN day (a closed venue takes no bookings) and needs a positive
	// amount — validated here so the caller gets a 422, not a 500 from the DB
	// CHECK (migration 0036). On a free day the amount is cleared so it never
	// lingers from a previous paid state.
	if in.BookingPaymentRequired {
		if in.IsClosed {
			return nil, fmt.Errorf("%w: a closed day cannot require booking payment", domain.ErrValidation)
		}
		if in.DepositAmountMinor == nil || *in.DepositAmountMinor <= 0 {
			return nil, fmt.Errorf("%w: a paid special day needs a positive deposit_amount_minor", domain.ErrValidation)
		}
		o.BookingPaymentRequired = true
		o.DepositAmountMinor = in.DepositAmountMinor
	} else {
		o.BookingPaymentRequired = false
		o.DepositAmountMinor = nil
	}
	if err := u.overrides.Upsert(ctx, o); err != nil {
		return nil, err
	}
	return o, nil
}

// DeleteScheduleOverride removes the override for a day, reverting to the weekly
// schedule. owner/manager.
func (u *UseCase) DeleteScheduleOverride(ctx context.Context, actor Actor, restaurantID uuid.UUID, date time.Time) error {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return err
	}
	if date.IsZero() {
		return fmt.Errorf("%w: date required", domain.ErrValidation)
	}
	return u.overrides.Delete(ctx, restaurantID, date)
}

// ---- Bookings --------------------------------------------------------------

// ListBookings is the venue booking calendar. Any staff role (booking.manage).
// Delegates to bookings.Facade, which pins the restaurant filter and re-checks
// staff access.
func (u *UseCase) ListBookings(ctx context.Context, actor Actor, restaurantID uuid.UUID, f domain.BookingFilter) ([]domain.Booking, int, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermBookingManage); err != nil {
		return nil, 0, err
	}
	return u.bookingList.ListByRestaurant(ctx, actor.bookingActor(), restaurantID, f)
}

// ConfirmBooking accepts/confirms a pending booking. Any staff role.
func (u *UseCase) ConfirmBooking(ctx context.Context, actor Actor, restaurantID, bookingID uuid.UUID, reason *string) (*domain.Booking, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermBookingManage); err != nil {
		return nil, err
	}
	return u.bookingTx.Confirm(ctx, actor.bookingActor(), bookingID, reason)
}

// RejectBooking declines a booking (venue refusal → cancelled, attributed to the
// restaurant). Any staff role.
func (u *UseCase) RejectBooking(ctx context.Context, actor Actor, restaurantID, bookingID uuid.UUID, reason *string) (*domain.Booking, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermBookingManage); err != nil {
		return nil, err
	}
	return u.bookingTx.Reject(ctx, actor.bookingActor(), bookingID, reason)
}

// CancelBooking cancels a booking as the venue. Any staff role.
func (u *UseCase) CancelBooking(ctx context.Context, actor Actor, restaurantID, bookingID uuid.UUID, in bookings.CancelInput) (*domain.Booking, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermBookingManage); err != nil {
		return nil, err
	}
	return u.bookingTx.Cancel(ctx, actor.bookingActor(), bookingID, in)
}

// NoShowBooking marks a confirmed booking as a no-show. Any staff role.
func (u *UseCase) NoShowBooking(ctx context.Context, actor Actor, restaurantID, bookingID uuid.UUID, reason *string) (*domain.Booking, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermBookingManage); err != nil {
		return nil, err
	}
	return u.bookingTx.NoShow(ctx, actor.bookingActor(), bookingID, reason)
}

// ---- Guests ----------------------------------------------------------------

// ListGuests returns the venue's aggregated guest list (read-only, from
// bookings). owner/manager.
func (u *UseCase) ListGuests(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.RestaurantGuest, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.guests.ListByRestaurant(ctx, restaurantID)
}

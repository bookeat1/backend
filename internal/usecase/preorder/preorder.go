// Package preorder is the pre-order usecase: a guest (or venue staff) attaches
// menu items to a booking, to be prepared and PAID FOR IN ADVANCE so the
// kitchen can start before the guest arrives.
//
// It deliberately reuses the EXISTING booking_items model (domain.BookingItem /
// domain.BookingItemRepository) — a pre-order line IS a booking item. There is
// no parallel table. The pre-order TOTAL is what the payment flow charges as
// domain.PurposePreorder (usecase/payments.resolveAmount already sums the
// booking's non-cancelled items when the venue requires pre-payment); this
// package only owns building those lines correctly and reading them back.
//
// The one invariant this package exists to protect: the price of every line is
// computed SERVER-SIDE from the menu item's CURRENT price, never taken from the
// client. A guest cannot pre-order a 5000-tenge dish for 1 tenge by sending a
// crafted amount — the request carries only (menu_item_id, quantity, comment),
// and the price is looked up here and snapshotted onto the line (frozen at
// attach time, exactly like a booking-time order).
package preorder

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Limits on a single pre-order request, guarding against an abusive or buggy
// client. A real menu order is small; these are generous ceilings, not a UX
// constraint.
const (
	maxLines      = 100
	maxQtyPerLine = 100
)

// Actor is the authenticated caller (from middleware.GetAuthUser). Role is the
// GLOBAL user role; the per-restaurant staff relation is resolved via the
// manager checker. Same shape as bookings.Actor / payments.Actor.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// Line is one requested pre-order position. The client sends ONLY the menu item
// and quantity (plus an optional comment) — never a price. The price is resolved
// server-side from the menu item, see the package doc.
type Line struct {
	MenuItemID uuid.UUID
	Quantity   int
	Comment    *string
}

// Preorder is a booking's pre-order: its line items plus the server-computed
// total. TotalMinor is the sum the payment flow charges as PurposePreorder.
type Preorder struct {
	BookingID  uuid.UUID
	Items      []domain.BookingItem
	TotalMinor int64
	Currency   string
}

// bookingReader loads a booking to resolve its restaurant, owner and status.
type bookingReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Booking, error)
}

// menuReader looks up a menu item's current price / availability / restaurant.
// Satisfied structurally by domain.MenuItemRepository (bound in bootstrap).
type menuReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.MenuItem, error)
}

// settingsReader reads the venue's payment-settings override — here only for the
// optional pre-order minimum (restaurants.preorder_min_amount_minor). Satisfied
// by *restaurant.Repository (the same GetPaymentOverride usecase/payments uses).
type settingsReader interface {
	GetPaymentOverride(ctx context.Context, restaurantID uuid.UUID) (domain.PaymentSettingsOverride, error)
}

// managerChecker answers "does this user manage this restaurant" (staff access).
type managerChecker interface {
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
}

// paymentReader reports whether a booking already has a LIVE (authorized or
// captured) payment. Once a pre-order has been paid, its lines are frozen:
// changing them would desync the captured amount from the food actually ordered.
type paymentReader interface {
	GetLiveByBookingID(ctx context.Context, bookingID uuid.UUID) (*domain.Payment, error)
}

// UseCase attaches/reads a booking's pre-order.
type UseCase struct {
	bookings bookingReader
	menu     menuReader
	items    domain.BookingItemRepository
	settings settingsReader
	managers managerChecker
	payments paymentReader
	tx       domain.TxManager
}

// NewUseCase constructs the pre-order usecase.
func NewUseCase(
	bookings bookingReader,
	menu menuReader,
	items domain.BookingItemRepository,
	settings settingsReader,
	managers managerChecker,
	payments paymentReader,
	tx domain.TxManager,
) *UseCase {
	return &UseCase{
		bookings: bookings, menu: menu, items: items, settings: settings,
		managers: managers, payments: payments, tx: tx,
	}
}

// Get returns a booking's current pre-order (lines + server-recomputed total).
// The caller must be the booking's owner, staff of its venue, or an admin.
func (u *UseCase) Get(ctx context.Context, actor Actor, bookingID uuid.UUID) (*Preorder, error) {
	b, err := u.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := u.authorize(ctx, actor, b); err != nil {
		return nil, err
	}
	items, err := u.items.ListByBooking(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	return buildPreorder(bookingID, items), nil
}

// Replace sets the booking's pre-order to exactly the given lines (a full
// replace, not an append): the whole set is validated, priced server-side and
// written in one transaction, so the guest's pre-order always matches the last
// successful request. Passing an empty slice CLEARS the pre-order.
//
// Every line is validated: the referenced menu item must exist, belong to THIS
// booking's restaurant (a cross-tenant item id is rejected, not silently
// dropped), and be available; the quantity must be positive and bounded. The
// price is taken from the menu item, never from the client. When the venue set a
// minimum pre-order (restaurants.preorder_min_amount_minor) and the total is
// non-zero, it must reach that floor.
func (u *UseCase) Replace(ctx context.Context, actor Actor, bookingID uuid.UUID, lines []Line) (*Preorder, error) {
	b, err := u.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := u.authorize(ctx, actor, b); err != nil {
		return nil, err
	}

	// A pre-order only makes sense on a booking that can still be prepared for.
	switch b.Status {
	case domain.BookingPending, domain.BookingConfirmed, domain.BookingWaitlist:
	default:
		return nil, fmt.Errorf("%w: booking is %s, its pre-order can no longer be changed", domain.ErrValidation, b.Status)
	}

	// Frozen after payment: a live (authorized/captured) payment already reflects
	// the current lines; changing them would move the charged amount away from
	// what was ordered. Not payable yet → free to edit.
	if live, err := u.payments.GetLiveByBookingID(ctx, bookingID); err == nil && live != nil {
		return nil, fmt.Errorf("%w: this booking's pre-order is already paid and can no longer be changed", domain.ErrValidation)
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	if len(lines) > maxLines {
		return nil, fmt.Errorf("%w: too many pre-order lines (max %d)", domain.ErrValidation, maxLines)
	}

	built := make([]domain.BookingItem, 0, len(lines))
	var total int64
	for _, ln := range lines {
		if ln.Quantity <= 0 || ln.Quantity > maxQtyPerLine {
			return nil, fmt.Errorf("%w: quantity must be between 1 and %d", domain.ErrValidation, maxQtyPerLine)
		}
		mi, err := u.menu.GetByID(ctx, ln.MenuItemID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, fmt.Errorf("%w: menu item %s does not exist", domain.ErrValidation, ln.MenuItemID)
			}
			return nil, err
		}
		if mi.RestaurantID != b.RestaurantID {
			return nil, fmt.Errorf("%w: menu item %s does not belong to this booking's restaurant", domain.ErrValidation, ln.MenuItemID)
		}
		if !mi.IsAvailable {
			return nil, fmt.Errorf("%w: menu item %q is not available", domain.ErrValidation, mi.Name)
		}
		priceMinor, err := domain.PriceStringToMinor(mi.Price)
		if err != nil {
			return nil, err
		}
		miID := mi.ID
		line := domain.BookingItem{
			BookingID:  bookingID,
			MenuItemID: &miID,
			ItemName:   mi.Name,
			PriceMinor: priceMinor,
			Currency:   string(domain.CurrencyKZT),
			Quantity:   ln.Quantity,
			Status:     domain.BookingItemPending,
			Comment:    ln.Comment,
		}
		total += line.TotalMinor()
		built = append(built, line)
	}

	// Optional per-venue minimum pre-order, enforced only on a non-empty order.
	if total > 0 {
		override, err := u.settings.GetPaymentOverride(ctx, b.RestaurantID)
		if err != nil {
			return nil, err
		}
		if min := override.PreorderMinAmountMinor; min != nil && total < *min {
			return nil, fmt.Errorf("%w: pre-order total %d is below this restaurant's minimum of %d", domain.ErrValidation, total, *min)
		}
	}

	if err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		return u.items.ReplaceForBooking(ctx, bookingID, built)
	}); err != nil {
		return nil, err
	}

	// Re-read so the response carries the canonical persisted rows (ids,
	// timestamps, defaulted fields) rather than the in-memory pre-insert set.
	saved, err := u.items.ListByBooking(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	return buildPreorder(bookingID, saved), nil
}

// authorize resolves the caller's relation to the booking: an admin always
// passes; venue staff pass for their OWN restaurant; the booking's owner passes.
// Everyone else gets ErrNotFound (a plain guest asking about someone else's
// booking must not learn it exists — same enumeration-oracle guard as
// usecase/bookings.authorize), except a restaurant-role caller managing another
// venue, who gets the clearer ErrForbidden.
func (u *UseCase) authorize(ctx context.Context, actor Actor, b *domain.Booking) error {
	if actor.UserID == uuid.Nil {
		return fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	owner := b.UserID != nil && *b.UserID == actor.UserID
	if owner {
		return nil
	}
	if actor.Role == domain.RoleRestaurant {
		ok, err := u.managers.Manages(ctx, actor.UserID, b.RestaurantID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return fmt.Errorf("%w: booking belongs to another restaurant", domain.ErrForbidden)
	}
	return fmt.Errorf("%w: booking", domain.ErrNotFound)
}

// buildPreorder assembles the result, summing the (non-cancelled) lines exactly
// as usecase/payments.resolveAmount does so the total shown here equals the
// amount that will be charged. Currency is taken from the lines (all KZT today);
// an empty pre-order reports KZT so the client always has a currency.
func buildPreorder(bookingID uuid.UUID, items []domain.BookingItem) *Preorder {
	out := &Preorder{BookingID: bookingID, Items: items, Currency: string(domain.CurrencyKZT)}
	for _, it := range items {
		if it.Status == domain.BookingItemCancelled {
			continue
		}
		out.TotalMinor += it.TotalMinor()
		if it.Currency != "" {
			out.Currency = it.Currency
		}
	}
	return out
}

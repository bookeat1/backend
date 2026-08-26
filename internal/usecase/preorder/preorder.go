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

// relation is the caller's resolved relation to a booking, as decided by
// resolveRelation. It exists because authorization ("may you touch this
// booking at all") and the pre-order lock ("may you still CHANGE it now that
// it is confirmed") are two different questions with two different answers for
// the same person: the guest who owns the booking passes the first and fails
// the second once the booking is confirmed.
type relation int

const (
	// relationGuest — the booking's own owner, acting as a guest from the app.
	relationGuest relation = iota
	// relationStaff — staff of the venue this booking belongs to.
	relationStaff
	// relationAdmin — a platform admin.
	relationAdmin
)

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

// paymentReader reports whether a booking has a payment IN FLIGHT — any
// non-terminal payment, including one still in the `created` state (the amount
// is snapshotted at POST /payments and captured later by the webhook, without
// re-reading the items). Once such a payment exists the pre-order is frozen:
// changing the lines would let the webhook capture an amount that no longer
// matches the ordered food. A terminal payment (failed/expired/voided/refunded)
// does not freeze it — the guest may re-order after a failed attempt.
type paymentReader interface {
	HasInFlightForBooking(ctx context.Context, bookingID uuid.UUID) (bool, error)
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
	if _, err := u.resolveRelation(ctx, actor, b); err != nil {
		return nil, err
	}
	items, err := u.items.ListByBooking(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	return buildPreorder(bookingID, items)
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
//
// Who may still change it:
//   - pending / waitlist — the guest, the venue's staff and admins;
//   - confirmed — the venue's staff and admins ONLY. The guest is refused with
//     domain.CodePreorderLocked (see the block in the body for why the venue
//     keeps the ability);
//   - anything else (arrived/completed/cancelled/no_show) — nobody.
func (u *UseCase) Replace(ctx context.Context, actor Actor, bookingID uuid.UUID, lines []Line) (*Preorder, error) {
	b, err := u.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	rel, err := u.resolveRelation(ctx, actor, b)
	if err != nil {
		return nil, err
	}

	// A pre-order only makes sense on a booking that can still be prepared for.
	// This gate applies to EVERYONE, including staff and admins: a booking that
	// is over (arrived/completed/cancelled/no_show) has nothing left to cook.
	switch b.Status {
	case domain.BookingPending, domain.BookingConfirmed, domain.BookingWaitlist:
	default:
		return nil, domain.WithCode(domain.CodePreorderBookingClosed,
			fmt.Errorf("%w: booking is %s, its pre-order can no longer be changed", domain.ErrValidation, b.Status))
	}

	// Once the booking is CONFIRMED the pre-order is closed TO THE GUEST: the
	// venue has accepted the order and starts planning the kitchen against it,
	// so a guest silently swapping the dishes afterwards is a change nobody at
	// the venue agreed to. Pending and waitlist stay editable — nothing has been
	// accepted yet in either.
	//
	// Venue staff (and admins) deliberately keep the ability after
	// confirmation: the venue is the party that has to cook the order and the
	// one that takes the phone call when a dish runs out or the guest changes
	// their mind. Taking it away would leave a legitimate change with no path at
	// all except cancelling the booking. The asymmetry is the point — the change
	// now goes through the party that agreed to it.
	if rel == relationGuest && b.Status == domain.BookingConfirmed {
		return nil, domain.WithCode(domain.CodePreorderLocked,
			fmt.Errorf("%w: booking is confirmed, its pre-order can only be changed by the restaurant", domain.ErrValidation))
	}

	// Frozen while a payment is in flight: any non-terminal payment (including a
	// `created` one whose amount is already snapshotted and will be captured by
	// the webhook) already reflects the current lines; changing them would move
	// the charged amount away from what was ordered. Only a terminal payment
	// (failed/expired/voided/refunded) leaves the pre-order free to edit again.
	inFlight, err := u.payments.HasInFlightForBooking(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if inFlight {
		return nil, domain.WithCode(domain.CodePreorderPaymentInFlight,
			fmt.Errorf("%w: this booking has a payment in progress; its pre-order can no longer be changed", domain.ErrValidation))
	}

	if len(lines) > maxLines {
		return nil, fmt.Errorf("%w: too many pre-order lines (max %d)", domain.ErrValidation, maxLines)
	}

	built := make([]domain.BookingItem, 0, len(lines))
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
		built = append(built, line)
	}

	// The total is the ONE shared definition (domain.SumPreorderItems), the same
	// helper the payment charge uses — displayed total and charge cannot drift.
	total, err := domain.SumPreorderItems(built)
	if err != nil {
		return nil, err
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
	return buildPreorder(bookingID, saved)
}

// resolveRelation authorizes the caller AND reports in what capacity they pass:
// an admin always passes; venue staff pass for their OWN restaurant; the
// booking's owner passes as a guest. Everyone else gets ErrNotFound (a plain
// guest asking about someone else's booking must not learn it exists — same
// enumeration-oracle guard as usecase/bookings.authorize), except a
// restaurant-role caller managing another venue, who gets the clearer
// ErrForbidden.
//
// The staff check runs BEFORE the owner check, but only for a restaurant-role
// caller, so it costs no extra query for an ordinary guest. The order matters
// for the one person who is both: a staff member who booked a table at their
// own venue is resolved as STAFF, i.e. keeps the venue's ability to change a
// confirmed pre-order. They are the venue; the lock protects the venue from
// unilateral guest edits, and there is nothing to protect it from here. Staff
// who booked at SOMEBODY ELSE's venue fall through to the owner check and are
// plain guests there, as they should be.
func (u *UseCase) resolveRelation(ctx context.Context, actor Actor, b *domain.Booking) (relation, error) {
	if actor.UserID == uuid.Nil {
		return 0, fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role == domain.RoleAdmin {
		return relationAdmin, nil
	}
	owner := b.UserID != nil && *b.UserID == actor.UserID
	if actor.Role == domain.RoleRestaurant {
		ok, err := u.managers.Manages(ctx, actor.UserID, b.RestaurantID)
		if err != nil {
			return 0, err
		}
		if ok {
			return relationStaff, nil
		}
		if owner {
			return relationGuest, nil
		}
		return 0, fmt.Errorf("%w: booking belongs to another restaurant", domain.ErrForbidden)
	}
	if owner {
		return relationGuest, nil
	}
	return 0, fmt.Errorf("%w: booking", domain.ErrNotFound)
}

// buildPreorder assembles the result. The total uses the SAME shared helper
// (domain.SumPreorderItems) as usecase/payments.resolveAmount, so the total
// shown here is byte-for-byte the amount that will be charged — a single source
// of truth, not two parallel loops that could drift. Currency is taken from the
// lines (all KZT today); an empty pre-order reports KZT so the client always has
// a currency.
func buildPreorder(bookingID uuid.UUID, items []domain.BookingItem) (*Preorder, error) {
	total, err := domain.SumPreorderItems(items)
	if err != nil {
		return nil, err
	}
	out := &Preorder{BookingID: bookingID, Items: items, TotalMinor: total, Currency: string(domain.CurrencyKZT)}
	for _, it := range items {
		if it.Status == domain.BookingItemCancelled {
			continue
		}
		if it.Currency != "" {
			out.Currency = it.Currency
		}
	}
	return out, nil
}

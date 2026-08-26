package preorder

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---- fakes -----------------------------------------------------------------

type fakeBookings struct{ b *domain.Booking }

func (f fakeBookings) GetByID(_ context.Context, id uuid.UUID) (*domain.Booking, error) {
	if f.b == nil || f.b.ID != id {
		return nil, domain.ErrNotFound
	}
	cp := *f.b
	return &cp, nil
}

type fakeMenu struct{ items map[uuid.UUID]domain.MenuItem }

func (f fakeMenu) GetByID(_ context.Context, id uuid.UUID) (*domain.MenuItem, error) {
	mi, ok := f.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := mi
	return &cp, nil
}

// fakeItems is an in-memory booking_items store. ReplaceForBooking mirrors the
// real repo: delete this booking's rows, insert the given set (assigning ids).
type fakeItems struct {
	byBooking    map[uuid.UUID][]domain.BookingItem
	replaceCalls int
	lastReplaced []domain.BookingItem
}

func newFakeItems() *fakeItems {
	return &fakeItems{byBooking: map[uuid.UUID][]domain.BookingItem{}}
}

func (f *fakeItems) ListByBooking(_ context.Context, bookingID uuid.UUID) ([]domain.BookingItem, error) {
	return f.byBooking[bookingID], nil
}

func (f *fakeItems) ReplaceForBooking(_ context.Context, bookingID uuid.UUID, items []domain.BookingItem) error {
	f.replaceCalls++
	stored := make([]domain.BookingItem, 0, len(items))
	for i := range items {
		it := items[i]
		it.BookingID = bookingID
		if it.ID == uuid.Nil {
			it.ID = uuid.New()
		}
		stored = append(stored, it)
	}
	f.byBooking[bookingID] = stored
	f.lastReplaced = stored
	return nil
}

func (f *fakeItems) Create(_ context.Context, _ []domain.BookingItem) error { return nil }
func (f *fakeItems) SetStatus(_ context.Context, _ uuid.UUID, _ domain.BookingItemStatus) error {
	return nil
}

type fakeSettings struct {
	override domain.PaymentSettingsOverride
}

func (f fakeSettings) GetPaymentOverride(_ context.Context, _ uuid.UUID) (domain.PaymentSettingsOverride, error) {
	return f.override, nil
}

type fakeManagers struct{ manages map[string]bool }

func (f fakeManagers) Manages(_ context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	return f.manages[userID.String()+"|"+restaurantID.String()], nil
}

// fakePayments reports whether a non-terminal payment is in flight for the
// booking. A `created` or any live/captured payment sets inFlight=true; a
// terminal (failed/expired/voided/refunded) payment sets it false.
type fakePayments struct{ inFlight bool }

func (f fakePayments) HasInFlightForBooking(_ context.Context, _ uuid.UUID) (bool, error) {
	return f.inFlight, nil
}

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (fakeTx) Detach(ctx context.Context) context.Context                         { return ctx }

// ---- harness ---------------------------------------------------------------

var (
	restA = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	restB = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
)

type harness struct {
	uc       *UseCase
	items    *fakeItems
	settings *fakeSettings
	payments *fakePayments
	booking  *domain.Booking
	dishA    domain.MenuItem // restA, 4500.00, available
	dishB    domain.MenuItem // restA, 1000.00, available
}

func newHarness(t *testing.T, ownerID *uuid.UUID, status domain.BookingStatus, manages map[string]bool) *harness {
	t.Helper()
	bookingID := uuid.New()
	b := &domain.Booking{ID: bookingID, RestaurantID: restA, UserID: ownerID, Status: status}

	dishA := domain.MenuItem{ID: uuid.New(), RestaurantID: restA, Name: "Beshbarmak", Price: "4500.00", IsAvailable: true}
	dishB := domain.MenuItem{ID: uuid.New(), RestaurantID: restA, Name: "Tea", Price: "1000.00", IsAvailable: true}

	items := newFakeItems()
	settings := &fakeSettings{}
	payments := &fakePayments{}
	uc := NewUseCase(
		fakeBookings{b: b},
		fakeMenu{items: map[uuid.UUID]domain.MenuItem{dishA.ID: dishA, dishB.ID: dishB}},
		items,
		settings,
		fakeManagers{manages: manages},
		payments,
		fakeTx{},
	)
	return &harness{uc: uc, items: items, settings: settings, payments: payments, booking: b, dishA: dishA, dishB: dishB}
}

func mustErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// ---- tests -----------------------------------------------------------------

// The core money invariant: the total is computed from the MENU item prices,
// server-side. The request carries no price at all — a guest cannot influence
// the charged amount.
func TestReplace_TotalFromMenuPrices(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	p, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{
		{MenuItemID: h.dishA.ID, Quantity: 2}, // 4500.00 * 2 = 900000 tiyn
		{MenuItemID: h.dishB.ID, Quantity: 1}, // 1000.00 * 1 = 100000 tiyn
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if p.TotalMinor != 1000000 {
		t.Errorf("total = %d, want 1000000", p.TotalMinor)
	}
	if len(p.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(p.Items))
	}
	// Each stored line carries the menu price snapshot in minor units.
	for _, it := range p.Items {
		switch it.MenuItemID != nil && *it.MenuItemID == h.dishA.ID {
		case true:
			if it.PriceMinor != 450000 {
				t.Errorf("dishA price = %d, want 450000", it.PriceMinor)
			}
		}
	}
	if h.items.replaceCalls != 1 {
		t.Errorf("replace calls = %d, want 1", h.items.replaceCalls)
	}
}

// An item belonging to a DIFFERENT restaurant is rejected — nothing is written.
func TestReplace_ItemFromAnotherRestaurant(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	foreign := domain.MenuItem{ID: uuid.New(), RestaurantID: restB, Name: "Sushi", Price: "3000.00", IsAvailable: true}
	h.uc.menu = fakeMenu{items: map[uuid.UUID]domain.MenuItem{foreign.ID: foreign}}
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: foreign.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called despite cross-tenant item")
	}
}

func TestReplace_UnavailableItem(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	h.dishA.IsAvailable = false
	h.uc.menu = fakeMenu{items: map[uuid.UUID]domain.MenuItem{h.dishA.ID: h.dishA}}
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called for an unavailable item")
	}
}

func TestReplace_UnknownItem(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	actor := Actor{UserID: owner, Role: domain.RoleUser}
	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: uuid.New(), Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
}

func TestReplace_QuantityBounds(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	actor := Actor{UserID: owner, Role: domain.RoleUser}
	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 0}})
	mustErr(t, err, domain.ErrValidation)
	_, err = h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: maxQtyPerLine + 1}})
	mustErr(t, err, domain.ErrValidation)
}

func TestReplace_MinimumEnforced(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	min := int64(500000) // 5000 KZT floor
	h.settings.override = domain.PaymentSettingsOverride{PreorderMinAmountMinor: &min}
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	// dishB * 1 = 100000 < 500000 → rejected.
	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishB.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called below the minimum")
	}

	// dishA * 2 = 900000 >= 500000 → ok.
	if _, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 2}}); err != nil {
		t.Fatalf("replace at/above minimum: %v", err)
	}
}

// Clearing (empty lines) bypasses the minimum and writes an empty set.
func TestReplace_ClearBypassesMinimum(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	min := int64(500000)
	h.settings.override = domain.PaymentSettingsOverride{PreorderMinAmountMinor: &min}
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	p, err := h.uc.Replace(context.Background(), actor, h.booking.ID, nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if p.TotalMinor != 0 || len(p.Items) != 0 {
		t.Errorf("cleared preorder = %+v, want empty", p)
	}
	if h.items.replaceCalls != 1 {
		t.Errorf("replace calls = %d, want 1", h.items.replaceCalls)
	}
}

// A payment in flight (any non-terminal status, INCLUDING `created` — the
// amount is snapshotted at POST /payments and captured later by the webhook)
// freezes the pre-order. This is the blocking money-integrity case: without it,
// a guest could change the items during the created→authorized window while the
// webhook captures the OLD snapshotted amount.
func TestReplace_FrozenWhilePaymentInFlight(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	h.payments.inFlight = true // e.g. a payment in `created` (redirect/3DS in flight)
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called while a payment was in flight")
	}

	// After that payment reaches a TERMINAL state (failed/expired/voided/
	// refunded) the booking is free to be re-ordered again.
	h.payments.inFlight = false
	if _, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("replace after terminal payment: %v", err)
	}
	if h.items.replaceCalls != 1 {
		t.Errorf("replace was not applied after the payment went terminal")
	}
}

// A terminal booking cannot have its pre-order changed.
func TestReplace_TerminalBookingRejected(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingCancelled, nil)
	actor := Actor{UserID: owner, Role: domain.RoleUser}
	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
}

// ---- authorization ---------------------------------------------------------

func TestReplace_Authorization(t *testing.T) {
	owner := uuid.New()
	other := uuid.New()
	staff := uuid.New()

	// A different plain guest gets ErrNotFound (no enumeration oracle).
	h := newHarness(t, &owner, domain.BookingPending, nil)
	_, err := h.uc.Replace(context.Background(), Actor{UserID: other, Role: domain.RoleUser}, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrNotFound)

	// Staff of THIS venue may attach on the guest's behalf.
	h = newHarness(t, &owner, domain.BookingPending, map[string]bool{staff.String() + "|" + restA.String(): true})
	if _, err := h.uc.Replace(context.Background(), Actor{UserID: staff, Role: domain.RoleRestaurant}, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("venue staff replace: %v", err)
	}

	// Staff of ANOTHER venue gets ErrForbidden.
	h = newHarness(t, &owner, domain.BookingPending, nil) // manages nothing
	_, err = h.uc.Replace(context.Background(), Actor{UserID: staff, Role: domain.RoleRestaurant}, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrForbidden)

	// Admin bypasses.
	h = newHarness(t, &owner, domain.BookingPending, nil)
	if _, err := h.uc.Replace(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("admin replace: %v", err)
	}
}

// Get recomputes the total from the stored lines and enforces the same access.
func TestGet(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingPending, nil)
	menuID := h.dishA.ID
	h.items.byBooking[h.booking.ID] = []domain.BookingItem{
		{ID: uuid.New(), BookingID: h.booking.ID, MenuItemID: &menuID, ItemName: "Beshbarmak",
			PriceMinor: 450000, Currency: "KZT", Quantity: 2, Status: domain.BookingItemPending},
		{ID: uuid.New(), BookingID: h.booking.ID, ItemName: "Cancelled dish",
			PriceMinor: 999999, Currency: "KZT", Quantity: 1, Status: domain.BookingItemCancelled},
	}

	p, err := h.uc.Get(context.Background(), Actor{UserID: owner, Role: domain.RoleUser}, h.booking.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Cancelled line excluded from the total, same rule as resolveAmount.
	if p.TotalMinor != 900000 {
		t.Errorf("total = %d, want 900000 (cancelled line excluded)", p.TotalMinor)
	}

	// A stranger cannot read it.
	_, err = h.uc.Get(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser}, h.booking.ID)
	mustErr(t, err, domain.ErrNotFound)
}

// A guest booking with no account (UserID nil) is staff-only: a random logged-in
// user must not attach a pre-order to it.
func TestReplace_AccountlessBookingStaffOnly(t *testing.T) {
	h := newHarness(t, nil, domain.BookingPending, nil)
	_, err := h.uc.Replace(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser}, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrNotFound)
}

// ---- the confirmed-booking lock -------------------------------------------

// mustCode asserts the error carries a specific machine-readable code — the
// contract the mobile app branches on. The message text is not a contract.
func mustCode(t *testing.T, err error, want domain.ErrorCode) {
	t.Helper()
	got, ok := domain.CodeOf(err)
	if !ok {
		t.Fatalf("err = %v carries no error code, want %q", err, want)
	}
	if got != want {
		t.Fatalf("error code = %q, want %q", got, want)
	}
}

// Once the booking is CONFIRMED the guest may no longer change the pre-order:
// the venue has accepted the order and plans the kitchen around it. The refusal
// is a distinguishable code, not a generic validation string.
func TestReplace_GuestBlockedOnConfirmedBooking(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingConfirmed, nil)
	actor := Actor{UserID: owner, Role: domain.RoleUser}

	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustErr(t, err, domain.ErrValidation)
	mustCode(t, err, domain.CodePreorderLocked)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called by the guest on a confirmed booking")
	}

	// Clearing the pre-order is a change too — the guest cannot empty it either.
	_, err = h.uc.Replace(context.Background(), actor, h.booking.ID, nil)
	mustCode(t, err, domain.CodePreorderLocked)
	if h.items.replaceCalls != 0 {
		t.Errorf("guest cleared the pre-order of a confirmed booking")
	}
}

// The lock is scoped to `confirmed` only: pending and waitlist bookings stay
// fully editable by the guest, which is the normal pre-order flow.
func TestReplace_GuestStillAllowedOnPendingAndWaitlist(t *testing.T) {
	for _, st := range []domain.BookingStatus{domain.BookingPending, domain.BookingWaitlist} {
		t.Run(string(st), func(t *testing.T) {
			owner := uuid.New()
			h := newHarness(t, &owner, st, nil)
			actor := Actor{UserID: owner, Role: domain.RoleUser}

			p, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
			if err != nil {
				t.Fatalf("guest replace on %s: %v", st, err)
			}
			if p.TotalMinor != 450000 {
				t.Errorf("total = %d, want 450000", p.TotalMinor)
			}
			if h.items.replaceCalls != 1 {
				t.Errorf("replace calls = %d, want 1", h.items.replaceCalls)
			}
		})
	}
}

// The venue KEEPS the ability after confirmation: it is the party that cooks
// the order and takes the phone call when a dish runs out. Same for an admin.
func TestReplace_VenueStaffAllowedOnConfirmedBooking(t *testing.T) {
	owner := uuid.New()
	staff := uuid.New()
	h := newHarness(t, &owner, domain.BookingConfirmed,
		map[string]bool{staff.String() + "|" + restA.String(): true})

	if _, err := h.uc.Replace(context.Background(), Actor{UserID: staff, Role: domain.RoleRestaurant},
		h.booking.ID, []Line{{MenuItemID: h.dishB.ID, Quantity: 3}}); err != nil {
		t.Fatalf("venue staff replace on confirmed booking: %v", err)
	}
	if h.items.replaceCalls != 1 {
		t.Errorf("replace calls = %d, want 1", h.items.replaceCalls)
	}

	h = newHarness(t, &owner, domain.BookingConfirmed, nil)
	if _, err := h.uc.Replace(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("admin replace on confirmed booking: %v", err)
	}
}

// A staff member who booked a table at their OWN venue is resolved as staff, so
// the guest lock does not apply to them — they are the venue.
func TestReplace_StaffOwningTheirOwnBookingIsStaff(t *testing.T) {
	staff := uuid.New()
	h := newHarness(t, &staff, domain.BookingConfirmed,
		map[string]bool{staff.String() + "|" + restA.String(): true})

	if _, err := h.uc.Replace(context.Background(), Actor{UserID: staff, Role: domain.RoleRestaurant},
		h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("staff replace on own confirmed booking at own venue: %v", err)
	}
	if h.items.replaceCalls != 1 {
		t.Errorf("replace calls = %d, want 1", h.items.replaceCalls)
	}
}

// A restaurant-role user who booked at SOMEBODY ELSE's venue is an ordinary
// guest there: they pass authorization as the owner (not ErrForbidden) and the
// confirmed lock applies to them.
func TestReplace_RestaurantRoleGuestAtAnotherVenue(t *testing.T) {
	owner := uuid.New() // has RoleRestaurant, but manages nothing at restA
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	h := newHarness(t, &owner, domain.BookingPending, nil)
	if _, err := h.uc.Replace(context.Background(), actor, h.booking.ID,
		[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}}); err != nil {
		t.Fatalf("restaurant-role owner on a pending booking at another venue: %v", err)
	}

	h = newHarness(t, &owner, domain.BookingConfirmed, nil)
	_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustCode(t, err, domain.CodePreorderLocked)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called on a confirmed booking by a guest-at-another-venue")
	}
}

// The payment freeze is NOT weakened by the confirmed lock and stays a distinct
// code: it applies to venue staff and admins too (they must not move the amount
// out from under a payment that is already snapshotted), and it is temporary —
// unlike the lock, it lifts when the payment goes terminal.
func TestReplace_PaymentFreezeAppliesToStaffAndAdminToo(t *testing.T) {
	owner := uuid.New()
	staff := uuid.New()
	manages := map[string]bool{staff.String() + "|" + restA.String(): true}

	for _, tc := range []struct {
		name  string
		actor Actor
	}{
		{"guest", Actor{UserID: owner, Role: domain.RoleUser}},
		{"staff", Actor{UserID: staff, Role: domain.RoleRestaurant}},
		{"admin", Actor{UserID: uuid.New(), Role: domain.RoleAdmin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Pending, so the confirmed lock cannot be what refuses the guest.
			h := newHarness(t, &owner, domain.BookingPending, manages)
			h.payments.inFlight = true

			_, err := h.uc.Replace(context.Background(), tc.actor, h.booking.ID,
				[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
			mustErr(t, err, domain.ErrValidation)
			mustCode(t, err, domain.CodePreorderPaymentInFlight)
			if h.items.replaceCalls != 0 {
				t.Errorf("replace was called while a payment was in flight")
			}
		})
	}
}

// A confirmed booking with a payment in flight reports the PAYMENT code to the
// venue (the freeze is the reason it cannot be touched by anyone) and the LOCK
// code to the guest — the guest's answer must not depend on the payment state,
// otherwise the app would tell them to wait for a payment that will never
// unlock anything for them.
func TestReplace_ConfirmedAndPaymentInFlightCodes(t *testing.T) {
	owner := uuid.New()
	staff := uuid.New()
	manages := map[string]bool{staff.String() + "|" + restA.String(): true}

	h := newHarness(t, &owner, domain.BookingConfirmed, manages)
	h.payments.inFlight = true

	_, err := h.uc.Replace(context.Background(), Actor{UserID: owner, Role: domain.RoleUser},
		h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustCode(t, err, domain.CodePreorderLocked)

	_, err = h.uc.Replace(context.Background(), Actor{UserID: staff, Role: domain.RoleRestaurant},
		h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
	mustCode(t, err, domain.CodePreorderPaymentInFlight)
	if h.items.replaceCalls != 0 {
		t.Errorf("replace was called on a frozen booking")
	}
}

// A booking that is over is closed to EVERYONE, staff and admin included, and
// says so with its own code.
func TestReplace_ClosedBookingCodeAndAppliesToStaff(t *testing.T) {
	owner := uuid.New()
	staff := uuid.New()
	manages := map[string]bool{staff.String() + "|" + restA.String(): true}

	for _, st := range []domain.BookingStatus{domain.BookingArrived, domain.BookingCompleted,
		domain.BookingCancelled, domain.BookingNoShow} {
		t.Run(string(st), func(t *testing.T) {
			h := newHarness(t, &owner, st, manages)
			for _, actor := range []Actor{
				{UserID: owner, Role: domain.RoleUser},
				{UserID: staff, Role: domain.RoleRestaurant},
				{UserID: uuid.New(), Role: domain.RoleAdmin},
			} {
				_, err := h.uc.Replace(context.Background(), actor, h.booking.ID,
					[]Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
				mustErr(t, err, domain.ErrValidation)
				mustCode(t, err, domain.CodePreorderBookingClosed)
			}
			if h.items.replaceCalls != 0 {
				t.Errorf("replace was called on a %s booking", st)
			}
		})
	}
}

// Reading a confirmed booking's pre-order is untouched — the lock is on
// writing, and the guest must still be able to see what they ordered.
func TestGet_GuestStillReadsConfirmedPreorder(t *testing.T) {
	owner := uuid.New()
	h := newHarness(t, &owner, domain.BookingConfirmed, nil)
	menuID := h.dishA.ID
	h.items.byBooking[h.booking.ID] = []domain.BookingItem{
		{ID: uuid.New(), BookingID: h.booking.ID, MenuItemID: &menuID, ItemName: "Beshbarmak",
			PriceMinor: 450000, Currency: "KZT", Quantity: 1, Status: domain.BookingItemPending},
	}

	p, err := h.uc.Get(context.Background(), Actor{UserID: owner, Role: domain.RoleUser}, h.booking.ID)
	if err != nil {
		t.Fatalf("get on a confirmed booking: %v", err)
	}
	if p.TotalMinor != 450000 {
		t.Errorf("total = %d, want 450000", p.TotalMinor)
	}
}

package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The whole security story of the Telegram buttons is this one check: the
// authority is the chat, so the only thing standing between one venue and
// another venue's bookings is that the booking belongs to the restaurant the
// chat resolved to.
func TestDecideAsVenueRefusesAnotherVenuesBooking(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
	someoneElse := uuid.New()

	_, err := h.uc.DecideAsVenue(context.Background(), someoneElse, h.booking.ID, VenueDecisionConfirm)

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound (never ErrForbidden — an outsider must not learn the id exists), got %v", err)
	}
	if got := h.booking.Status; got != domain.BookingPending {
		t.Fatalf("booking must be untouched, got %s", got)
	}
}

func TestDecideAsVenueConfirmsItsOwnPendingBooking(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)

	res, err := h.uc.DecideAsVenue(context.Background(), h.booking.RestaurantID, h.booking.ID, VenueDecisionConfirm)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !res.Applied || res.Conflict {
		t.Fatalf("want applied, got %+v", res)
	}
	if got := res.Booking.Status; got != domain.BookingConfirmed {
		t.Fatalf("status = %s, want confirmed", got)
	}
	if res.Booking.ConfirmedAt == nil {
		t.Fatal("confirmed_at must be stamped")
	}
}

func TestDecideAsVenueRejectionIsAttributedToTheRestaurant(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)

	res, err := h.uc.DecideAsVenue(context.Background(), h.booking.RestaurantID, h.booking.ID, VenueDecisionReject)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !res.Applied {
		t.Fatal("want applied")
	}
	if got := res.Booking.Status; got != domain.BookingCancelled {
		t.Fatalf("status = %s, want cancelled", got)
	}
	if res.Booking.CancelledBy == nil || *res.Booking.CancelledBy != domain.CancelledByRestaurant {
		t.Fatalf("cancellation must be attributed to the restaurant, got %v", res.Booking.CancelledBy)
	}
}

// Two staff members tapping the same button, or one person tapping twice
// because Telegram felt slow. The venue's intent is satisfied either way, so
// this reports "nothing to do" rather than failing at them.
func TestDecideAsVenueIsIdempotent(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
	rid := h.booking.RestaurantID

	if _, err := h.uc.DecideAsVenue(context.Background(), rid, h.booking.ID, VenueDecisionConfirm); err != nil {
		t.Fatalf("first press: %v", err)
	}
	res, err := h.uc.DecideAsVenue(context.Background(), rid, h.booking.ID, VenueDecisionConfirm)
	if err != nil {
		t.Fatalf("second press must not fail: %v", err)
	}
	if res.Applied {
		t.Fatal("second press must report that nothing changed")
	}
	if res.Conflict {
		t.Fatal("a repeat of the same decision is not a conflict")
	}
}

// The guest cancelled while the alert sat unread. Confirming now would resurrect
// a booking the guest believes is gone, so the press is refused and the caller
// tells the venue what actually happened.
func TestDecideAsVenueReportsAConflictOnACancelledBooking(t *testing.T) {
	h := newStatusHarness(t, domain.BookingCancelled, 48*time.Hour)

	res, err := h.uc.DecideAsVenue(context.Background(), h.booking.RestaurantID, h.booking.ID, VenueDecisionConfirm)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.Applied {
		t.Fatal("a cancelled booking must not be confirmed by a late button press")
	}
	if !res.Conflict {
		t.Fatal("want a conflict so the venue is told the truth")
	}
	if got := res.Booking.Status; got != domain.BookingCancelled {
		t.Fatalf("status must stay cancelled, got %s", got)
	}
}

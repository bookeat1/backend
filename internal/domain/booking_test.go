package domain

import (
	"errors"
	"testing"
)

func TestValidateTransition(t *testing.T) {
	cases := []struct {
		name string
		from BookingStatus
		to   BookingStatus
		want error
	}{
		{"pending → confirmed", BookingPending, BookingConfirmed, nil},
		{"pending → waitlist", BookingPending, BookingWaitlist, nil},
		{"pending → cancelled", BookingPending, BookingCancelled, nil},
		{"waitlist → confirmed", BookingWaitlist, BookingConfirmed, nil},
		{"waitlist → cancelled", BookingWaitlist, BookingCancelled, nil},
		{"confirmed → arrived", BookingConfirmed, BookingArrived, nil},
		{"confirmed → cancelled", BookingConfirmed, BookingCancelled, nil},
		{"confirmed → no_show", BookingConfirmed, BookingNoShow, nil},
		{"arrived → completed", BookingArrived, BookingCompleted, nil},
		{"arrived → cancelled", BookingArrived, BookingCancelled, nil},

		{"pending → arrived skips confirm", BookingPending, BookingArrived, ErrInvalidStatus},
		// A no-show is the guest breaking a promise the venue accepted, so it
		// is reachable only from confirmed. A request the venue never answered
		// is closed as cancelled by the worker instead.
		{"pending → no_show", BookingPending, BookingNoShow, ErrInvalidStatus},
		{"waitlist → no_show", BookingWaitlist, BookingNoShow, ErrInvalidStatus},
		{"pending → completed", BookingPending, BookingCompleted, ErrInvalidStatus},
		{"waitlist → arrived", BookingWaitlist, BookingArrived, ErrInvalidStatus},
		{"confirmed → completed skips arrive", BookingConfirmed, BookingCompleted, ErrInvalidStatus},
		{"confirmed → waitlist", BookingConfirmed, BookingWaitlist, ErrInvalidStatus},
		{"arrived → no_show", BookingArrived, BookingNoShow, ErrInvalidStatus},
		{"cancelled → confirmed", BookingCancelled, BookingConfirmed, ErrInvalidStatus},
		{"completed → arrived", BookingCompleted, BookingArrived, ErrInvalidStatus},
		{"no_show → completed", BookingNoShow, BookingCompleted, ErrInvalidStatus},
		{"same status is not a transition", BookingConfirmed, BookingConfirmed, ErrInvalidStatus},

		{"unknown source", BookingStatus("draft"), BookingConfirmed, ErrValidation},
		{"unknown target", BookingPending, BookingStatus("paid"), ErrValidation},
		{"empty statuses", BookingStatus(""), BookingStatus(""), ErrValidation},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTransition(tc.from, tc.to)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateTransition(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
			}
			if want := tc.want == nil; CanTransition(tc.from, tc.to) != want {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", tc.from, tc.to, !want, want)
			}
		})
	}
}

func TestBookingStatusValid(t *testing.T) {
	cases := map[BookingStatus]bool{
		BookingPending:   true,
		BookingConfirmed: true,
		BookingWaitlist:  true,
		BookingArrived:   true,
		BookingCompleted: true,
		BookingCancelled: true,
		BookingNoShow:    true,
		"noshow":         false,
		"Pending":        false,
		"":               false,
	}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("BookingStatus(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}

// HoldsTable must mirror the DB trigger in migrations/0004_bookings.sql.
func TestBookingStatusHoldsTable(t *testing.T) {
	cases := map[BookingStatus]bool{
		BookingPending:   true,
		BookingConfirmed: true,
		BookingArrived:   true,
		BookingWaitlist:  false,
		BookingCompleted: false,
		BookingCancelled: false,
		BookingNoShow:    false,
	}
	for s, want := range cases {
		if got := s.HoldsTable(); got != want {
			t.Errorf("BookingStatus(%q).HoldsTable() = %v, want %v", s, got, want)
		}
	}
}

func TestBookingStatusTerminal(t *testing.T) {
	cases := map[BookingStatus]bool{
		BookingCompleted: true,
		BookingCancelled: true,
		BookingNoShow:    true,
		BookingPending:   false,
		BookingWaitlist:  false,
		BookingConfirmed: false,
		BookingArrived:   false,
	}
	for s, want := range cases {
		if got := s.Terminal(); got != want {
			t.Errorf("BookingStatus(%q).Terminal() = %v, want %v", s, got, want)
		}
	}
}

func TestTerminalStatusesHaveNoOutgoingTransitions(t *testing.T) {
	all := []BookingStatus{
		BookingPending, BookingConfirmed, BookingWaitlist, BookingArrived,
		BookingCompleted, BookingCancelled, BookingNoShow,
	}
	for _, from := range []BookingStatus{BookingCompleted, BookingCancelled, BookingNoShow} {
		for _, to := range all {
			if CanTransition(from, to) {
				t.Errorf("terminal %q must not transition to %q", from, to)
			}
		}
	}
}

func TestBookingSourceValid(t *testing.T) {
	cases := map[BookingSource]bool{
		SourceApp: true, SourceAdmin: true, SourcePhone: true, SourceWidget: true,
		"web": false, "": false,
	}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("BookingSource(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}

func TestCancelledByValid(t *testing.T) {
	cases := map[CancelledBy]bool{
		CancelledByGuest: true, CancelledByRestaurant: true, CancelledBySystem: true,
		"manager": false, "": false,
	}
	for c, want := range cases {
		if got := c.Valid(); got != want {
			t.Errorf("CancelledBy(%q).Valid() = %v, want %v", c, got, want)
		}
	}
}

func TestActorTypeValid(t *testing.T) {
	cases := map[ActorType]bool{
		ActorGuest: true, ActorManager: true, ActorAdmin: true, ActorSystem: true,
		"restaurant": false, "": false,
	}
	for a, want := range cases {
		if got := a.Valid(); got != want {
			t.Errorf("ActorType(%q).Valid() = %v, want %v", a, got, want)
		}
	}
}

func TestSenderTypeValid(t *testing.T) {
	cases := map[SenderType]bool{
		SenderGuest: true, SenderRestaurant: true, SenderSystem: true,
		"manager": false, "": false,
	}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("SenderType(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}

func TestBookingItemTotalMinor(t *testing.T) {
	it := BookingItem{PriceMinor: 450000, Quantity: 3}
	if got := it.TotalMinor(); got != 1350000 {
		t.Fatalf("TotalMinor() = %d, want 1350000", got)
	}
}

func TestValidRatingAndNPS(t *testing.T) {
	ratings := map[int]bool{1: true, 3: true, 5: true, 0: false, 6: false, -1: false}
	for v, want := range ratings {
		if got := ValidRating(v); got != want {
			t.Errorf("ValidRating(%d) = %v, want %v", v, got, want)
		}
	}
	nps := map[int]bool{0: true, 5: true, 10: true, -1: false, 11: false}
	for v, want := range nps {
		if got := ValidNPS(v); got != want {
			t.Errorf("ValidNPS(%d) = %v, want %v", v, got, want)
		}
	}
}

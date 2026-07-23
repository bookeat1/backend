package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type statusHarness struct {
	uc       StatusUseCase
	bookings *fakeBookings
	history  *fakeHistory
	outbox   *fakeOutbox
	tx       *fakeTx

	booking      *domain.Booking
	guest        Actor
	manager      Actor
	admin        Actor
	outsider     Actor
	otherManager Actor
}

func newStatusHarness(t *testing.T, status domain.BookingStatus, startsIn time.Duration) *statusHarness {
	t.Helper()
	rid := uuid.New()
	guestID := uuid.New()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Name: "Дамир",
		PhoneNormalized: "+77071234567", Guests: 2, Status: status,
		StartsAt: time.Now().Add(startsIn), EndsAt: time.Now().Add(startsIn + 2*time.Hour),
		Source: domain.SourceApp,
	}
	h := &statusHarness{
		bookings: newFakeBookings(b),
		history:  &fakeHistory{},
		outbox:   &fakeOutbox{},
		tx:       &fakeTx{},
		booking:  b,
		guest:    Actor{UserID: guestID, Role: domain.RoleUser},
		manager:  Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		admin:    Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		outsider: Actor{UserID: uuid.New(), Role: domain.RoleUser},
	}
	h.otherManager = Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}
	h.uc = NewStatusUseCase(
		h.bookings, h.history, h.outbox,
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		newFakeManagers([2]uuid.UUID{h.manager.UserID, rid}),
		h.tx, testConfig(),
	)
	return h
}

// Every edge of the state machine, allowed and forbidden, driven by staff.
func TestStatusTransitions(t *testing.T) {
	call := func(uc StatusUseCase, actor Actor, id uuid.UUID, to domain.BookingStatus) error {
		var err error
		switch to {
		case domain.BookingConfirmed:
			_, err = uc.Confirm(context.Background(), actor, id, nil)
		case domain.BookingWaitlist:
			_, err = uc.Waitlist(context.Background(), actor, id, nil)
		case domain.BookingArrived:
			_, err = uc.Arrive(context.Background(), actor, id)
		case domain.BookingCompleted:
			_, err = uc.Complete(context.Background(), actor, id)
		case domain.BookingNoShow:
			_, err = uc.NoShow(context.Background(), actor, id, nil)
		case domain.BookingCancelled:
			_, err = uc.Cancel(context.Background(), actor, id, CancelInput{})
		}
		return err
	}

	all := []domain.BookingStatus{
		domain.BookingPending, domain.BookingWaitlist, domain.BookingConfirmed,
		domain.BookingArrived, domain.BookingCompleted, domain.BookingCancelled,
		domain.BookingNoShow,
	}
	for _, from := range all {
		for _, to := range all {
			from, to := from, to
			if to == domain.BookingPending {
				continue // no API reaches pending: it is the creation state only
			}
			t.Run(string(from)+"→"+string(to), func(t *testing.T) {
				h := newStatusHarness(t, from, 48*time.Hour)
				err := call(h.uc, h.manager, h.booking.ID, to)
				if domain.CanTransition(from, to) {
					if err != nil {
						t.Fatalf("allowed transition failed: %v", err)
					}
					if len(h.bookings.statuses) != 1 || h.bookings.statuses[0].Status != to {
						t.Fatalf("status writes = %+v", h.bookings.statuses)
					}
					if len(h.history.created) != 1 || len(h.outbox.created) != 1 {
						t.Fatalf("history/outbox = %d/%d", len(h.history.created), len(h.outbox.created))
					}
					if h.tx.calls != 1 {
						t.Fatalf("transition must run in one transaction, got %d", h.tx.calls)
					}
					if *h.history.created[0].FromStatus != from || h.history.created[0].ToStatus != to {
						t.Fatalf("history row = %+v", h.history.created[0])
					}
					return
				}
				if !errors.Is(err, domain.ErrInvalidStatus) {
					t.Fatalf("forbidden transition = %v, want ErrInvalidStatus", err)
				}
				if len(h.bookings.statuses) != 0 || len(h.outbox.created) != 0 {
					t.Fatal("a rejected transition must not write anything")
				}
			})
		}
	}
}

func TestStatusActorPermissions(t *testing.T) {
	t.Run("manager of another restaurant is forbidden", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
		if _, err := h.uc.Confirm(context.Background(), h.otherManager, h.booking.ID, nil); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("= %v, want ErrForbidden", err)
		}
	})
	t.Run("unrelated guest is forbidden", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
		if _, err := h.uc.Cancel(context.Background(), h.outsider, h.booking.ID, CancelInput{}); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("= %v, want ErrForbidden", err)
		}
	})
	t.Run("unauthenticated actor is rejected", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
		if _, err := h.uc.Confirm(context.Background(), Actor{}, h.booking.ID, nil); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("= %v, want ErrUnauthorized", err)
		}
	})
	t.Run("admin may act on any restaurant", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
		b, err := h.uc.Confirm(context.Background(), h.admin, h.booking.ID, nil)
		if err != nil {
			t.Fatalf("admin confirm: %v", err)
		}
		if b.Status != domain.BookingConfirmed || h.history.created[0].ActorType != domain.ActorAdmin {
			t.Fatalf("admin confirm = %+v / %+v", b, h.history.created)
		}
	})

	// A guest may only cancel — never drive the venue-side transitions.
	forbidden := map[string]func(uc StatusUseCase, a Actor, id uuid.UUID) error{
		"confirm": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.Confirm(context.Background(), a, id, nil)
			return e
		},
		"reject": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.Reject(context.Background(), a, id, nil)
			return e
		},
		"arrive": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.Arrive(context.Background(), a, id)
			return e
		},
		"complete": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.Complete(context.Background(), a, id)
			return e
		},
		"no_show": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.NoShow(context.Background(), a, id, nil)
			return e
		},
		"waitlist": func(uc StatusUseCase, a Actor, id uuid.UUID) error {
			_, e := uc.Waitlist(context.Background(), a, id, nil)
			return e
		},
	}
	for name, fn := range forbidden {
		t.Run("guest may not "+name, func(t *testing.T) {
			h := newStatusHarness(t, domain.BookingConfirmed, 48*time.Hour)
			if err := fn(h.uc, h.guest, h.booking.ID); !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("guest %s = %v, want ErrForbidden", name, err)
			}
			if len(h.bookings.statuses) != 0 {
				t.Fatal("forbidden action must not write")
			}
		})
	}
}

func TestGuestCancelDeadline(t *testing.T) {
	t.Run("before the deadline", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingConfirmed, 4*time.Hour) // deadline is 3h before
		b, err := h.uc.Cancel(context.Background(), h.guest, h.booking.ID, CancelInput{Reason: sptr("changed plans")})
		if err != nil {
			t.Fatalf("guest cancel: %v", err)
		}
		if b.Status != domain.BookingCancelled || b.CancelledBy == nil || *b.CancelledBy != domain.CancelledByGuest {
			t.Fatalf("booking = %+v", b)
		}
		if b.CancelledAt == nil || b.CancellationReason == nil {
			t.Fatalf("cancellation metadata missing: %+v", b)
		}
		if len(h.bookings.updated) != 1 {
			t.Fatalf("cancellation metadata must be persisted, updates = %d", len(h.bookings.updated))
		}
		if h.history.created[0].ActorType != domain.ActorGuest {
			t.Fatalf("actor type = %s", h.history.created[0].ActorType)
		}
	})

	t.Run("after the deadline", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingConfirmed, 2*time.Hour) // inside the 3h window
		_, err := h.uc.Cancel(context.Background(), h.guest, h.booking.ID, CancelInput{})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("late guest cancel = %v, want ErrForbidden", err)
		}
		if len(h.bookings.statuses) != 0 || len(h.outbox.created) != 0 {
			t.Fatal("late cancel must not write anything")
		}
	})

	t.Run("the venue can still cancel after the deadline", func(t *testing.T) {
		h := newStatusHarness(t, domain.BookingConfirmed, 30*time.Minute)
		b, err := h.uc.Cancel(context.Background(), h.manager, h.booking.ID, CancelInput{})
		if err != nil {
			t.Fatalf("venue cancel: %v", err)
		}
		if *b.CancelledBy != domain.CancelledByRestaurant {
			t.Fatalf("cancelled_by = %s", *b.CancelledBy)
		}
	})
}

func TestRejectIsRestaurantCancellation(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
	b, err := h.uc.Reject(context.Background(), h.manager, h.booking.ID, sptr("fully booked"))
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if b.Status != domain.BookingCancelled || *b.CancelledBy != domain.CancelledByRestaurant {
		t.Fatalf("booking = %+v", b)
	}
	if h.outbox.types()[0] != domain.EventBookingCancelled {
		t.Fatalf("event = %v", h.outbox.types())
	}
	if h.history.created[0].Reason == nil || *h.history.created[0].Reason != "fully booked" {
		t.Fatalf("history reason = %+v", h.history.created[0])
	}
}

func TestStatusUnknownBooking(t *testing.T) {
	h := newStatusHarness(t, domain.BookingPending, 48*time.Hour)
	if _, err := h.uc.Confirm(context.Background(), h.manager, uuid.New(), nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("= %v, want ErrNotFound", err)
	}
}

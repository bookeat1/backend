package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type updateHarness struct {
	uc       UpdateUseCase
	bookings *fakeBookings
	links    *fakeLinks
	outbox   *fakeOutbox
	tx       *fakeTx

	booking      *domain.Booking
	restaurantID uuid.UUID
	guest        Actor
	manager      Actor
	otherManager Actor
}

func newUpdateHarness(t *testing.T, status domain.BookingStatus) *updateHarness {
	t.Helper()
	rid := uuid.New()
	guestID := uuid.New()
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Hour)
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Name: "Дамир",
		PhoneNormalized: "+77071234567", Guests: 2, Status: status,
		StartsAt: start, EndsAt: start.Add(2 * time.Hour), Source: domain.SourceApp,
	}
	h := &updateHarness{
		bookings:     newFakeBookings(b),
		links:        &fakeLinks{},
		outbox:       &fakeOutbox{},
		tx:           &fakeTx{},
		booking:      b,
		restaurantID: rid,
		guest:        Actor{UserID: guestID, Role: domain.RoleUser},
		manager:      Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		otherManager: Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
	}
	h.uc = NewUpdateUseCase(
		h.bookings, h.links, h.outbox,
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		&fakeSchedule{tables: []domain.RestaurantTable{
			{ID: uuid.New(), RestaurantID: rid, Capacity: 4, IsActive: true},
		}},
		newFakeManagers([2]uuid.UUID{h.manager.UserID, rid}),
		h.tx, testConfig(),
	)
	return h
}

// Access control on PATCH: the venue's own staff may amend, a guest may not,
// and a manager of another venue is rejected with 403 (spec §7).
func TestUpdateAccess(t *testing.T) {
	cases := []struct {
		name  string
		actor func(h *updateHarness) Actor
		want  error
	}{
		{"venue manager may amend", func(h *updateHarness) Actor { return h.manager }, nil},
		{"owner guest may not", func(h *updateHarness) Actor { return h.guest }, domain.ErrForbidden},
		{"manager of another venue", func(h *updateHarness) Actor { return h.otherManager }, domain.ErrForbidden},
		{"unrelated guest", func(h *updateHarness) Actor {
			return Actor{UserID: uuid.New(), Role: domain.RoleUser}
		}, domain.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newUpdateHarness(t, domain.BookingConfirmed)
			guests := 3
			_, err := h.uc.Update(context.Background(), tc.actor(h), h.booking.ID, UpdateInput{Guests: &guests})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A booking that no longer holds its tables cannot be rescheduled.
func TestUpdateTerminalBooking(t *testing.T) {
	for _, s := range []domain.BookingStatus{domain.BookingCancelled, domain.BookingCompleted, domain.BookingNoShow} {
		h := newUpdateHarness(t, s)
		guests := 3
		_, err := h.uc.Update(context.Background(), h.manager, h.booking.ID, UpdateInput{Guests: &guests})
		if !errors.Is(err, domain.ErrInvalidStatus) {
			t.Errorf("%s: err = %v, want ErrInvalidStatus", s, err)
		}
	}
}

// Moving a booking in time recomputes ends_at from the venue policy, replaces
// the table links and emits exactly one outbox event, all inside one tx.
func TestUpdateReschedule(t *testing.T) {
	h := newUpdateHarness(t, domain.BookingConfirmed)
	newStart := h.booking.StartsAt.Add(time.Hour)
	out, err := h.uc.Update(context.Background(), h.manager, h.booking.ID, UpdateInput{StartsAt: &newStart, Force: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Booking.StartsAt.Equal(newStart) {
		t.Errorf("starts_at = %v, want %v", out.Booking.StartsAt, newStart)
	}
	if want := newStart.Add(2 * time.Hour); !out.Booking.EndsAt.Equal(want) {
		t.Errorf("ends_at = %v, want %v", out.Booking.EndsAt, want)
	}
	if h.tx.calls != 1 {
		t.Errorf("tx entered %d times, want 1", h.tx.calls)
	}
	if len(h.outbox.created) != 1 || h.outbox.created[0].EventType != domain.EventBookingUpdated {
		t.Errorf("outbox = %+v, want one booking.updated event", h.outbox.created)
	}
}

// A notes-only edit must not touch the placement.
func TestUpdateNotesOnlyKeepsTables(t *testing.T) {
	h := newUpdateHarness(t, domain.BookingConfirmed)
	notes := "  window seat  "
	out, err := h.uc.Update(context.Background(), h.manager, h.booking.ID, UpdateInput{Notes: &notes})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Booking.Notes == nil || *out.Booking.Notes != "window seat" {
		t.Errorf("notes = %v, want trimmed", out.Booking.Notes)
	}
	if len(h.links.created) != 0 {
		t.Errorf("links rewritten on a notes-only edit: %+v", h.links.created)
	}
}

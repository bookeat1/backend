package bookings

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type settlerCall struct {
	bookingID uuid.UUID
	trigger   domain.RefundTrigger
}

type fakeDepositSettler struct {
	mu    sync.Mutex
	calls []settlerCall
}

func (f *fakeDepositSettler) SettleDepositOnCancel(_ context.Context, id uuid.UUID, trigger domain.RefundTrigger, _ *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, settlerCall{id, trigger})
	return nil
}

func (f *fakeDepositSettler) triggerFor(id uuid.UUID) (domain.RefundTrigger, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.bookingID == id {
			return c.trigger, true
		}
	}
	return "", false
}

// A guest cancelling in time drives a guest_cancel deposit settlement (the
// payments layer then decides void vs capture from the window).
func TestStatus_GuestCancelSettlesDeposit(t *testing.T) {
	rid := uuid.New()
	guestID := uuid.New()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Name: "G",
		PhoneNormalized: "+77071234567", Guests: 2, Status: domain.BookingConfirmed,
		StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(26 * time.Hour),
		Source: domain.SourceApp,
	}
	settler := &fakeDepositSettler{}
	uc := NewStatusUseCase(newFakeBookings(b), &fakeHistory{}, &fakeOutbox{},
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		newFakeManagers([2]uuid.UUID{uuid.New(), rid}),
		&fakeTx{}, testConfig(),
		WithDepositSettler(settler))

	guest := Actor{UserID: guestID, Role: domain.RoleUser}
	if _, err := uc.Cancel(context.Background(), guest, b.ID, CancelInput{}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	got, ok := settler.triggerFor(b.ID)
	if !ok {
		t.Fatalf("deposit settlement was not invoked on guest cancel")
	}
	if got != domain.RefundTriggerGuestCancel {
		t.Fatalf("trigger = %s, want guest_cancel", got)
	}
}

// A venue rejection drives a venue_cancel settlement (the hold is released).
func TestStatus_VenueRejectSettlesDeposit(t *testing.T) {
	rid := uuid.New()
	managerID := uuid.New()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, Name: "G",
		PhoneNormalized: "+77071234567", Guests: 2, Status: domain.BookingPending,
		StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(26 * time.Hour),
		Source: domain.SourceApp,
	}
	settler := &fakeDepositSettler{}
	uc := NewStatusUseCase(newFakeBookings(b), &fakeHistory{}, &fakeOutbox{},
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		newFakeManagers([2]uuid.UUID{managerID, rid}),
		&fakeTx{}, testConfig(),
		WithDepositSettler(settler))

	manager := Actor{UserID: managerID, Role: domain.RoleRestaurant}
	if _, err := uc.Reject(context.Background(), manager, b.ID, nil); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	got, ok := settler.triggerFor(b.ID)
	if !ok {
		t.Fatalf("deposit settlement was not invoked on venue reject")
	}
	if got != domain.RefundTriggerVenueCancel {
		t.Fatalf("trigger = %s, want venue_cancel", got)
	}
}

// The no-show worker forfeits the held deposit: it invokes the settler with the
// no_show trigger for each booking it closes as a no-show.
func TestWorker_NoShowSettlesDeposit(t *testing.T) {
	h := newWorkerHarness(t)
	rid := uuid.New()
	h.venue(rid, nil, nil)
	confirmed := h.booking(rid, domain.BookingConfirmed, 5*time.Hour, time.Hour) // ended > grace ago
	h.bookings.byID[confirmed.ID] = confirmed

	settler := &fakeDepositSettler{}
	w := NewWorker(h.bookings, h.history, h.outbox, h.rests, h.tx, testConfig(),
		WorkerConfig{TickInterval: time.Minute, NoShowGrace: 30 * time.Minute, BatchSize: 100},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithWorkerDepositSettler(settler))
	w.now = func() time.Time { return h.now }

	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if h.statusOf(confirmed.ID) != domain.BookingNoShow {
		t.Fatalf("booking status = %s, want no_show", h.statusOf(confirmed.ID))
	}
	got, ok := settler.triggerFor(confirmed.ID)
	if !ok {
		t.Fatalf("no-show deposit settlement was not invoked")
	}
	if got != domain.RefundTriggerNoShow {
		t.Fatalf("trigger = %s, want no_show", got)
	}
}

// Item 5 (single-window): a guest cancelling LATE (well inside the free-cancel
// window) is NOT blocked — the booking is cancelled and the deposit settlement
// still fires with the guest_cancel trigger (the payments layer then captures
// it to the venue). This is the "cancel always allowed, money depends on the
// window" behaviour.
func TestStatus_LateGuestCancelStillSettlesDeposit(t *testing.T) {
	rid := uuid.New()
	guestID := uuid.New()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Name: "G",
		PhoneNormalized: "+77071234567", Guests: 2, Status: domain.BookingConfirmed,
		StartsAt: time.Now().Add(20 * time.Minute), EndsAt: time.Now().Add(2 * time.Hour), // minutes away → late
		Source: domain.SourceApp,
	}
	settler := &fakeDepositSettler{}
	uc := NewStatusUseCase(newFakeBookings(b), &fakeHistory{}, &fakeOutbox{},
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, IsActive: true}}},
		newFakeManagers([2]uuid.UUID{uuid.New(), rid}),
		&fakeTx{}, testConfig(),
		WithDepositSettler(settler))

	guest := Actor{UserID: guestID, Role: domain.RoleUser}
	got, err := uc.Cancel(context.Background(), guest, b.ID, CancelInput{})
	if err != nil {
		t.Fatalf("late guest Cancel must be allowed, got %v", err)
	}
	if got.Status != domain.BookingCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	if trig, ok := settler.triggerFor(b.ID); !ok || trig != domain.RefundTriggerGuestCancel {
		t.Fatalf("settler trigger = %v (found=%v), want guest_cancel", trig, ok)
	}
}

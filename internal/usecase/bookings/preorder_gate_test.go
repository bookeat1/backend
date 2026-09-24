package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Owner decisions 2026-09-24: a booking behind an unpaid pre-order is hidden
// from the venue; a held pre-order is captured on confirmation; venue silence
// and non-payment cancel.

type fakeCapturer struct {
	calls int
	err   error
}

func (f *fakeCapturer) CaptureOnConfirm(context.Context, uuid.UUID) error {
	f.calls++
	return f.err
}

func newGateStatusHarness(t *testing.T, status domain.BookingStatus, hidden bool, cap PreorderCapturer) *statusHarness {
	t.Helper()
	h := newStatusHarness(t, status, 48*time.Hour)
	if hidden {
		h.booking.ReleasedToVenueAt = nil
	}
	h.uc = NewStatusUseCase(h.bookings, h.history, h.outbox,
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: h.booking.RestaurantID, IsActive: true}}},
		newFakeManagers([2]uuid.UUID{h.manager.UserID, h.booking.RestaurantID}),
		h.tx, testConfig(), WithPreorderCapturer(cap))
	return h
}

func TestConfirmHiddenBookingIsRefusedWithNarrowCode(t *testing.T) {
	c := &fakeCapturer{}
	h := newGateStatusHarness(t, domain.BookingPending, true, c)
	_, err := h.uc.Confirm(context.Background(), h.manager, h.booking.ID, nil)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists (409)", err)
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeBookingAwaitingPayment {
		t.Fatalf("code = %q, want %q", code, domain.CodeBookingAwaitingPayment)
	}
	if c.calls != 0 || len(h.bookings.statuses) != 0 {
		t.Fatalf("a refused confirm must capture nothing and write nothing")
	}
}

func TestGuestMayCancelHiddenBooking(t *testing.T) {
	h := newGateStatusHarness(t, domain.BookingPending, true, &fakeCapturer{})
	if _, err := h.uc.Cancel(context.Background(), h.guest, h.booking.ID, CancelInput{}); err != nil {
		t.Fatalf("guest cancel of a hidden booking: %v", err)
	}
}

func TestConfirmCapturesTheHoldOnce(t *testing.T) {
	c := &fakeCapturer{}
	h := newGateStatusHarness(t, domain.BookingPending, false, c)
	if _, err := h.uc.Confirm(context.Background(), h.manager, h.booking.ID, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("capture calls = %d, want 1", c.calls)
	}
}

func TestConfirmWithDefinitiveDeclineCancelsAsSystem(t *testing.T) {
	c := &fakeCapturer{err: domain.ErrProviderDeclined}
	h := newGateStatusHarness(t, domain.BookingPending, false, c)
	if _, err := h.uc.Confirm(context.Background(), h.manager, h.booking.ID, nil); err != nil {
		t.Fatalf("confirm itself must succeed: %v", err)
	}
	b := lastUpdated(h.bookings, h.booking.ID)
	if h.bookings.byID[h.booking.ID].Status != domain.BookingCancelled || b == nil {
		t.Fatalf("status = %s, want cancelled after a declined capture", b.Status)
	}
	if b.CancelledBy == nil || *b.CancelledBy != domain.CancelledBySystem ||
		b.CancellationReasonCode == nil || *b.CancellationReasonCode != domain.CancelReasonPreorderCaptureFailed {
		t.Fatalf("cancelled_by/reason = %v/%v, want system/preorder_capture_failed", b.CancelledBy, b.CancellationReasonCode)
	}
}

func TestConfirmWithUnknownCaptureOutcomeStaysConfirmed(t *testing.T) {
	c := &fakeCapturer{err: errors.New("timeout")}
	h := newGateStatusHarness(t, domain.BookingPending, false, c)
	if _, err := h.uc.Confirm(context.Background(), h.manager, h.booking.ID, nil); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := h.bookings.byID[h.booking.ID].Status; got != domain.BookingConfirmed {
		t.Fatalf("status = %s, want confirmed (the reconciler finishes the capture)", got)
	}
}

func TestCancelPendingOnHoldLostOnlyTouchesPending(t *testing.T) {
	for _, tc := range []struct {
		from domain.BookingStatus
		want domain.BookingStatus
	}{
		{domain.BookingPending, domain.BookingCancelled},
		{domain.BookingConfirmed, domain.BookingConfirmed},
		{domain.BookingCancelled, domain.BookingCancelled},
	} {
		h := newGateStatusHarness(t, tc.from, false, &fakeCapturer{})
		u := h.uc.(interface {
			CancelPendingOnHoldLost(context.Context, uuid.UUID) error
		})
		for i := 0; i < 2; i++ { // idempotent
			if err := u.CancelPendingOnHoldLost(context.Background(), h.booking.ID); err != nil {
				t.Fatalf("%s: %v", tc.from, err)
			}
		}
		if got := h.bookings.byID[h.booking.ID].Status; got != tc.want {
			t.Fatalf("from %s: status = %s, want %s", tc.from, got, tc.want)
		}
	}
}

// --- worker ---

type fakeRelease struct{ bookings *fakeBookings }

func (f fakeRelease) Release(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	b := f.bookings.byID[id]
	if b == nil || b.Status != domain.BookingPending || b.ReleasedToVenueAt != nil {
		return false, nil
	}
	b.ReleasedToVenueAt = &at
	return true, nil
}

func (f fakeRelease) ClaimUnpaidHidden(_ context.Context, before time.Time, limit int) ([]domain.Booking, error) {
	var out []domain.Booking
	for _, b := range f.bookings.byID {
		if b.Status == domain.BookingPending && b.ReleasedToVenueAt == nil && b.CreatedAt.Before(before) {
			out = append(out, *b)
		}
	}
	return out, nil
}

type fakeHolds map[uuid.UUID]bool

func (f fakeHolds) HasPreorderHold(_ context.Context, id uuid.UUID) (bool, error) { return f[id], nil }

func TestWorkerCancelsUnpaidHiddenBookingAfterTTL(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	old := h.booking(rid, domain.BookingPending, 31*time.Minute, -5*time.Hour)
	old.ReleasedToVenueAt = nil
	fresh := h.booking(rid, domain.BookingPending, 10*time.Minute, -5*time.Hour)
	fresh.ReleasedToVenueAt = nil
	h.bookings.byID[old.ID], h.bookings.byID[fresh.ID] = old, fresh
	h.w.release, h.w.holds = fakeRelease{h.bookings}, fakeHolds{}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Unpaid != 1 || h.statusOf(old.ID) != domain.BookingCancelled || h.statusOf(fresh.ID) != domain.BookingPending {
		t.Fatalf("res=%+v old=%s fresh=%s, want only the 31-minute-old booking cancelled", res, h.statusOf(old.ID), h.statusOf(fresh.ID))
	}
	if code := lastUpdated(h.bookings, old.ID).CancellationReasonCode; code == nil || *code != domain.CancelReasonPreorderPaymentNotCompleted {
		t.Fatalf("reason = %v", code)
	}
}

// Silence of the venue is a cancellation for a held pre-order, never a confirmation
// — even with auto_confirm on (owner decision 1).
func TestWorkerVenueSilenceCancelsHeldBookingInsteadOfAutoConfirm(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil) // auto_confirm on, SLA 120m
	held := h.booking(rid, domain.BookingPending, 3*time.Hour, -5*time.Hour)
	plain := h.booking(rid, domain.BookingPending, 3*time.Hour, -5*time.Hour) // no hold: old behaviour
	h.bookings.byID[held.ID], h.bookings.byID[plain.ID] = held, plain
	h.w.release, h.w.holds = fakeRelease{h.bookings}, fakeHolds{held.ID: true}
	// The venue was told 3h ago (past its 120m SLA), and starts_at is far ahead.
	for _, b := range []*domain.Booking{held, plain} {
		rel := h.now.Add(-3 * time.Hour)
		b.ReleasedToVenueAt = &rel
		b.StartsAt = h.now.Add(48 * time.Hour)
		b.EndsAt = b.StartsAt.Add(2 * time.Hour)
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if h.statusOf(held.ID) != domain.BookingCancelled || res.NoAnswer != 1 {
		t.Fatalf("held: status=%s res=%+v, want cancelled/NoAnswer=1", h.statusOf(held.ID), res)
	}
	if code := lastUpdated(h.bookings, held.ID).CancellationReasonCode; code == nil || *code != domain.CancelReasonVenueNoAnswer {
		t.Fatalf("reason = %v, want venue_no_answer", code)
	}
	if h.statusOf(plain.ID) != domain.BookingConfirmed {
		t.Fatalf("a booking without a hold keeps auto-confirming, got %s", h.statusOf(plain.ID))
	}
}

// The SLA of a held booking starts at release, not at created_at.
func TestWorkerHeldBookingSLACountsFromRelease(t *testing.T) {
	rid := uuid.New()
	h := newWorkerHarness(t)
	h.venue(rid, nil, nil)
	b := h.booking(rid, domain.BookingPending, 5*time.Hour, -5*time.Hour) // created long ago
	rel := h.now.Add(-20 * time.Minute)                                   // but released 20 min ago
	b.ReleasedToVenueAt = &rel
	b.StartsAt = h.now.Add(48 * time.Hour)
	b.EndsAt = b.StartsAt.Add(2 * time.Hour)
	h.bookings.byID[b.ID] = b
	h.w.release, h.w.holds = fakeRelease{h.bookings}, fakeHolds{b.ID: true}

	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if h.statusOf(b.ID) != domain.BookingPending {
		t.Fatalf("status = %s, want pending: the venue has had 20 of 120 minutes", h.statusOf(b.ID))
	}
}

// lastUpdated is the last full-row Update the fake recorded for a booking (the
// fake keeps them apart from the status writes).
func lastUpdated(f *fakeBookings, id uuid.UUID) *domain.Booking {
	for i := len(f.updated) - 1; i >= 0; i-- {
		if f.updated[i].ID == id {
			return f.updated[i]
		}
	}
	return nil
}

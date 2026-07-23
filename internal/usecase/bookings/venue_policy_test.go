package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakePolicyWriter applies the patch onto an in-memory aggregate the way the
// repository does — only non-nil fields are written — so the usecase test also
// pins the PATCH semantics the transport layer relies on.
type fakePolicyWriter struct {
	agg    *domain.RestaurantAggregate
	calls  int
	lastIn domain.BookingPolicyOverride
	err    error
}

func (f *fakePolicyWriter) UpdateBookingPolicy(_ context.Context, _ uuid.UUID, o domain.BookingPolicyOverride) error {
	if f.err != nil {
		return f.err
	}
	f.calls++
	f.lastIn = o
	p := &f.agg.BookingPolicy
	if o.Timezone != nil {
		p.Timezone = o.Timezone
	}
	if o.BookingDurationMinutes != nil {
		p.BookingDurationMinutes = o.BookingDurationMinutes
	}
	if o.BookingBufferMinutes != nil {
		p.BookingBufferMinutes = o.BookingBufferMinutes
	}
	if o.BookingLeadMinutes != nil {
		p.BookingLeadMinutes = o.BookingLeadMinutes
	}
	if o.BookingHorizonDays != nil {
		p.BookingHorizonDays = o.BookingHorizonDays
	}
	if o.CancelDeadlineMinutes != nil {
		p.CancelDeadlineMinutes = o.CancelDeadlineMinutes
	}
	if o.ConfirmSLAMinutes != nil {
		p.ConfirmSLAMinutes = o.ConfirmSLAMinutes
	}
	if o.MaxGuestsPerBooking != nil {
		p.MaxGuestsPerBooking = o.MaxGuestsPerBooking
	}
	if o.AutoConfirm != nil {
		p.AutoConfirm = o.AutoConfirm
	}
	return nil
}

type policyHarness struct {
	uc     PolicyUseCase
	writer *fakePolicyWriter
	rid    uuid.UUID
}

func newPolicyHarness(t *testing.T, managerID uuid.UUID) *policyHarness {
	t.Helper()
	rid := uuid.New()
	agg := &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid, Name: "Venue"}}
	writer := &fakePolicyWriter{agg: agg}
	cfg := Config{
		DefaultDuration: 120 * time.Minute, DefaultLead: 60 * time.Minute,
		DefaultHorizonDays: 60, DefaultConfirmSLA: 120 * time.Minute,
		DefaultMaxGuests: 20, DefaultAutoConfirm: true, TimezoneFallback: "Asia/Almaty",
	}
	uc := NewPolicyUseCase(
		&fakeRestaurants{agg: agg},
		writer,
		newFakeManagers([2]uuid.UUID{managerID, rid}),
		cfg,
	)
	return &policyHarness{uc: uc, writer: writer, rid: rid}
}

func TestPolicyUpdateAuthorization(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	ctx := context.Background()
	patch := domain.BookingPolicyOverride{AutoConfirm: bptr(false)}

	t.Run("own manager may patch", func(t *testing.T) {
		view, err := h.uc.Update(ctx, Actor{UserID: owner, Role: domain.RoleRestaurant}, h.rid, patch)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if view.Effective.AutoConfirm {
			t.Errorf("effective auto_confirm = true, want false")
		}
	})

	t.Run("admin may patch", func(t *testing.T) {
		if _, err := h.uc.Update(ctx, Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, h.rid, patch); err != nil {
			t.Fatalf("admin update: %v", err)
		}
	})

	t.Run("manager of another venue is forbidden", func(t *testing.T) {
		before := h.writer.calls
		_, err := h.uc.Update(ctx, Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}, h.rid, patch)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
		if h.writer.calls != before {
			t.Errorf("writer was called despite 403")
		}
	})

	t.Run("plain guest is forbidden", func(t *testing.T) {
		_, err := h.uc.Update(ctx, Actor{UserID: uuid.New(), Role: domain.RoleUser}, h.rid, patch)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("unauthenticated actor is rejected", func(t *testing.T) {
		_, err := h.uc.Update(ctx, Actor{}, h.rid, patch)
		if !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("get is gated the same way", func(t *testing.T) {
		_, err := h.uc.Get(ctx, Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}, h.rid)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})
}

func TestPolicyUpdateValidation(t *testing.T) {
	owner := uuid.New()
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}

	cases := []struct {
		name  string
		in    domain.BookingPolicyOverride
		valid bool
	}{
		{"unknown timezone", domain.BookingPolicyOverride{Timezone: sptr("Mars/Olympus")}, false},
		{"empty timezone", domain.BookingPolicyOverride{Timezone: sptr("  ")}, false},
		{"valid timezone", domain.BookingPolicyOverride{Timezone: sptr("Asia/Almaty")}, true},
		{"duration too short", domain.BookingPolicyOverride{BookingDurationMinutes: iptr(5)}, false},
		{"duration too long", domain.BookingPolicyOverride{BookingDurationMinutes: iptr(601)}, false},
		{"duration at lower bound", domain.BookingPolicyOverride{BookingDurationMinutes: iptr(15)}, true},
		{"negative buffer", domain.BookingPolicyOverride{BookingBufferMinutes: iptr(-1)}, false},
		{"zero buffer is meaningful", domain.BookingPolicyOverride{BookingBufferMinutes: iptr(0)}, true},
		{"negative lead", domain.BookingPolicyOverride{BookingLeadMinutes: iptr(-30)}, false},
		{"horizon zero", domain.BookingPolicyOverride{BookingHorizonDays: iptr(0)}, false},
		{"horizon too far", domain.BookingPolicyOverride{BookingHorizonDays: iptr(366)}, false},
		{"horizon at upper bound", domain.BookingPolicyOverride{BookingHorizonDays: iptr(365)}, true},
		{"negative cancel deadline", domain.BookingPolicyOverride{CancelDeadlineMinutes: iptr(-5)}, false},
		{"sla zero", domain.BookingPolicyOverride{ConfirmSLAMinutes: iptr(0)}, false},
		{"guests zero", domain.BookingPolicyOverride{MaxGuestsPerBooking: iptr(0)}, false},
		{"guests too many", domain.BookingPolicyOverride{MaxGuestsPerBooking: iptr(101)}, false},
		{"guests at upper bound", domain.BookingPolicyOverride{MaxGuestsPerBooking: iptr(100)}, true},
		{"auto_confirm off", domain.BookingPolicyOverride{AutoConfirm: bptr(false)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPolicyHarness(t, owner)
			_, err := h.uc.Update(context.Background(), actor, h.rid, tc.in)
			switch {
			case tc.valid && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case !tc.valid && !errors.Is(err, domain.ErrValidation):
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if !tc.valid && h.writer.calls != 0 {
				t.Errorf("invalid patch reached the repository")
			}
		})
	}
}

// TestPolicyUpdatePatchSemantics pins that omitted fields keep inheriting the
// global default while the provided ones take effect.
func TestPolicyUpdatePatchSemantics(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	actor := Actor{UserID: owner, Role: domain.RoleRestaurant}
	ctx := context.Background()

	if _, err := h.uc.Update(ctx, actor, h.rid, domain.BookingPolicyOverride{
		BookingDurationMinutes: iptr(90),
		AutoConfirm:            bptr(false),
	}); err != nil {
		t.Fatalf("first patch: %v", err)
	}

	view, err := h.uc.Update(ctx, actor, h.rid, domain.BookingPolicyOverride{MaxGuestsPerBooking: iptr(8)})
	if err != nil {
		t.Fatalf("second patch: %v", err)
	}
	if view.Effective.Duration != 90*time.Minute {
		t.Errorf("duration = %v, want 90m preserved by the second patch", view.Effective.Duration)
	}
	if view.Effective.AutoConfirm {
		t.Errorf("auto_confirm = true, want the earlier override to survive")
	}
	if view.Effective.MaxGuestsPerBooking != 8 {
		t.Errorf("max guests = %d, want 8", view.Effective.MaxGuestsPerBooking)
	}
	// Never-touched fields still resolve from Config.
	if view.Effective.HorizonDays != 60 || view.Effective.Timezone != "Asia/Almaty" {
		t.Errorf("inherited defaults lost: %+v", view.Effective)
	}
	if view.Override.BookingLeadMinutes != nil {
		t.Errorf("lead override = %v, want nil (still inherited)", view.Override.BookingLeadMinutes)
	}
}

// TestPolicyUpdateTrimsTimezone guards against a stored " Asia/Almaty" that
// validates on write but fails time.LoadLocation on read.
func TestPolicyUpdateTrimsTimezone(t *testing.T) {
	owner := uuid.New()
	h := newPolicyHarness(t, owner)
	view, err := h.uc.Update(context.Background(), Actor{UserID: owner, Role: domain.RoleRestaurant},
		h.rid, domain.BookingPolicyOverride{Timezone: sptr("  Europe/Berlin  ")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if h.writer.lastIn.Timezone == nil || *h.writer.lastIn.Timezone != "Europe/Berlin" {
		t.Fatalf("stored timezone = %v, want trimmed", h.writer.lastIn.Timezone)
	}
	if view.Effective.Timezone != "Europe/Berlin" {
		t.Errorf("effective timezone = %q", view.Effective.Timezone)
	}
}

// TestPolicyUpdatePropagatesNotFound makes sure a missing venue surfaces as
// ErrNotFound for an admin (a non-manager restaurant user is stopped by 403
// before the repository is reached).
func TestPolicyUpdatePropagatesNotFound(t *testing.T) {
	h := newPolicyHarness(t, uuid.New())
	h.writer.err = domain.ErrNotFound
	_, err := h.uc.Update(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		h.rid, domain.BookingPolicyOverride{AutoConfirm: bptr(true)})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

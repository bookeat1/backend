package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeRepo records the args it was called with and returns canned data.
type fakeRepo struct {
	overview domain.PlatformOverview

	bookingCounts []domain.BookingStatusCount
	captured      domain.MoneyAggregate
	refunded      domain.MoneyAggregate
	topBookings   []domain.TopRestaurant
	topGMV        []domain.TopRestaurant

	err error

	// captured call args
	gotFrom, gotTo any
	gotCurrency    string
	gotLimit       int
	byGMVCalled    bool
	byBookCalled   bool
}

func (f *fakeRepo) Overview(context.Context) (domain.PlatformOverview, error) {
	return f.overview, f.err
}
func (f *fakeRepo) BookingsByStatus(_ context.Context, from, to any) ([]domain.BookingStatusCount, error) {
	f.gotFrom, f.gotTo = from, to
	return f.bookingCounts, f.err
}
func (f *fakeRepo) PaymentsGMV(_ context.Context, from, to any, currency string) (domain.MoneyAggregate, domain.MoneyAggregate, error) {
	f.gotFrom, f.gotTo, f.gotCurrency = from, to, currency
	return f.captured, f.refunded, f.err
}
func (f *fakeRepo) TopRestaurantsByBookings(_ context.Context, from, to any, limit int) ([]domain.TopRestaurant, error) {
	f.gotFrom, f.gotTo, f.gotLimit, f.byBookCalled = from, to, limit, true
	return f.topBookings, f.err
}
func (f *fakeRepo) TopRestaurantsByGMV(_ context.Context, from, to any, currency string, limit int) ([]domain.TopRestaurant, error) {
	f.gotFrom, f.gotTo, f.gotCurrency, f.gotLimit, f.byGMVCalled = from, to, currency, limit, true
	return f.topGMV, f.err
}

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

// Every non-superadmin role must be forbidden on every method, and the repo
// must never be touched.
func TestAllMethodsRejectNonSuperadmin(t *testing.T) {
	roles := []domain.Role{domain.RoleUser, domain.RoleRestaurant, domain.Role("owner"), domain.Role("")}
	for _, role := range roles {
		f := &fakeRepo{}
		u := NewUseCase(f)
		actor := Actor{UserID: uuid.New(), Role: role}

		if _, err := u.Overview(context.Background(), actor); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("Overview role %q: got %v, want ErrForbidden", role, err)
		}
		if _, err := u.BookingsBreakdown(context.Background(), actor, time.Time{}, time.Time{}); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("BookingsBreakdown role %q: got %v, want ErrForbidden", role, err)
		}
		if _, err := u.PaymentsGMV(context.Background(), actor, time.Time{}, time.Time{}, ""); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("PaymentsGMV role %q: got %v, want ErrForbidden", role, err)
		}
		if _, err := u.TopRestaurants(context.Background(), actor, time.Time{}, time.Time{}, "", "", 0); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("TopRestaurants role %q: got %v, want ErrForbidden", role, err)
		}
		if f.byBookCalled || f.byGMVCalled || f.gotFrom != nil {
			t.Fatalf("role %q: repo was touched despite forbidden actor", role)
		}
	}
}

func TestBookingsBreakdownZeroFillsAllStatuses(t *testing.T) {
	f := &fakeRepo{bookingCounts: []domain.BookingStatusCount{
		{Status: domain.BookingConfirmed, Count: 3},
		{Status: domain.BookingCancelled, Count: 2},
	}}
	u := NewUseCase(f)
	b, err := u.BookingsBreakdown(context.Background(), superadmin(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if b.Total != 5 {
		t.Fatalf("total: got %d, want 5", b.Total)
	}
	if len(b.ByStatus) != 7 {
		t.Fatalf("by_status len: got %d, want 7 (every known status)", len(b.ByStatus))
	}
	got := map[domain.BookingStatus]int64{}
	for _, s := range b.ByStatus {
		got[s.Status] = s.Count
	}
	if got[domain.BookingConfirmed] != 3 || got[domain.BookingCancelled] != 2 {
		t.Fatalf("populated statuses wrong: %+v", got)
	}
	if got[domain.BookingPending] != 0 || got[domain.BookingNoShow] != 0 {
		t.Fatalf("absent statuses must be zero, not missing: %+v", got)
	}
}

func TestEmptyPlatformIsZerosNotError(t *testing.T) {
	f := &fakeRepo{} // all zero values, no rows
	u := NewUseCase(f)
	b, err := u.BookingsBreakdown(context.Background(), superadmin(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("breakdown on empty platform: %v", err)
	}
	if b.Total != 0 || len(b.ByStatus) != 7 {
		t.Fatalf("empty platform: total=%d statuses=%d, want 0 / 7", b.Total, len(b.ByStatus))
	}
	for _, s := range b.ByStatus {
		if s.Count != 0 {
			t.Fatalf("empty platform status %s: got %d, want 0", s.Status, s.Count)
		}
	}
}

func TestPeriodDefaultAndValidation(t *testing.T) {
	fixedNow := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	f := &fakeRepo{}
	u := NewUseCase(f)
	u.now = func() time.Time { return fixedNow }

	// Default: to=now, from=now-30d.
	if _, err := u.BookingsBreakdown(context.Background(), superadmin(), time.Time{}, time.Time{}); err != nil {
		t.Fatalf("default period: %v", err)
	}
	if f.gotTo.(time.Time) != fixedNow {
		t.Fatalf("default to: got %v, want %v", f.gotTo, fixedNow)
	}
	wantFrom := fixedNow.Add(-defaultPeriod)
	if f.gotFrom.(time.Time) != wantFrom {
		t.Fatalf("default from: got %v, want %v", f.gotFrom, wantFrom)
	}

	// Explicit narrowing window is passed through verbatim.
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	if _, err := u.BookingsBreakdown(context.Background(), superadmin(), from, to); err != nil {
		t.Fatalf("explicit period: %v", err)
	}
	if f.gotFrom.(time.Time) != from || f.gotTo.(time.Time) != to {
		t.Fatalf("explicit period passthrough: got [%v,%v]", f.gotFrom, f.gotTo)
	}

	// from >= to is a validation error, repo untouched for that call.
	if _, err := u.BookingsBreakdown(context.Background(), superadmin(), to, from); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("inverted period: got %v, want ErrValidation", err)
	}
	if _, err := u.PaymentsGMV(context.Background(), superadmin(), to, to, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero-width period: got %v, want ErrValidation", err)
	}
}

func TestPaymentsCurrencyDefaultsAndPassesThrough(t *testing.T) {
	f := &fakeRepo{captured: domain.MoneyAggregate{AmountMinor: 500000, Count: 4}, refunded: domain.MoneyAggregate{AmountMinor: 120000, Count: 1}}
	u := NewUseCase(f)

	p, err := u.PaymentsGMV(context.Background(), superadmin(), time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("gmv: %v", err)
	}
	if p.Currency != "KZT" {
		t.Fatalf("default currency: got %q, want KZT", p.Currency)
	}
	if f.gotCurrency != "KZT" {
		t.Fatalf("repo currency: got %q, want KZT", f.gotCurrency)
	}
	if p.Captured.AmountMinor != 500000 || p.Captured.Count != 4 {
		t.Fatalf("captured passthrough wrong: %+v", p.Captured)
	}
	if p.Refunded.AmountMinor != 120000 || p.Refunded.Count != 1 {
		t.Fatalf("refunded passthrough wrong: %+v", p.Refunded)
	}

	// Lowercase currency is normalized to upper.
	if _, err := u.PaymentsGMV(context.Background(), superadmin(), time.Time{}, time.Time{}, "usd"); err != nil {
		t.Fatalf("gmv usd: %v", err)
	}
	if f.gotCurrency != "USD" {
		t.Fatalf("currency normalize: got %q, want USD", f.gotCurrency)
	}
}

func TestTopRestaurantsDimensionRouting(t *testing.T) {
	f := &fakeRepo{}
	u := NewUseCase(f)

	// Default dimension is bookings.
	if _, err := u.TopRestaurants(context.Background(), superadmin(), time.Time{}, time.Time{}, "", "", 0); err != nil {
		t.Fatalf("top default: %v", err)
	}
	if !f.byBookCalled || f.byGMVCalled {
		t.Fatalf("default dimension should be bookings")
	}
	if f.gotLimit != defaultTopLimit {
		t.Fatalf("default limit: got %d, want %d", f.gotLimit, defaultTopLimit)
	}

	// by=gmv routes to the GMV repo method with the currency.
	f2 := &fakeRepo{}
	u2 := NewUseCase(f2)
	if _, err := u2.TopRestaurants(context.Background(), superadmin(), time.Time{}, time.Time{}, "gmv", "kzt", 100); err != nil {
		t.Fatalf("top gmv: %v", err)
	}
	if !f2.byGMVCalled || f2.byBookCalled {
		t.Fatalf("by=gmv should route to GMV method")
	}
	if f2.gotCurrency != "KZT" {
		t.Fatalf("gmv currency: got %q, want KZT", f2.gotCurrency)
	}
	if f2.gotLimit != maxTopLimit {
		t.Fatalf("limit clamp: got %d, want %d", f2.gotLimit, maxTopLimit)
	}

	// Unknown dimension is a validation error.
	if _, err := u.TopRestaurants(context.Background(), superadmin(), time.Time{}, time.Time{}, "revenue", "", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown dimension: got %v, want ErrValidation", err)
	}
}

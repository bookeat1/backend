package booking

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	paymentrepo "backend-core/internal/infrastructure/postgres/payment"
)

func hiddenBooking(rid uuid.UUID) *domain.Booking {
	b := newBooking(rid, time.Now().Add(48*time.Hour))
	b.ReleasedToVenueAt = nil // the gate's explicit NULL
	return b
}

func TestHiddenBookingListingAndRelease(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	rid := seedRestaurant(t, pool)

	hidden := hiddenBooking(rid)
	visible := newBooking(rid, time.Now().Add(49*time.Hour))
	now := time.Now()
	visible.ReleasedToVenueAt = &now
	for _, b := range []*domain.Booking{hidden, visible} {
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	ridc := rid
	venue, _, err := repo.List(ctx, domain.BookingFilter{RestaurantID: &ridc, HideUnreleased: true})
	if err != nil || len(venue) != 1 || venue[0].ID != visible.ID {
		t.Fatalf("venue list = %v err=%v, want only the released booking", venue, err)
	}
	admin, _, err := repo.List(ctx, domain.BookingFilter{RestaurantID: &ridc})
	if err != nil || len(admin) != 2 {
		t.Fatalf("admin list has %d rows err=%v, want both", len(admin), err)
	}
	got, _ := repo.GetByID(ctx, hidden.ID)
	if !got.AwaitingPreorderPayment() {
		t.Fatalf("hidden booking must report AwaitingPreorderPayment")
	}

	// Release is idempotent: exactly one caller wins.
	won, err := repo.Release(ctx, hidden.ID, time.Now())
	if err != nil || !won {
		t.Fatalf("first release = %v, %v, want true", won, err)
	}
	if won, _ = repo.Release(ctx, hidden.ID, time.Now()); won {
		t.Fatalf("second release reported a win")
	}
	// A cancelled booking cannot be released back to the venue.
	h2 := hiddenBooking(rid)
	if err := repo.Create(ctx, h2); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, h2.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatal(err)
	}
	if won, _ = repo.Release(ctx, h2.ID, time.Now()); won {
		t.Fatalf("a cancelled booking was released")
	}
}

func TestClaimUnpaidHiddenSparesLiveLinks(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	pay := paymentrepo.New(pool)
	rid := seedRestaurant(t, pool)

	noPayment, liveLink, expiredLink := hiddenBooking(rid), hiddenBooking(rid), hiddenBooking(rid)
	for _, b := range []*domain.Booking{noPayment, liveLink, expiredLink} {
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE bookings SET created_at = now() - interval '2 hours' WHERE id=$1`, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(b *domain.Booking, exp time.Time) {
		base, fee := int64(500000), int64(17500)
		p := &domain.Payment{
			ID: uuid.New(), BookingID: b.ID, RestaurantID: rid, Provider: domain.ProviderFreedomPay,
			Purpose: domain.PurposePreorder, Status: domain.PaymentCreated, AmountMinor: base + fee,
			BaseAmountMinor: base, FeeMinor: fee, Currency: domain.CurrencyKZT,
			IdempotencyKey: b.ID.String() + ":k", ExpiresAt: &exp, RequiresConfirmation: true,
		}
		if err := pay.Create(ctx, p); err != nil {
			t.Fatalf("payment: %v", err)
		}
	}
	mk(liveLink, time.Now().Add(10*time.Minute))
	mk(expiredLink, time.Now().Add(-time.Minute))

	var ids []uuid.UUID
	rows, err := repo.ClaimUnpaidHidden(ctx, time.Now().Add(-30*time.Minute), 100)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	has := func(id uuid.UUID) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	if !has(noPayment.ID) || !has(expiredLink.ID) || has(liveLink.ID) {
		t.Fatalf("claimed %v: want no-payment and expired-link claimed, live link spared (criterion 20)", ids)
	}
}

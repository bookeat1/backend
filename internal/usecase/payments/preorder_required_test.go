package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// preorderRequiredHarness builds a create usecase for a booking with one paid
// pre-order line (2 x 300000). The venue override and the global config are the
// two inputs of the pre-order payment flag under test.
func preorderRequiredHarness(t *testing.T, override domain.PaymentSettingsOverride, cfg Config) (CreateUseCase, uuid.UUID, uuid.UUID, *fakeRestaurantSettings) {
	t.Helper()
	b := testBooking(uuid.New())
	payments := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	items := newFakeItemReader()
	items.byBooking[b.ID] = []domain.BookingItem{
		{ID: uuid.New(), BookingID: b.ID, Quantity: 2, PriceMinor: 300_000, Status: domain.BookingItemPending},
	}
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = override
	gw := newFakeGateway(domain.ProviderFreedomPay)
	tx := &fakeTx{payments: payments, outbox: outbox}
	cfg.ServiceFeeBps = 350
	u := NewCreateUseCase(payments, outbox, newFakeBookingReader(b), items, settings, newFakeSpecialDays(),
		newFakeGatewayResolver(gw), newFakeManagerChecker(), tx, cfg)
	return u, b.ID, b.RestaurantID, settings
}

// The flag the guest app reads is the very value the checkout resolves: the
// venue's own setting wins, a NULL inherits the platform default.
func TestPreorderPaymentRequired_ResolutionMatchesCheckout(t *testing.T) {
	cases := []struct {
		name     string
		venue    *bool
		global   bool
		want     bool
		wantPurp domain.PaymentPurpose // "" = checkout refuses (no payment owed)
	}{
		{"venue true, global off", boolPtr(true), false, true, domain.PurposePreorder},
		{"venue false, global on", boolPtr(false), true, false, ""},
		{"venue NULL, global off", nil, false, false, ""},
		{"venue NULL, global on", nil, true, true, domain.PurposePreorder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, bookingID, rid, _ := preorderRequiredHarness(t,
				domain.PaymentSettingsOverride{PaymentsEnabled: boolPtr(true), PreorderPaymentRequired: tc.venue},
				Config{PreorderPaymentRequired: tc.global})

			got, err := u.PreorderPaymentRequired(context.Background(), rid)
			if err != nil {
				t.Fatalf("PreorderPaymentRequired() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("PreorderPaymentRequired() = %v, want %v", got, tc.want)
			}

			// The checkout agrees with the flag: required -> the pre-order total is
			// charged as PurposePreorder; not required -> 422 "requires no payment".
			p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: bookingID, IdempotencyKey: "k"})
			if tc.wantPurp == "" {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("CreateForBooking() error = %v, want ErrValidation (requires no payment)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateForBooking() error = %v", err)
			}
			if p.Purpose != tc.wantPurp || p.BaseAmountMinor != 600_000 {
				t.Fatalf("purpose=%s base=%d, want %s / 600000 (sum of the pre-order items)", p.Purpose, p.BaseAmountMinor, tc.wantPurp)
			}
		})
	}
}

// The flag is a plain settings read: it is reported even when the venue's
// payments master switch is off (the app combines it with accepts_online_payment).
func TestPreorderPaymentRequired_IndependentOfMasterSwitch(t *testing.T) {
	u, _, rid, _ := preorderRequiredHarness(t,
		domain.PaymentSettingsOverride{PaymentsEnabled: boolPtr(false), PreorderPaymentRequired: boolPtr(true)}, Config{})
	got, err := u.PreorderPaymentRequired(context.Background(), rid)
	if err != nil || !got {
		t.Fatalf("PreorderPaymentRequired() = %v, %v; want true, nil with payments off", got, err)
	}
}

func TestPreorderPaymentRequired_Errors(t *testing.T) {
	u, _, rid, settings := preorderRequiredHarness(t, domain.PaymentSettingsOverride{}, Config{})
	if _, err := u.PreorderPaymentRequired(context.Background(), uuid.Nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil restaurant: error = %v, want ErrValidation", err)
	}
	// A failed settings read propagates (never published as false).
	settings.err = errors.New("db down")
	if _, err := u.PreorderPaymentRequired(context.Background(), rid); err == nil {
		t.Fatal("failed read: error = nil, want the settings read error to propagate")
	}
}

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

const (
	testVenueSplitAccount    = "sub_merchant_venue"
	testPlatformSplitAccount = "sub_merchant_platform"
)

// fakeSplitAccounts is the venue↔sub-merchant mapping. An account absent from
// the map is a venue that has not been onboarded to acquiring — which, as of
// 2026-08-20, is EVERY venue.
type fakeSplitAccounts struct {
	byRestaurant map[uuid.UUID]string
	calls        int
}

func newFakeSplitAccounts() *fakeSplitAccounts {
	return &fakeSplitAccounts{byRestaurant: map[uuid.UUID]string{}}
}

func (f *fakeSplitAccounts) GetActive(_ context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error) {
	f.calls++
	ref, ok := f.byRestaurant[restaurantID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return &domain.RestaurantSplitAccount{
		RestaurantID: restaurantID, Provider: provider, AccountRef: ref, IsActive: true,
	}, nil
}

// newSplitCreateHarness is newCreateHarness with split payments switched on.
func newSplitCreateHarness(t *testing.T, b *domain.Booking, deposit int64, feeBps int) (*createUseCase, *fakeGateway, *fakeSplitAccounts) {
	t.Helper()
	payments := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	bookings := newFakeBookingReader(b)
	items := newFakeItemReader()
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{
		PaymentsEnabled:    boolPtr(true),
		DepositRequired:    boolPtr(true),
		DepositAmountMinor: int64Ptr(deposit),
		ServiceFeeBps:      intPtr(feeBps),
	}
	gw := newFakeGateway(domain.ProviderTipTopPay)
	resolver := newFakeGatewayResolver(gw)
	accounts := newFakeSplitAccounts()
	tx := &fakeTx{payments: payments, outbox: outbox}
	u := NewCreateUseCase(payments, outbox, bookings, items, settings, newFakeSpecialDays(), resolver,
		newFakeManagerChecker(), tx,
		Config{
			DefaultProvider:         domain.ProviderTipTopPay,
			SplitEnabled:            true,
			PlatformSplitAccountRef: testPlatformSplitAccount,
		},
		WithSplitAccounts(accounts),
	).(*createUseCase)
	return u, gw, accounts
}

// TestCreateForBookingSplitsBaseToVenueAndFeeToPlatform is the happy path: the
// division handed to the acquirer is the SAME one the ledger books later — base
// to the venue, service fee to the platform — and it adds up to the charge.
func TestCreateForBookingSplitsBaseToVenueAndFeeToPlatform(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw, accounts := newSplitCreateHarness(t, b, 1_000_000, 350)
	accounts.byRestaurant[b.RestaurantID] = testVenueSplitAccount

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "key-split-1", ReturnURL: "https://app/return",
	})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}

	splits := gw.lastAuthorize.Splits
	if len(splits) != 2 {
		t.Fatalf("splits = %+v, want two shares", splits)
	}
	if splits[0].Payee != domain.SplitPayeeVenue ||
		splits[0].AccountRef != testVenueSplitAccount ||
		splits[0].Amount.AmountMinor != p.BaseAmountMinor {
		t.Fatalf("venue share = %+v, want the base %d to %s", splits[0], p.BaseAmountMinor, testVenueSplitAccount)
	}
	if splits[1].Payee != domain.SplitPayeePlatform ||
		splits[1].AccountRef != testPlatformSplitAccount ||
		splits[1].Amount.AmountMinor != p.FeeMinor {
		t.Fatalf("platform share = %+v, want the fee %d to %s", splits[1], p.FeeMinor, testPlatformSplitAccount)
	}
	// The invariant the acquirer checks too, only here it costs nothing.
	if err := splits.Validate(p.Total()); err != nil {
		t.Fatalf("the plan handed to the acquirer does not add up: %v", err)
	}
	// And it is exactly what the ledger will book on capture: nothing here is a
	// second, parallel notion of commission.
	if splits[0].Amount.AmountMinor+splits[1].Amount.AmountMinor != p.AmountMinor {
		t.Fatalf("shares %d + %d != charge %d",
			splits[0].Amount.AmountMinor, splits[1].Amount.AmountMinor, p.AmountMinor)
	}
}

// TestCreateForBookingRefusesAVenueWithoutASubMerchantAccount is the owner's
// rule (2026-08-20) and the state EVERY venue is in today: no identifier means
// no payment, refused before the acquirer is called — never a charge that
// quietly lands the venue's money on the platform's account.
func TestCreateForBookingRefusesAVenueWithoutASubMerchantAccount(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw, accounts := newSplitCreateHarness(t, b, 1_000_000, 350)
	// Deliberately NOT registered: accounts.byRestaurant stays empty.

	_, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "key-split-2", ReturnURL: "https://app/return",
	})
	if err == nil {
		t.Fatalf("CreateForBooking() = nil, want a refusal")
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeSplitAccountMissing {
		t.Fatalf("code = %q (present=%v), want %q — err: %v", code, ok, domain.CodeSplitAccountMissing, err)
	}
	// 503, not 422: the guest did nothing wrong, this deployment is not ready
	// for that venue yet.
	if !isUnavailable(err) {
		t.Fatalf("err = %v, want it to wrap domain.ErrUnavailable", err)
	}
	if accounts.calls != 1 {
		t.Fatalf("split account looked up %d times, want 1", accounts.calls)
	}
	if gw.callCount("authorize") != 0 {
		t.Fatalf("the acquirer was called %d times for a venue that cannot receive money", gw.callCount("authorize"))
	}
}

// TestCreateForBookingWithoutSplitEnabledIsUnchanged pins the default: until
// venues have sub-merchant identifiers the feature is off, and a payment is
// created exactly as it always was — no lookup, no splits, no new failure mode.
func TestCreateForBookingWithoutSplitEnabledIsUnchanged(t *testing.T) {
	b := testBooking(uuid.New())
	u, _, _, _ := newCreateHarness(t, b, 1_000_000, 350)

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "key-split-3", ReturnURL: "https://app/return",
	})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, want created", p.Status)
	}
}

// TestCreateForBookingSplitDoesNotSurviveAMisconfiguredPlatformAccount: with
// splits on and a non-zero fee, the platform's own sub-merchant id is not
// optional — the shares would not add up without it, and the acquirer would
// answer "Amount is not equal to request amount" after taking a round trip.
func TestCreateForBookingSplitDoesNotSurviveAMisconfiguredPlatformAccount(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw, accounts := newSplitCreateHarness(t, b, 1_000_000, 350)
	accounts.byRestaurant[b.RestaurantID] = testVenueSplitAccount
	u.cfg.PlatformSplitAccountRef = ""

	_, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "key-split-4", ReturnURL: "https://app/return",
	})
	if err == nil {
		t.Fatalf("CreateForBooking() = nil, want a refusal")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeSplitAccountMissing {
		t.Fatalf("code = %q, want %q — err: %v", code, domain.CodeSplitAccountMissing, err)
	}
	if gw.callCount("authorize") != 0 {
		t.Fatalf("the acquirer was called %d times", gw.callCount("authorize"))
	}
}

func isUnavailable(err error) bool { return errors.Is(err, domain.ErrUnavailable) }

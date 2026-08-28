package payments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// A one-stage acquirer that can only charge whole currency units — Kaspi Pay.
// The adapter itself is tested in infrastructure/payment/kaspi; here it is
// only its SHAPE that matters to the checkout.
// ---------------------------------------------------------------------------

type wholeUnitGateway struct {
	*fakeGateway
	unit int64
}

func (g wholeUnitGateway) MinChargeableUnitMinor() int64 { return g.unit }

func newWholeUnitCreateHarness(t *testing.T, b *domain.Booking, deposit int64, feeBps int) (*createUseCase, *fakeGateway) {
	t.Helper()
	repo := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{
		PaymentsEnabled:    boolPtr(true),
		DepositRequired:    boolPtr(true),
		DepositAmountMinor: int64Ptr(deposit),
		ServiceFeeBps:      intPtr(feeBps),
	}
	gw := newFakeGateway(domain.ProviderKaspi)
	resolver := &fakeGatewayResolver{byProvider: map[domain.PaymentProvider]domain.PaymentGateway{
		domain.ProviderKaspi: wholeUnitGateway{fakeGateway: gw, unit: 100},
	}}
	tx := &fakeTx{payments: repo, outbox: outbox}
	u := NewCreateUseCase(repo, outbox, newFakeBookingReader(b), newFakeItemReader(), settings,
		newFakeSpecialDays(), resolver, newFakeManagerChecker(), tx, Config{}).(*createUseCase)
	return u, gw
}

func TestCreateRoundsTheTotalUpToWhatTheAcquirerCanActuallyCharge(t *testing.T) {
	b := testBooking(uuid.New())
	// 2 500 ₸ deposit at a 1.5% acquirer rate grosses up to 253 807 tiyn —
	// 2 538.07 ₸, which Kaspi cannot charge.
	u, gw := newWholeUnitCreateHarness(t, b, 250_000, 150)

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.AmountMinor%100 != 0 {
		t.Fatalf("total = %d tiyn, not a whole tenge — this acquirer cannot charge it", p.AmountMinor)
	}
	if p.AmountMinor != 253_900 {
		t.Fatalf("total = %d, want 253900 (253807 rounded UP to the next tenge)", p.AmountMinor)
	}
	// The rounding is absorbed by OUR fee, never by what the venue is owed:
	// a guest shown 2 500 ₸ of food must not be charged for 2 501 ₸ of food.
	if p.BaseAmountMinor != 250_000 {
		t.Fatalf("base = %d, want it untouched at 250000", p.BaseAmountMinor)
	}
	if p.FeeMinor != 3_900 {
		t.Fatalf("fee = %d, want 3900", p.FeeMinor)
	}
	if p.BaseAmountMinor+p.FeeMinor != p.AmountMinor {
		t.Fatalf("base+fee = %d != amount %d (chk_payments_amount_split would reject this row)",
			p.BaseAmountMinor+p.FeeMinor, p.AmountMinor)
	}
	// And the acquirer is asked for exactly the number we stored.
	if gw.lastAuthorize.Amount.AmountMinor != p.AmountMinor {
		t.Fatalf("charged %d, stored %d — the ledger would never match the bank",
			gw.lastAuthorize.Amount.AmountMinor, p.AmountMinor)
	}
}

func TestCreateLeavesAnAlreadyWholeAmountAlone(t *testing.T) {
	b := testBooking(uuid.New())
	u, _ := newWholeUnitCreateHarness(t, b, 100_000, 0) // no fee, already whole

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.AmountMinor != 100_000 || p.FeeMinor != 0 {
		t.Fatalf("amount/fee = %d/%d, want 100000/0 untouched", p.AmountMinor, p.FeeMinor)
	}
}

func TestCreateTakesTheLinkExpiryFromTheAcquirer(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw := newWholeUnitCreateHarness(t, b, 100_000, 0)

	// A Kaspi link lives minutes; our own HoldTTL default is hours.
	expiry := time.Now().Add(3 * time.Minute).Truncate(time.Second)
	gw.authorizeResp = &domain.GatewayPayment{
		ProviderPaymentID: "991", Status: domain.PaymentCreated,
		PaymentURL: "https://pay.kaspi.kz/pay/abc", ExpiresAt: &expiry,
	}

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.ExpiresAt == nil || !p.ExpiresAt.Equal(expiry) {
		t.Fatalf("expires_at = %v, want the acquirer's own %v — the app shows this countdown", p.ExpiresAt, expiry)
	}
}

func TestCreateIgnoresAnExpiryThatHasAlreadyPassed(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw := newWholeUnitCreateHarness(t, b, 100_000, 0)

	past := time.Now().Add(-time.Minute)
	gw.authorizeResp = &domain.GatewayPayment{
		ProviderPaymentID: "991", Status: domain.PaymentCreated, ExpiresAt: &past,
	}

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.ExpiresAt == nil || !p.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at = %v — a clock skew must not store a payment that is born expired", p.ExpiresAt)
	}
}

func TestCreatePassesTheVenuesAcquirerAccountToTheAdapter(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw := newWholeUnitCreateHarness(t, b, 100_000, 0)
	accounts := newFakeSplitAccounts()
	accounts.byRestaurant[b.RestaurantID] = "7"
	WithSplitAccounts(accounts)(u)

	if _, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"}); err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if gw.lastAuthorize.MerchantAccountRef != "7" {
		t.Fatalf("merchant account ref = %q, want the venue's Kaspi company 7", gw.lastAuthorize.MerchantAccountRef)
	}
}

func TestCreateWithoutAMappingSendsAnEmptyRefForTheAdapterToRefuse(t *testing.T) {
	b := testBooking(uuid.New())
	u, gw := newWholeUnitCreateHarness(t, b, 100_000, 0)
	WithSplitAccounts(newFakeSplitAccounts())(u)

	if _, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"}); err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	// An acquirer that settles onto one platform account needs no per-venue
	// address at all, so a missing mapping is not an error HERE; the adapters
	// that do need one refuse it themselves (kaspi.validateAuthorize).
	if gw.lastAuthorize.MerchantAccountRef != "" {
		t.Fatalf("merchant account ref = %q, want empty", gw.lastAuthorize.MerchantAccountRef)
	}
}

// ---------------------------------------------------------------------------
// The webhook side: a paid LINK, and the three ugly cases around it.
// ---------------------------------------------------------------------------

func kaspiPreorder(bookingID uuid.UUID, providerPaymentID string) *domain.Payment {
	p := testPayment(bookingID, domain.PaymentCreated, providerPaymentID)
	p.Provider = domain.ProviderKaspi
	p.Purpose = domain.PurposePreorder
	p.AmountMinor, p.BaseAmountMinor, p.FeeMinor = 253_900, 250_000, 3_900
	return p
}

func newKaspiWebhookHarness(payments ...*domain.Payment) (*webhookUseCase, *fakePaymentRepo, *fakeLedgerRepo, *fakeGateway) {
	repo := newFakePaymentRepo(payments...)
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderKaspi)
	resolver := newFakeGatewayResolver(gw)
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox}
	u := NewWebhookUseCase(repo, newFakeEventRepo(), ledger, outbox, resolver, tx).(*webhookUseCase)
	return u, repo, ledger, gw
}

// kaspiSuccess is the event the kaspi adapter produces for a paid link. The
// event id is the Kaspi SERVICE's own delivery idempotency key, verbatim —
// derived in kaspi.VerifyWebhook and proved by that package's own test; it is
// spelled out here because it is exactly what makes a redelivery a no-op.
func kaspiSuccess(providerPaymentID string, amountMinor int64) *domain.WebhookEvent {
	return &domain.WebhookEvent{
		Provider:          domain.ProviderKaspi,
		ProviderEventID:   "qr:" + providerPaymentID + ":payment.success",
		ProviderPaymentID: providerPaymentID,
		Type:              domain.WebhookPaymentAuthorized,
		Status:            domain.PaymentAuthorized,
		Amount:            domain.Money{AmountMinor: amountMinor, Currency: domain.CurrencyKZT},
		SignatureValid:    true,
	}
}

func TestKaspiRedeliveredSuccessPaysTheVenueOnce(t *testing.T) {
	b := uuid.New()
	p := kaspiPreorder(b, "991")
	u, repo, ledger, gw := newKaspiWebhookHarness(p)
	gw.verifyFn = verifyOK(kaspiSuccess("991", p.AmountMinor))
	ctx := context.Background()

	// The Kaspi service retries a delivery until it gets a 2xx: five attempts
	// of the same payment.success are ordinary, not an attack.
	for i := 0; i < 5; i++ {
		if err := u.HandleWebhook(ctx, domain.ProviderKaspi, []byte("body"), nil); err != nil {
			t.Fatalf("delivery %d: HandleWebhook() error = %v", i+1, err)
		}
	}

	stored, _ := repo.GetByID(ctx, p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured (a pre-order is taken at payment time)", stored.Status)
	}
	if gw.callCount("capture") != 1 {
		t.Fatalf("capture called %d times, want exactly 1", gw.callCount("capture"))
	}
	entries, err := ledger.ListByPaymentID(ctx, p.ID)
	if err != nil {
		t.Fatalf("ListByPaymentID() error = %v", err)
	}
	// ONE balanced capture batch: guest debited the total once, the venue
	// credited its base once, the platform its fee once. A second batch would
	// mean the venue was credited twice for one payment.
	var guestDebited, venueCredited, platformCredited int64
	for _, e := range entries {
		switch e.Account {
		case domain.AccountGuest:
			guestDebited += e.AmountMinor
		case domain.AccountRestaurant:
			venueCredited += e.AmountMinor
		case domain.AccountPlatform:
			platformCredited += e.AmountMinor
		}
	}
	if len(entries) != 3 {
		t.Fatalf("ledger entries = %d, want 3 (guest debit + venue credit + platform fee)", len(entries))
	}
	if guestDebited != p.AmountMinor || venueCredited != p.BaseAmountMinor || platformCredited != p.FeeMinor {
		t.Fatalf("ledger totals guest/venue/platform = %d/%d/%d, want %d/%d/%d — the money was booked more than once",
			guestDebited, venueCredited, platformCredited, p.AmountMinor, p.BaseAmountMinor, p.FeeMinor)
	}
}

func TestKaspiSecondPaidLinkForOneBookingIsNotCapturedTwice(t *testing.T) {
	b := uuid.New()
	first := kaspiPreorder(b, "991")
	second := kaspiPreorder(b, "992") // the guest opened the checkout twice
	u, repo, ledger, gw := newKaspiWebhookHarness(first, second)
	ctx := context.Background()

	gw.verifyFn = verifyOK(kaspiSuccess("991", first.AmountMinor))
	if err := u.HandleWebhook(ctx, domain.ProviderKaspi, []byte("body-1"), nil); err != nil {
		t.Fatalf("first payment: HandleWebhook() error = %v", err)
	}

	gw.verifyFn = verifyOK(kaspiSuccess("992", second.AmountMinor))
	// The second callback is authentic and the money IS at Kaspi — the
	// booking-level uniqueness is what stops it becoming a second capture.
	_ = u.HandleWebhook(ctx, domain.ProviderKaspi, []byte("body-2"), nil)

	storedFirst, _ := repo.GetByID(ctx, first.ID)
	storedSecond, _ := repo.GetByID(ctx, second.ID)
	if storedFirst.Status != domain.PaymentCaptured {
		t.Fatalf("first payment status = %s, want captured", storedFirst.Status)
	}
	if storedSecond.Status == domain.PaymentCaptured {
		t.Fatal("the second link was captured too — the guest paid for one booking twice")
	}
	entries, _ := ledger.ListByPaymentID(ctx, second.ID)
	if len(entries) != 0 {
		t.Fatalf("second payment booked %d ledger entries, want 0", len(entries))
	}
	// The compensation must be ATTEMPTED, so the money is either released or
	// (for an acquirer with no void, like Kaspi) loudly refused rather than
	// forgotten.
	if gw.callCount("void") == 0 {
		t.Fatal("no compensation was attempted for the losing payment")
	}
}

// recordingSettler stands in for the deposit/pre-order cancellation settlement.
type recordingSettler struct {
	calls []DepositCancelInput
	err   error
}

func (r *recordingSettler) SettleDepositOnCancel(_ context.Context, _ Actor, _ uuid.UUID, in DepositCancelInput) (*domain.Payment, error) {
	r.calls = append(r.calls, in)
	return nil, r.err
}

func TestKaspiPaymentThatLandsAfterTheBookingWasCancelledIsSettled(t *testing.T) {
	cases := map[string]struct {
		status      domain.BookingStatus
		cancelledBy *domain.CancelledBy
		want        domain.RefundTrigger
	}{
		"guest cancelled":           {domain.BookingCancelled, cancelledByPtr(domain.CancelledByGuest), domain.RefundTriggerGuestCancel},
		"venue cancelled":           {domain.BookingCancelled, cancelledByPtr(domain.CancelledByRestaurant), domain.RefundTriggerVenueCancel},
		"nobody wrote down who did": {domain.BookingCancelled, nil, domain.RefundTriggerVenueCancel},
		"guest never showed up":     {domain.BookingNoShow, nil, domain.RefundTriggerNoShow},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := &domain.Booking{ID: uuid.New(), RestaurantID: uuid.New(), Status: tc.status, CancelledBy: tc.cancelledBy}
			p := kaspiPreorder(b.ID, "991")
			u, repo, _, gw := newKaspiWebhookHarness(p)
			settler := &recordingSettler{}
			WithLateCancelSettlement(newFakeBookingReader(b), settler)(u)
			gw.verifyFn = verifyOK(kaspiSuccess("991", p.AmountMinor))

			if err := u.HandleWebhook(context.Background(), domain.ProviderKaspi, []byte("body"), nil); err != nil {
				t.Fatalf("HandleWebhook() error = %v", err)
			}
			// The money is recorded truthfully first — it really was taken.
			stored, _ := repo.GetByID(context.Background(), p.ID)
			if stored.Status != domain.PaymentCaptured {
				t.Fatalf("status = %s, want captured", stored.Status)
			}
			if len(settler.calls) != 1 {
				t.Fatalf("settlement called %d times, want 1 — money for a cancelled booking must not just sit there", len(settler.calls))
			}
			if settler.calls[0].Trigger != tc.want {
				t.Fatalf("trigger = %s, want %s", settler.calls[0].Trigger, tc.want)
			}
		})
	}
}

func TestKaspiPaymentForALiveBookingIsNotSettled(t *testing.T) {
	b := &domain.Booking{ID: uuid.New(), RestaurantID: uuid.New(), Status: domain.BookingConfirmed}
	p := kaspiPreorder(b.ID, "991")
	u, _, _, gw := newKaspiWebhookHarness(p)
	settler := &recordingSettler{}
	WithLateCancelSettlement(newFakeBookingReader(b), settler)(u)
	gw.verifyFn = verifyOK(kaspiSuccess("991", p.AmountMinor))

	if err := u.HandleWebhook(context.Background(), domain.ProviderKaspi, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	if len(settler.calls) != 0 {
		t.Fatalf("settlement ran %d times for a live booking, want 0", len(settler.calls))
	}
}

func TestKaspiFailedSettlementLeavesTheCallbackForRetry(t *testing.T) {
	b := &domain.Booking{ID: uuid.New(), RestaurantID: uuid.New(), Status: domain.BookingCancelled}
	p := kaspiPreorder(b.ID, "991")
	u, _, _, gw := newKaspiWebhookHarness(p)
	settler := &recordingSettler{err: errors.New("acquirer unreachable")}
	WithLateCancelSettlement(newFakeBookingReader(b), settler)(u)
	gw.verifyFn = verifyOK(kaspiSuccess("991", p.AmountMinor))

	// The error must PROPAGATE: an acknowledged callback would leave money
	// taken for a cancelled booking with nobody looking at it.
	if err := u.HandleWebhook(context.Background(), domain.ProviderKaspi, []byte("body"), nil); err == nil {
		t.Fatal("HandleWebhook() = nil, want the settlement failure surfaced")
	}
}

func TestKaspiExpiredLinkFailsThePaymentAndSettlesNothing(t *testing.T) {
	b := &domain.Booking{ID: uuid.New(), RestaurantID: uuid.New(), Status: domain.BookingCancelled}
	p := kaspiPreorder(b.ID, "991")
	u, repo, _, gw := newKaspiWebhookHarness(p)
	settler := &recordingSettler{}
	WithLateCancelSettlement(newFakeBookingReader(b), settler)(u)
	gw.verifyFn = verifyOK(&domain.WebhookEvent{
		Provider: domain.ProviderKaspi, ProviderEventID: "qr:991:payment.expired", ProviderPaymentID: "991",
		Type: domain.WebhookPaymentExpired, Status: domain.PaymentExpired, SignatureValid: true,
	})

	if err := u.HandleWebhook(context.Background(), domain.ProviderKaspi, []byte("body"), nil); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
	stored, _ := repo.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentExpired {
		t.Fatalf("status = %s, want expired — the guest never paid the link", stored.Status)
	}
	if len(settler.calls) != 0 {
		t.Fatalf("settlement ran %d times, want 0 — there is no money to settle", len(settler.calls))
	}
}

func cancelledByPtr(c domain.CancelledBy) *domain.CancelledBy { return &c }

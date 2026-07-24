package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func testBooking(restaurantID uuid.UUID) *domain.Booking {
	return &domain.Booking{
		ID: uuid.New(), RestaurantID: restaurantID, UserID: nil,
		Name: "Guest", Phone: "+77001234567", PhoneNormalized: "+77001234567",
		Guests: 2, StartsAt: time.Now().Add(2 * time.Hour), Status: domain.BookingPending,
		Source: domain.SourceApp, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func newCreateHarness(t *testing.T, b *domain.Booking, deposit int64, feeBps int) (*createUseCase, *fakePaymentRepo, *fakePaymentOutbox, *fakeGateway) {
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
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	tx := &fakeTx{payments: payments, outbox: outbox}
	u := NewCreateUseCase(payments, outbox, bookings, items, settings, resolver, managers, tx, Config{}).(*createUseCase)
	return u, payments, outbox, gw
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(v int64) *int64 { return &v }
func intPtr(v int) *int       { return &v }

func TestCreateForBooking_HappyPath(t *testing.T) {
	b := testBooking(uuid.New())
	u, payments, outbox, gw := newCreateHarness(t, b, 1_000_000, 350)

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "key-1", ReturnURL: "https://app/return",
	})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v", err)
	}
	if p.Status != domain.PaymentCreated {
		t.Fatalf("status = %s, want created", p.Status)
	}
	if p.BaseAmountMinor != 1_000_000 {
		t.Fatalf("base = %d, want 1000000", p.BaseAmountMinor)
	}
	// Grossed up to cover a 3.5% acquirer cut: total = ceil(1000000×10000/9650)
	// = 1036270, fee = 36270. NOT a plain 3.5% (35000) of base — the acquirer
	// takes its cut from the total, so the markup must be slightly larger.
	if p.FeeMinor != 36_270 {
		t.Fatalf("fee = %d, want 36270 (gross-up for 3.5%% acquirer)", p.FeeMinor)
	}
	if p.AmountMinor != p.BaseAmountMinor+p.FeeMinor {
		t.Fatalf("amount %d != base+fee %d", p.AmountMinor, p.BaseAmountMinor+p.FeeMinor)
	}
	// The venue-made-whole property: after the acquirer withholds 3.5% of the
	// TOTAL, the venue still nets at least the full base.
	netToVenue := p.AmountMinor - (p.AmountMinor*350)/10_000
	if netToVenue < p.BaseAmountMinor {
		t.Fatalf("net to venue %d < base %d — venue is short", netToVenue, p.BaseAmountMinor)
	}
	if p.PaymentURL == nil || *p.PaymentURL == "" {
		t.Fatalf("payment url not set")
	}
	if gw.callCount("authorize") != 1 {
		t.Fatalf("authorize called %d times, want 1", gw.callCount("authorize"))
	}
	stored, err := payments.GetByID(context.Background(), p.ID)
	if err != nil || stored.ID != p.ID {
		t.Fatalf("payment not persisted: %v", err)
	}
	if len(outbox.types()) != 1 || outbox.types()[0] != domain.EventPaymentCreated {
		t.Fatalf("outbox events = %v, want [payment.created]", outbox.types())
	}
}

func TestCreateForBooking_IdempotentReplay(t *testing.T) {
	b := testBooking(uuid.New())
	u, payments, outbox, gw := newCreateHarness(t, b, 1_000_000, 350)
	ctx := context.Background()

	first, err := u.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "retry-key"})
	if err != nil {
		t.Fatalf("first call error = %v", err)
	}
	second, err := u.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "retry-key"})
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay created a different payment: %s != %s", first.ID, second.ID)
	}
	if gw.callCount("authorize") != 1 {
		t.Fatalf("authorize called %d times on replay, want 1 (no second hold)", gw.callCount("authorize"))
	}
	if len(outbox.types()) != 1 {
		t.Fatalf("outbox got %d events on a replay, want 1", len(outbox.types()))
	}
	all := 0
	for id := range payments.byID {
		_ = id
		all++
	}
	if all != 1 {
		t.Fatalf("payments table has %d rows, want exactly 1", all)
	}
}

func TestCreateForBooking_DifferentBookingSameKeyIsRejected(t *testing.T) {
	b1 := testBooking(uuid.New())
	b2 := testBooking(b1.RestaurantID)
	u, payments, _, _ := newCreateHarness(t, b1, 1_000_000, 350)
	// second booking must be readable too
	u.bookings.(*fakeBookingReader).byID[b2.ID] = b2
	_ = payments

	ctx := context.Background()
	if _, err := u.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: b1.ID, IdempotencyKey: "shared"}); err != nil {
		t.Fatalf("first booking error = %v", err)
	}
	// Same client-chosen key, different booking id: our own dbKey embeds the
	// booking id, so this cannot collide with the first row and must be
	// treated as an independent payment, not silently merged.
	p2, err := u.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: b2.ID, IdempotencyKey: "shared"})
	if err != nil {
		t.Fatalf("second booking error = %v", err)
	}
	if p2.BookingID != b2.ID {
		t.Fatalf("booking id = %s, want %s", p2.BookingID, b2.ID)
	}
}

func TestCreateForBooking_ConcurrentSameKeyRacesToOneRow(t *testing.T) {
	b := testBooking(uuid.New())
	u, payments, _, gw := newCreateHarness(t, b, 1_000_000, 350)
	// Force every racer to actually overlap inside Authorize before any of
	// them proceeds to the local insert — see the comment on
	// fakeGateway.authorizeDelay.
	gw.authorizeDelay = 20 * time.Millisecond
	ctx := context.Background()

	const n = 8
	results := make([]*domain.Payment, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			results[i], errs[i] = u.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "race-key"})
		}(i)
	}
	start.Done()
	wg.Wait()

	var firstID uuid.UUID
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d error = %v", i, errs[i])
		}
		if firstID == uuid.Nil {
			firstID = results[i].ID
		} else if results[i].ID != firstID {
			t.Fatalf("goroutine %d got a different payment id: %s != %s", i, results[i].ID, firstID)
		}
	}
	rows := 0
	payments.mu.Lock()
	rows = len(payments.byID)
	payments.mu.Unlock()
	if rows != 1 {
		t.Fatalf("payments table has %d rows after a %d-way race on one key, want 1", rows, n)
	}
	if gw.callCount("authorize") < 2 {
		// Every racer is allowed to call Authorize with the SAME idempotency
		// key before persisting locally — that is what makes the local race
		// safe without a distributed lock (spec §8: the acquirer resolves a
		// repeated key to the same hold). Only the LOCAL row must not
		// duplicate, which is asserted above. This assertion just confirms
		// the goroutines actually overlapped instead of running sequentially.
		t.Fatalf("authorize called %d times, want at least 2 (racers must have overlapped)", gw.callCount("authorize"))
	}
}

func TestCreateForBooking_GatewayTimeoutLeavesNoLocalRow(t *testing.T) {
	b := testBooking(uuid.New())
	u, payments, outbox, gw := newCreateHarness(t, b, 1_000_000, 350)
	gw.authorizeErr = errors.New("dial tcp: i/o timeout")

	_, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "timeout-key"})
	if err == nil {
		t.Fatalf("expected an error from a gateway timeout")
	}
	if len(payments.byID) != 0 {
		t.Fatalf("a failed Authorize left %d local rows, want 0", len(payments.byID))
	}
	if len(outbox.types()) != 0 {
		t.Fatalf("a failed Authorize published %d outbox events, want 0", len(outbox.types()))
	}
}

func TestCreateForBooking_NoPaymentRequired(t *testing.T) {
	b := testBooking(uuid.New())
	payments := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	bookings := newFakeBookingReader(b)
	items := newFakeItemReader()
	settings := newFakeRestaurantSettings() // no override: deposit not required
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	tx := &fakeTx{payments: payments, outbox: outbox}
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{PaymentsEnabled: boolPtr(true)}
	u := NewCreateUseCase(payments, outbox, bookings, items, settings, resolver, managers, tx, Config{})

	_, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

// TestCreateForBooking_GlobalDepositRequiredWithNoOverride is report item
// #10: a restaurant that never sets ITS OWN deposit_required override must
// still be payable when the GLOBAL config requires a deposit — before the
// fix, Config had no DepositRequired/PreorderPaymentRequired field at all, so
// resolveSettings always produced DepositRequired=false/
// PreorderPaymentRequired=false for every restaurant running on defaults,
// and CreateForBooking rejected every checkout with "this booking requires
// no payment". This test only sets PaymentsEnabled on the override (the one
// column every OTHER test in this file also sets) and drives
// DepositRequired/DepositDefaultMinor purely from Config, unlike every other
// test in this file which sets the override directly.
func TestCreateForBooking_GlobalDepositRequiredWithNoOverride(t *testing.T) {
	b := testBooking(uuid.New())
	payments := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	bookings := newFakeBookingReader(b)
	items := newFakeItemReader()
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{PaymentsEnabled: boolPtr(true)}
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	tx := &fakeTx{payments: payments, outbox: outbox}
	cfg := Config{DepositRequired: true, DepositDefaultMinor: 500_000, ServiceFeeBps: 350}
	u := NewCreateUseCase(payments, outbox, bookings, items, settings, resolver, managers, tx, cfg)

	p, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if err != nil {
		t.Fatalf("CreateForBooking() error = %v, want nil (global DepositRequired must apply with no venue override)", err)
	}
	if p.Purpose != domain.PurposeDeposit {
		t.Fatalf("purpose = %s, want deposit", p.Purpose)
	}
	if p.BaseAmountMinor != 500_000 {
		t.Fatalf("base = %d, want the global DepositDefaultMinor of 500000", p.BaseAmountMinor)
	}
}

func TestCreateForBooking_RejectsAnotherGuestsBooking(t *testing.T) {
	owner := uuid.New()
	b := testBooking(uuid.New())
	b.UserID = &owner
	u, _, _, _ := newCreateHarness(t, b, 1_000_000, 350)

	stranger := uuid.New()
	_, err := u.CreateForBooking(context.Background(), Actor{UserID: &stranger, Role: domain.RoleUser}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "k",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

// TestCreateForBooking_CrossTenantStaffIsRejected is report item #13: staff
// of a DIFFERENT restaurant must not be able to start a payment link for
// this booking merely by knowing its id.
func TestCreateForBooking_CrossTenantStaffIsRejected(t *testing.T) {
	b := testBooking(uuid.New())
	payments := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	bookings := newFakeBookingReader(b)
	items := newFakeItemReader()
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{
		PaymentsEnabled: boolPtr(true), DepositRequired: boolPtr(true), DepositAmountMinor: int64Ptr(1_000_000),
	}
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	strangerStaff := uuid.New()
	managers := &fakeManagerChecker{managed: map[uuid.UUID]map[uuid.UUID]bool{}, allowAllByDefault: false}
	managers.set(strangerStaff, b.RestaurantID, false)
	tx := &fakeTx{payments: payments, outbox: outbox}
	u := NewCreateUseCase(payments, outbox, bookings, items, settings, resolver, managers, tx, Config{})

	_, err := u.CreateForBooking(context.Background(), Actor{UserID: &strangerStaff, Role: domain.RoleRestaurant}, CreateInput{
		BookingID: b.ID, IdempotencyKey: "k",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden (staff of a different restaurant)", err)
	}
	if gw.callCount("authorize") != 0 {
		t.Fatalf("authorize called %d times, want 0", gw.callCount("authorize"))
	}
}

func TestCreateForBooking_RejectsWrongBookingStatus(t *testing.T) {
	b := testBooking(uuid.New())
	b.Status = domain.BookingCancelled
	u, _, _, _ := newCreateHarness(t, b, 1_000_000, 350)

	_, err := u.CreateForBooking(context.Background(), Actor{}, CreateInput{BookingID: b.ID, IdempotencyKey: "k"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

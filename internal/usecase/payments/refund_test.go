package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// refundHarness bundles everything a Settle() test needs, including the
// booking + deadline-policy fakes that back the server-derived cancellation
// timing (report item #15) — CancelledAt/CancelDeadline are no longer
// SettleInput fields, so tests drive them through booking.CancelledAt and
// deadlineResolver.deadline instead of passing them directly to Settle.
type refundHarness struct {
	u        RefundUseCase
	payments *fakePaymentRepo
	refunds  *fakeRefundRepo
	ledger   *fakeLedgerRepo
	outbox   *fakePaymentOutbox
	gw       *fakeGateway
	bookings *fakeBookingReader
	deadline *fakeCancelDeadlineResolver
}

func newRefundHarness(p *domain.Payment, acquiringBps int) *refundHarness {
	repo := newFakePaymentRepo(p)
	refunds := newFakeRefundRepo()
	ledger := newFakeLedgerRepo()
	outbox := newFakePaymentOutbox()
	gw := newFakeGateway(domain.ProviderFreedomPay)
	resolver := newFakeGatewayResolver(gw)
	managers := newFakeManagerChecker()
	booking := &domain.Booking{ID: p.BookingID, RestaurantID: p.RestaurantID, StartsAt: time.Now().Add(2 * time.Hour)}
	bookings := newFakeBookingReader(booking)
	deadlineResolver := &fakeCancelDeadlineResolver{deadline: time.Now().Add(time.Hour)}
	tx := &fakeTx{payments: repo, ledger: ledger, outbox: outbox, refunds: refunds}
	u := NewRefundUseCase(repo, refunds, ledger, outbox, resolver, managers, bookings, deadlineResolver, tx, Config{RefundAcquiringBps: acquiringBps})
	return &refundHarness{u: u, payments: repo, refunds: refunds, ledger: ledger, outbox: outbox, gw: gw, bookings: bookings, deadline: deadlineResolver}
}

// setCancelledAt sets the booking's own CancelledAt — the server-trusted
// source of truth for a guest-cancel settlement (report item #15).
func (h *refundHarness) setCancelledAt(bookingID uuid.UUID, at time.Time) {
	h.bookings.byID[bookingID].CancelledAt = &at
}

func capturedTestPayment(bookingID uuid.UUID) *domain.Payment {
	return &domain.Payment{
		ID: uuid.New(), BookingID: bookingID, RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_000_000, BaseAmountMinor: 966_000, FeeMinor: 34_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// TestSettle_TableFromSpec walks the exact §9.1 settlement table (variant A)
// through the usecase, including the ledger write, and checks the payment's
// terminal status.
func TestSettle_TableFromSpec(t *testing.T) {
	cases := []struct {
		name           string
		trigger        domain.RefundTrigger
		cancelledAt    *time.Time // nil = no recorded cancellation (no_show / venue_cancel do not need one)
		actor          Actor
		wantStatus     domain.PaymentStatus
		wantRefund     bool
		wantRestaurant int64
		wantPlatform   int64
	}{
		{
			name:        "guest cancels before deadline: refunded minus acquiring",
			trigger:     domain.RefundTriggerGuestCancel,
			cancelledAt: timePtr(time.Now().Add(-time.Minute)),
			actor:       Actor{}, wantStatus: domain.PaymentRefunded, wantRefund: true,
		},
		{
			name:        "guest cancels after deadline: settled as no-show, no ledger movement",
			trigger:     domain.RefundTriggerGuestCancel,
			cancelledAt: timePtr(time.Now().Add(2 * time.Hour)),
			actor:       staffActor, wantStatus: domain.PaymentCaptured, wantRefund: false,
			wantRestaurant: 966_000, wantPlatform: 34_000,
		},
		{
			name:        "no-show: base to venue, fee to platform, no ledger movement",
			trigger:     domain.RefundTriggerNoShow,
			cancelledAt: nil,
			actor:       staffActor, wantStatus: domain.PaymentCaptured, wantRefund: false,
			wantRestaurant: 966_000, wantPlatform: 34_000,
		},
		{
			name:        "venue cancels: full refund including the service fee",
			trigger:     domain.RefundTriggerVenueCancel,
			cancelledAt: nil,
			actor:       staffActor, wantStatus: domain.PaymentRefunded, wantRefund: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := capturedTestPayment(uuid.New())
			h := newRefundHarness(p, 100) // 1%
			if tc.cancelledAt != nil {
				h.setCancelledAt(p.BookingID, *tc.cancelledAt)
			}

			got, err := h.u.Settle(context.Background(), tc.actor, p.BookingID, SettleInput{
				Trigger: tc.trigger, IdempotencyKey: "settle-1",
			})
			if err != nil {
				t.Fatalf("Settle() error = %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			stored, _ := h.payments.GetByID(context.Background(), p.ID)
			if stored.Status != tc.wantStatus {
				t.Fatalf("persisted status = %s, want %s", stored.Status, tc.wantStatus)
			}
			if stored.SettledAt == nil {
				t.Fatalf("SettledAt not set after Settle()")
			}

			entries, _ := h.ledger.ListByPaymentID(context.Background(), p.ID)
			if err := domain.ValidateLedgerBalance(entries); len(entries) > 0 && err != nil {
				t.Fatalf("ledger does not balance: %v", err)
			}

			if tc.wantRefund {
				if h.gw.callCount("refund") != 1 {
					t.Fatalf("refund called %d times, want 1", h.gw.callCount("refund"))
				}
				if len(entries) == 0 {
					t.Fatalf("expected ledger entries for a refund, got none")
				}
			} else {
				if h.gw.callCount("refund") != 0 {
					t.Fatalf("refund called %d times, want 0 (no money moves)", h.gw.callCount("refund"))
				}
				if len(entries) != 0 {
					t.Fatalf("expected NO ledger entries (nothing changes from capture-time booking), got %d: %+v", len(entries), entries)
				}
			}
			foundSettledOrRefunded := false
			for _, ty := range h.outbox.types() {
				if ty == domain.EventPaymentSettled || ty == domain.EventPaymentRefunded {
					foundSettledOrRefunded = true
				}
			}
			if !foundSettledOrRefunded {
				t.Fatalf("outbox events = %v, want a settled/refunded event", h.outbox.types())
			}
		})
	}
}

func TestSettle_PartialRefundAmountMatchesPolicy(t *testing.T) {
	p := &domain.Payment{
		ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		Provider: domain.ProviderFreedomPay, ProviderPaymentID: strPtrTest("gw-1"),
		Purpose: domain.PurposeDeposit, Status: domain.PaymentCaptured,
		AmountMinor: 1_035_000, BaseAmountMinor: 1_000_000, FeeMinor: 35_000,
		Currency: domain.CurrencyKZT, IdempotencyKey: "k",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	h := newRefundHarness(p, 100) // 1%
	h.setCancelledAt(p.BookingID, time.Now())

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	if h.gw.refundN != 1 {
		t.Fatalf("refund called %d times, want 1", h.gw.refundN)
	}
	bal, _ := h.ledger.BalanceByAccount(context.Background(), p.ID)
	// 1% of 1,035,000 = 10,350 withheld; guest gets 1,024,650 back.
	if got := -bal[domain.AccountGuest]; got != 1_024_650 {
		t.Fatalf("guest net = %d, want -1024650 (a credit of 1,024,650)", got)
	}
	if got := bal[domain.AccountAcquirer]; got != -10_350 {
		t.Fatalf("acquirer net = %d, want a credit of 10,350", got)
	}
}

func TestSettle_IdempotentReplayNoSecondRefund(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	ctx := context.Background()
	in := SettleInput{Trigger: domain.RefundTriggerVenueCancel, IdempotencyKey: "retry-1"}

	first, err := h.u.Settle(ctx, staffActor, p.BookingID, in)
	if err != nil {
		t.Fatalf("first Settle() error = %v", err)
	}
	if first.Status != domain.PaymentRefunded {
		t.Fatalf("first Settle() status = %s, want refunded", first.Status)
	}
	// The payment left the "live" (authorized/captured) set after the first
	// call succeeded, so GetLiveByBookingID correctly reports ErrNotFound on
	// a second call — there is nothing left to settle twice. This IS the
	// idempotency guarantee for the sequential-retry case (spec §8): a caller
	// that retries after seeing the first call succeed can tell "already
	// done" apart from "still in progress" by this exact error.
	_, err = h.u.Settle(ctx, staffActor, p.BookingID, in)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second Settle() error = %v, want ErrNotFound (payment no longer live)", err)
	}
	if h.gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times across 2 Settle() calls, want 1", h.gw.callCount("refund"))
	}
}

func TestSettle_ProviderRefundFailureRecordsAttempt(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	h.gw.refundErr = errors.New("acquirer: card declined") // a generic error, treated as an UNKNOWN outcome (report item #1), never a definite failure

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if err == nil {
		t.Fatalf("expected an error from a provider refund failure")
	}
	stored, _ := h.payments.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want unchanged captured", stored.Status)
	}
	if len(h.ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries from a failed refund, want 0", len(h.ledger.entries))
	}
	if len(h.outbox.types()) != 0 {
		t.Fatalf("outbox got %d events from a failed refund, want 0", len(h.outbox.types()))
	}
	found := false
	for _, r := range h.refunds.byID {
		if r.Status == domain.RefundPending {
			found = true
		}
	}
	if !found {
		t.Fatalf("no pending refund attempt recorded")
	}
}

// TestSettle_TimeoutThenRetryDoesNotCallGatewayTwice is report item #1's core
// test: a refund attempt that times out (an uncertain outcome, NOT a
// definite failure) must leave the refund row `pending`, and a retry with the
// SAME idempotency key must be refused loudly instead of calling the
// acquirer's Refund a second time.
func TestSettle_TimeoutThenRetryDoesNotCallGatewayTwice(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	h.gw.refundErr = context.DeadlineExceeded

	in := SettleInput{Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "timeout-1"}
	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, in)
	if err == nil {
		t.Fatalf("expected an error from a timed-out refund")
	}
	if h.gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times on the first (timed out) attempt, want 1", h.gw.callCount("refund"))
	}
	var recorded *domain.PaymentRefund
	for _, r := range h.refunds.byID {
		recorded = r
	}
	if recorded == nil || recorded.Status != domain.RefundPending {
		t.Fatalf("refund row = %+v, want status pending after a timeout", recorded)
	}

	// Retry with the SAME key: must be refused, and must NOT call the
	// acquirer again.
	_, err = h.u.Settle(context.Background(), Actor{}, p.BookingID, in)
	if err == nil {
		t.Fatalf("expected the retry to be refused while the refund is pending")
	}
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("retry error = %v, want to wrap domain.ErrProviderOutcomeUnknown", err)
	}
	if h.gw.callCount("refund") != 1 {
		t.Fatalf("refund called %d times after the retry, want still 1 (no second acquirer call on an uncertain outcome)", h.gw.callCount("refund"))
	}
}

// TestSettle_ProviderExplicitDeclineIsTerminal is the counterpart to
// TestSettle_TimeoutThenRetryDoesNotCallGatewayTwice: an explicit,
// well-formed decline (domain.ErrProviderDeclined) is a DEFINITE outcome, not
// an unknown one — it is recorded as RefundFailed, not RefundPending, and the
// error clearly says the refund was declined rather than pointing at
// reconciliation.
func TestSettle_ProviderExplicitDeclineIsTerminal(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	h.gw.refundErr = fmt.Errorf("acquirer: refund window expired: %w", domain.ErrProviderDeclined)

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("error = %v, want to wrap domain.ErrProviderDeclined", err)
	}
	var recorded *domain.PaymentRefund
	for _, r := range h.refunds.byID {
		recorded = r
	}
	if recorded == nil || recorded.Status != domain.RefundFailed {
		t.Fatalf("refund row = %+v, want status failed (a definite decline)", recorded)
	}
}

// TestSettle_GatewayReportsProcessingIsNotCredited is report item #12: the
// gateway can answer with no transport error but a status other than
// "succeeded" (e.g. still processing). That must not be credited as a
// successful refund.
func TestSettle_GatewayReportsProcessingIsNotCredited(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	h.gw.refundResp = &domain.GatewayRefund{ProviderRefundID: "rf-1", Status: domain.RefundPending}

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if err == nil {
		t.Fatalf("expected an error: the gateway did not confirm success")
	}
	stored, _ := h.payments.GetByID(context.Background(), p.ID)
	if stored.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want unchanged captured (nothing was credited)", stored.Status)
	}
	if len(h.ledger.entries) != 0 {
		t.Fatalf("ledger got %d entries although the gateway never confirmed success", len(h.ledger.entries))
	}
	var recorded *domain.PaymentRefund
	for _, r := range h.refunds.byID {
		recorded = r
	}
	if recorded == nil || recorded.Status != domain.RefundPending {
		t.Fatalf("refund row = %+v, want pending (gateway answered but not with Succeeded)", recorded)
	}
}

func TestSettle_GuestCannotTriggerNoShowOnThemselves(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)

	_, err := h.u.Settle(context.Background(), Actor{Role: domain.RoleUser}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerNoShow, IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

// TestSettle_NoShowThenGuestCancelIsRejected is report item #7's core test: a
// no-show settlement (which legitimately leaves Status == captured and moves
// no ledger money) must NOT be followed by a second, different Settle call
// that refunds the guest on top of what the venue already kept.
func TestSettle_NoShowThenGuestCancelIsRejected(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	ctx := context.Background()

	first, err := h.u.Settle(ctx, staffActor, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerNoShow, IdempotencyKey: "no-show-1",
	})
	if err != nil {
		t.Fatalf("no-show Settle() error = %v", err)
	}
	if first.Status != domain.PaymentCaptured {
		t.Fatalf("status after no-show = %s, want unchanged captured", first.Status)
	}

	// A DIFFERENT idempotency key, a DIFFERENT trigger: without the
	// SettledAt guard this would sail through (Status is still captured) and
	// refund the guest almost in full, on top of what the venue was already
	// paid.
	h.setCancelledAt(p.BookingID, time.Now())
	_, err = h.u.Settle(ctx, Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "guest-cancel-after-no-show",
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists (payment already settled)", err)
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0 — the no-show settlement must not be reopened", h.gw.callCount("refund"))
	}
}

// TestSettle_SameKeyDifferentTriggerIsRejected is report item #2 (second
// review), the exact exploit the review describes: a late cancellation
// settles first (no refund, the venue keeps the base), and a SECOND Settle
// call reuses the SAME idempotency key but with a DIFFERENT trigger
// (venue_cancel, a full refund). Checking the idempotency key ALONE used to
// treat this as "a legitimate retry, resume it" and pay the guest a full
// refund on top of what the venue already kept. The trigger must be checked
// too — a key match with a trigger mismatch is a conflict, not a resume.
func TestSettle_SameKeyDifferentTriggerIsRejected(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	ctx := context.Background()
	const key = "shared-key-1"

	first, err := h.u.Settle(ctx, staffActor, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerNoShow, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("no-show Settle() error = %v", err)
	}
	if first.Status != domain.PaymentCaptured {
		t.Fatalf("status after no-show = %s, want unchanged captured", first.Status)
	}

	_, err = h.u.Settle(ctx, staffActor, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerVenueCancel, IdempotencyKey: key,
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("error = %v, want ErrAlreadyExists (same key, different trigger is a conflict, not a resume)", err)
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0 — the no-show settlement must not be reopened into a full refund", h.gw.callCount("refund"))
	}
}

// TestSettle_NoShowRetrySameKeyIsIdempotent is the companion to the test
// above: a legitimate retry with the SAME idempotency key (a client retrying
// a timed-out HTTP call, say) must succeed as a no-op, not be rejected as a
// conflict.
func TestSettle_NoShowRetrySameKeyIsIdempotent(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	ctx := context.Background()
	in := SettleInput{Trigger: domain.RefundTriggerNoShow, IdempotencyKey: "no-show-1"}

	if _, err := h.u.Settle(ctx, staffActor, p.BookingID, in); err != nil {
		t.Fatalf("first Settle() error = %v", err)
	}
	got, err := h.u.Settle(ctx, staffActor, p.BookingID, in)
	if err != nil {
		t.Fatalf("retry Settle() with the same key error = %v, want nil (idempotent replay)", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
	// Only one payment.settled event, not two.
	n := 0
	for _, ty := range h.outbox.types() {
		if ty == domain.EventPaymentSettled {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("payment.settled published %d times, want 1", n)
	}
}

// TestSettle_ConcurrentSettleOnlyOneWins is report item #7's race case: two
// concurrent Settle calls for the SAME booking with DIFFERENT triggers/keys
// (a no-show recorded by staff racing a guest's own cancellation request)
// must not both succeed — the ClaimSettlement CAS must let exactly one
// through.
func TestSettle_ConcurrentSettleOnlyOneWins(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())
	ctx := context.Background()

	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		start.Wait()
		_, errs[0] = h.u.Settle(ctx, staffActor, p.BookingID, SettleInput{Trigger: domain.RefundTriggerNoShow, IdempotencyKey: "race-no-show"})
	}()
	go func() {
		defer wg.Done()
		start.Wait()
		_, errs[1] = h.u.Settle(ctx, Actor{}, p.BookingID, SettleInput{Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "race-guest-cancel"})
	}()
	start.Done()
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1 (only one Settle may win the claim), errs = %v", successes, errs)
	}
	if h.gw.callCount("refund") > 1 {
		t.Fatalf("refund called %d times, want at most 1", h.gw.callCount("refund"))
	}
}

// TestSettle_RefundExceedingRemainderIsRejected is report item #11: never
// refund more than what remains on a payment. This exercises the defensive,
// DB-facing guard directly — this usecase's own design (one-shot
// whole-payment settlement via ClaimSettlement) never lets a SECOND Settle
// reach it in the normal flow, see the comment in refund.go on why the check
// still exists.
func TestSettle_RefundExceedingRemainderIsRejected(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	h.setCancelledAt(p.BookingID, time.Now())

	// Simulate a remainder that is already exhausted by a previous refund —
	// SucceededTotal must already be at the payment's total, so ANY further
	// refund is rejected before the acquirer is ever called.
	h.refunds.byID[uuid.New()] = &domain.PaymentRefund{
		ID: uuid.New(), PaymentID: p.ID, AmountMinor: p.AmountMinor,
		Currency: p.Currency, Status: domain.RefundSucceeded,
		IdempotencyKey: "already-refunded", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation (refund exceeds remainder)", err)
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0 (rejected before the acquirer call)", h.gw.callCount("refund"))
	}
}

// TestSettle_ManualCancelledAtRequiresStaff is report item #15: a
// caller-supplied cancellation time must never be accepted from a bare guest
// actor.
func TestSettle_ManualCancelledAtRequiresStaff(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	at := time.Now().Add(-time.Hour)

	_, err := h.u.Settle(context.Background(), Actor{Role: domain.RoleUser}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1", ManualCancelledAt: &at,
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0", h.gw.callCount("refund"))
	}
}

// TestSettle_GuestCancelWithoutRecordedCancellationIsRejected is report item
// #15's main case: a guest-cancel settlement for a booking the booking flow
// never recorded as cancelled must be rejected — this used to be exploitable
// by anyone who knew the booking id, since CancelledAt/CancelDeadline were
// caller-supplied.
func TestSettle_GuestCancelWithoutRecordedCancellationIsRejected(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100) // booking.CancelledAt left nil

	_, err := h.u.Settle(context.Background(), Actor{}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation (no recorded cancellation)", err)
	}
	if h.gw.callCount("refund") != 0 {
		t.Fatalf("refund called %d times, want 0", h.gw.callCount("refund"))
	}
}

// TestSettle_ManualCancelledAtStaffOverride shows the escape hatch works for
// its intended user: staff recording an out-of-band cancellation.
func TestSettle_ManualCancelledAtStaffOverride(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100) // booking.CancelledAt left nil (never recorded)
	at := time.Now().Add(-time.Minute)

	got, err := h.u.Settle(context.Background(), staffActor, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerGuestCancel, IdempotencyKey: "settle-1", ManualCancelledAt: &at,
	})
	if err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	if got.Status != domain.PaymentRefunded {
		t.Fatalf("status = %s, want refunded", got.Status)
	}
}

// TestSettle_CrossTenantStaffIsRejected is report item #13: staff of a
// DIFFERENT restaurant must not be able to settle this payment merely by
// knowing its booking id.
func TestSettle_CrossTenantStaffIsRejected(t *testing.T) {
	p := capturedTestPayment(uuid.New())
	h := newRefundHarness(p, 100)
	strangerStaff := uuid.New()
	// Explicitly deny this staff user for this restaurant (fakeManagerChecker
	// defaults to "allow" unless told otherwise).
	managers := &fakeManagerChecker{managed: map[uuid.UUID]map[uuid.UUID]bool{}, allowAllByDefault: false}
	managers.set(strangerStaff, p.RestaurantID, false)
	h.u = NewRefundUseCase(h.payments, h.refunds, h.ledger, h.outbox,
		newFakeGatewayResolver(h.gw), managers, h.bookings, h.deadline,
		&fakeTx{payments: h.payments, ledger: h.ledger, outbox: h.outbox, refunds: h.refunds},
		Config{RefundAcquiringBps: 100})

	_, err := h.u.Settle(context.Background(), Actor{UserID: &strangerStaff, Role: domain.RoleRestaurant}, p.BookingID, SettleInput{
		Trigger: domain.RefundTriggerNoShow, IdempotencyKey: "settle-1",
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden (staff of a different restaurant)", err)
	}
}

func strPtrTest(s string) *string    { return &s }
func timePtr(t time.Time) *time.Time { return &t }

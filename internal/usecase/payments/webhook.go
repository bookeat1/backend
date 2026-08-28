package payments

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// WebhookUseCase applies a verified acquirer callback to the payment state
// machine (spec §7).
type WebhookUseCase interface {
	HandleWebhook(ctx context.Context, provider domain.PaymentProvider, raw []byte, headers map[string]string) error
}

type webhookUseCase struct {
	payments       domain.PaymentRepository
	events         domain.PaymentEventRepository
	ledger         domain.PaymentLedgerRepository
	outbox         domain.PaymentOutboxRepository
	gateways       gatewayResolver
	tx             domain.TxManager
	ticketObserver PaymentSubjectObserver
	// bookings / lateSettler are the OPTIONAL late-payment guard, wired
	// together by WithLateCancelSettlement. Both nil = the previous behaviour.
	bookings    bookingReader
	lateSettler DepositCancellationUseCase
}

// PaymentSubjectObserver is notified after a webhook has successfully applied a
// status change to a payment whose subject is NOT a booking (EventTicketID set).
// It exists so the ticket layer can project the payment's new status onto its
// ticket (pending→paid on capture, pending→cancelled on fail/expire/void,
// paid→refunded) WITHOUT this package ever importing usecase/tickets and
// WITHOUT forking the webhook. A booking payment (EventTicketID == nil) never
// invokes it. It is an OPTIONAL dependency (WithPaymentSubjectObserver) —
// existing callers pass none and behave exactly as before.
type PaymentSubjectObserver interface {
	// OnPaymentApplied projects p's current status onto its subject. Returning
	// an error leaves the webhook event unprocessed so it is retried (the
	// projection must not be silently lost), same contract as any other apply
	// failure.
	OnPaymentApplied(ctx context.Context, p *domain.Payment) error
}

// WebhookOption configures the webhook usecase without breaking positional
// callers — same backward-compatible variadic-option pattern as
// bookings.NewStatusUseCase's WithDepositSettler.
type WebhookOption func(*webhookUseCase)

// WithPaymentSubjectObserver wires a non-booking subject projection (tickets).
func WithPaymentSubjectObserver(obs PaymentSubjectObserver) WebhookOption {
	return func(u *webhookUseCase) { u.ticketObserver = obs }
}

// WithLateCancelSettlement closes the "the guest paid AFTER the booking was
// cancelled" hole.
//
// It is reachable for every acquirer, but a payment LINK makes it ordinary
// rather than exotic: a Kaspi link is created in status `created`, and a
// `created` payment is deliberately NOT "live" (idx_payments_live_per_booking
// — a guest may abandon a checkout), so a cancellation that happens while the
// link is still unpaid finds nothing to settle and returns a clean no-op.
// If the guest then opens the link and pays, the callback arrives against a
// booking that no longer exists as far as the venue is concerned, and without
// this the money is simply taken and nobody is told.
//
// Wiring it makes the webhook re-run the SAME settlement the cancellation
// itself would have run (SettleDepositOnCancel), with the trigger derived from
// who cancelled — so an early guest cancel or a venue cancel refunds in full,
// and a late cancel / no-show leaves the money with the venue, exactly as the
// policy says. Nothing new decides anything about money here.
func WithLateCancelSettlement(bookings bookingReader, settler DepositCancellationUseCase) WebhookOption {
	return func(u *webhookUseCase) {
		u.bookings = bookings
		u.lateSettler = settler
	}
}

// NewWebhookUseCase constructs the webhook-processing usecase.
func NewWebhookUseCase(
	payments domain.PaymentRepository,
	events domain.PaymentEventRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	tx domain.TxManager,
	opts ...WebhookOption,
) WebhookUseCase {
	u := &webhookUseCase{payments: payments, events: events, ledger: ledger, outbox: outbox, gateways: gateways, tx: tx}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// HandleWebhook is the single entry point for every provider's callback
// route. Order, and it never changes (spec §7):
//
//  1. verify the signature FIRST — an unverified body is never interpreted;
//  2. store the raw event BEFORE any interpretation, including one whose
//     signature did not verify — that row IS the idempotency guard: a
//     redelivered callback hits (provider, provider_event_id) and this method
//     returns without reprocessing;
//  3. only then resolve the local payment and apply the transition.
//
// It resolves the gateway via ForRefund (not For/Resolve): a disabled
// acquirer must still be able to tell us what happened to money it already
// touched (spec §9.1).
func (u *webhookUseCase) HandleWebhook(ctx context.Context, provider domain.PaymentProvider, raw []byte, headers map[string]string) error {
	gw, err := u.gateways.ForRefund(provider)
	if err != nil {
		return err
	}

	event, verr := gw.VerifyWebhook(raw, headers)
	if verr != nil {
		u.storeInvalidEvent(ctx, provider, raw, verr)
		logging.FromContext(ctx).Warn(logging.EventPaymentWebhookInvalid,
			slog.String("provider", string(provider)),
		)
		return verr
	}

	row := &domain.PaymentEvent{
		ID: uuid.New(), Provider: provider, ProviderEventID: event.ProviderEventID,
		ProviderPaymentID: nullableStr(event.ProviderPaymentID), EventType: &event.Type,
		Payload: event.Payload, SignatureValid: true, ReceivedAt: time.Now(),
	}
	if err := u.events.Create(ctx, row); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			// Redelivery of a callback we already stored. Report item #8:
			// this used to acknowledge unconditionally, on the assumption
			// that "already stored" means "already processed or being
			// processed right now". That is false if the FIRST insert's own
			// request crashed (or was killed) AFTER the insert committed but
			// BEFORE it reached MarkProcessed — that event would sit
			// unprocessed forever, because every future redelivery hit this
			// exact branch and returned early without ever looking at
			// processed_at. Re-read the stored row and resume processing it
			// if it never finished.
			existing, gerr := u.events.GetByProviderEventID(ctx, provider, event.ProviderEventID)
			if gerr != nil {
				return gerr
			}
			logging.FromContext(ctx).Info(logging.EventPaymentWebhookReceived,
				slog.String("provider", string(provider)),
				slog.String("provider_event_id", event.ProviderEventID),
				slog.Bool("duplicate", true),
				slog.Bool("already_processed", existing.ProcessedAt != nil),
			)
			if existing.ProcessedAt != nil {
				return nil
			}
			return u.resolveAndApply(ctx, gw, provider, existing, event)
		}
		return err
	}

	logging.FromContext(ctx).Info(logging.EventPaymentWebhookReceived,
		slog.String("provider", string(provider)),
		slog.String("provider_event_id", event.ProviderEventID),
		slog.String("event_type", string(event.Type)),
	)

	return u.resolveAndApply(ctx, gw, provider, row, event)
}

// resolveAndApply resolves the local payment for event and applies it,
// closing row (MarkProcessed) only on an outcome that is truly final — a
// successful apply, or the deliberate "no local record exists at all"
// verdict (spec §7 forbids ever creating a payment from a webhook, so that
// verdict cannot change on a later retry). Any OTHER failure to apply leaves
// row unprocessed (report item #9) so a later delivery or the reconciliation
// worker gets another chance at it, instead of it silently falling out of
// ClaimUnprocessed's scan forever.
func (u *webhookUseCase) resolveAndApply(ctx context.Context, gw domain.PaymentGateway, provider domain.PaymentProvider, row *domain.PaymentEvent, event *domain.WebhookEvent) error {
	p, err := u.resolvePayment(ctx, provider, event)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Never create a payment from a webhook (spec §7): store the
			// fact that we could not resolve it and stop. This verdict is
			// final — retrying the same lookup will not make the payment
			// exist — so it is correct (and the ONLY case besides success)
			// to MarkProcessed here. The acquirer still gets a 200.
			if merr := u.events.MarkProcessed(ctx, row.ID, time.Now(), "unknown payment: no local record for this acquirer callback"); merr != nil {
				return merr
			}
			logging.FromContext(ctx).Error("payment.webhook_unknown_payment",
				slog.String("provider", string(provider)),
				slog.String("provider_payment_id", event.ProviderPaymentID),
				slog.String("merchant_payment_id", event.MerchantPaymentID),
			)
			return nil
		}
		return err
	}

	// Report item #16 (minor): backfill payment_id now that it is known, even
	// if apply() is about to fail — idx_payment_events_payment exists so
	// reconciliation can find every event for a payment, including the ones
	// that failed to apply.
	if serr := u.events.SetPaymentID(ctx, row.ID, p.ID); serr != nil {
		logging.FromContext(ctx).Error("payment.webhook_payment_id_backfill_failed",
			slog.String("event_id", row.ID.String()), slog.String("error", serr.Error()))
	}

	if applyErr := u.apply(ctx, gw, p, event); applyErr != nil {
		// Report item #9: processed_at is NOT set here. Only the error text
		// is recorded, and the event stays in ClaimUnprocessed's scan.
		if rerr := u.events.RecordProcessingError(ctx, row.ID, applyErr.Error()); rerr != nil {
			logging.FromContext(ctx).Error("payment.webhook_error_not_recorded",
				slog.String("event_id", row.ID.String()), slog.String("error", rerr.Error()))
		}
		// Report item #14: an out-of-order delivery (e.g. `captured` arriving
		// before `authorized`, delivery order is not guaranteed) surfaces
		// here as domain.ErrInvalidStatus. Leaving it unprocessed (above) is
		// enough to not lose it, but it deserves an explicit, distinct log
		// line so an operator sees "waiting for an earlier event, not
		// broken" instead of a generic apply failure.
		if errors.Is(applyErr, domain.ErrInvalidStatus) {
			logging.FromContext(ctx).Warn("payment.webhook_out_of_order",
				slog.String("payment_id", p.ID.String()),
				slog.String("event_type", string(event.Type)),
				slog.String("error", applyErr.Error()),
			)
		}
		return applyErr
	}
	return u.events.MarkProcessed(ctx, row.ID, time.Now(), "")
}

// resolvePayment looks the callback's payment up by the acquirer's own id
// first, then by MerchantPaymentID (our id, echoed back) — needed because a
// hosted payment page only produces an acquirer-side transaction id once the
// card is charged (spec §7).
func (u *webhookUseCase) resolvePayment(ctx context.Context, provider domain.PaymentProvider, event *domain.WebhookEvent) (*domain.Payment, error) {
	if event.ProviderPaymentID != "" {
		p, err := u.payments.GetByProviderPaymentID(ctx, provider, event.ProviderPaymentID)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
	}
	if event.MerchantPaymentID != "" {
		if id, perr := uuid.Parse(event.MerchantPaymentID); perr == nil {
			p, err := u.payments.GetByID(ctx, id)
			if err != nil {
				return nil, err
			}
			// Non-blocking item #3 (second review): GetByID resolves by OUR
			// primary key alone, with no idea which acquirer sent the
			// callback. Without this check, a FreedomPay webhook whose
			// MerchantPaymentID happens to parse as a UUID that belongs to a
			// TipTopPay payment (or vice versa — a coincidence, a
			// misconfigured endpoint, or an attacker probing the callback
			// URL of the wrong provider) would be applied to a payment that
			// acquirer never touched. VerifyWebhook already proved the
			// SIGNATURE is authentic for `provider`; that says nothing about
			// which payment it is authentic FOR.
			if p.Provider != provider {
				return nil, fmt.Errorf(
					"%w: webhook from %s resolved to payment %s which belongs to %s",
					domain.ErrNotFound, provider, p.ID, p.Provider)
			}
			return p, nil
		}
	}
	return nil, domain.ErrNotFound
}

// apply routes a verified event to its state-machine handler. An event type
// this build does not recognise, or one mapped to domain.WebhookUnknown, is
// acknowledged and never read as "paid" (spec §7) — it is evidence, already
// stored, waiting for a human.
func (u *webhookUseCase) apply(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, event *domain.WebhookEvent) error {
	if err := u.applyToPayment(ctx, gw, p, event); err != nil {
		return err
	}
	// Project the payment's now-current status onto a non-booking subject (an
	// event ticket). p.Status was updated in place by the handler above. A
	// projection failure is returned so the webhook event stays unprocessed and
	// is retried — the ticket must never silently diverge from its payment.
	if u.ticketObserver != nil && p.EventTicketID != nil {
		return u.ticketObserver.OnPaymentApplied(ctx, p)
	}
	return u.settleIfBookingAlreadyCancelled(ctx, p)
}

// settleIfBookingAlreadyCancelled settles money that arrived for a booking the
// venue has already closed (see WithLateCancelSettlement).
//
// It runs only after the callback was applied successfully, so the payment's
// status is the truth before any decision is taken. An error is RETURNED,
// which leaves the webhook event unprocessed and gets it retried: money taken
// for a cancelled booking that nobody settled must stay visible, not be
// swallowed by an acknowledged callback.
func (u *webhookUseCase) settleIfBookingAlreadyCancelled(ctx context.Context, p *domain.Payment) error {
	if u.lateSettler == nil || u.bookings == nil || p.BookingID == uuid.Nil {
		return nil
	}
	// Only a payment that is holding or has taken money is worth settling; a
	// failed / expired callback leaves nothing behind.
	if !p.Status.HoldsMoney() {
		return nil
	}
	b, err := u.bookings.GetByID(ctx, p.BookingID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	trigger, ok := lateSettlementTrigger(b)
	if !ok {
		return nil // the booking is alive; the ordinary flow owns this payment
	}

	logging.FromContext(ctx).Warn("payment.paid_after_booking_closed",
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("booking_status", string(b.Status)),
		slog.String("trigger", string(trigger)),
	)
	_, err = u.lateSettler.SettleDepositOnCancel(ctx, systemActor, p.BookingID, DepositCancelInput{
		Trigger:     trigger,
		CancelledAt: b.CancelledAt,
		Reason:      strPtr("payment arrived after the booking was already closed"),
	})
	return err
}

// lateSettlementTrigger maps a closed booking onto the refund trigger whose
// policy applies. ok=false means the booking is still alive.
//
// A no-show is its own trigger (the venue keeps the money). A cancellation is
// attributed to whoever made it: the guest's own cancel is judged against the
// free-cancellation window, while a venue or system cancellation always
// returns the money in full. An unrecorded cancelled_by is treated as the
// VENUE's — the guest must not lose money because we failed to write down who
// cancelled.
func lateSettlementTrigger(b *domain.Booking) (domain.RefundTrigger, bool) {
	switch b.Status {
	case domain.BookingNoShow:
		return domain.RefundTriggerNoShow, true
	case domain.BookingCancelled:
		if b.CancelledBy != nil && *b.CancelledBy == domain.CancelledByGuest {
			return domain.RefundTriggerGuestCancel, true
		}
		return domain.RefundTriggerVenueCancel, true
	default:
		return "", false
	}
}

func strPtr(s string) *string { return &s }

func (u *webhookUseCase) applyToPayment(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, event *domain.WebhookEvent) error {
	switch event.Type {
	case domain.WebhookPaymentAuthorized:
		return u.applyAuthorized(ctx, gw, p, event)
	case domain.WebhookPaymentCaptured:
		return u.applyCaptured(ctx, p, event)
	case domain.WebhookPaymentFailed:
		return u.applyFailed(ctx, p, event)
	case domain.WebhookPaymentVoided:
		return u.applyVoided(ctx, p, event)
	case domain.WebhookPaymentExpired:
		return u.applyExpired(ctx, p, event)
	case domain.WebhookRefundSucceeded, domain.WebhookRefundFailed:
		// A refund we initiated ourselves (usecase/payments.RefundUseCase)
		// already records the outcome synchronously from the acquirer's
		// direct response. Reconciling a refund from a webhook-only
		// confirmation is a KNOWN GAP, not built in this change — see the
		// final report. Acknowledge so the acquirer stops retrying.
		logging.FromContext(ctx).Info("payment.webhook_refund_ack",
			slog.String("payment_id", p.ID.String()), slog.String("event_type", string(event.Type)))
		return nil
	default:
		logging.FromContext(ctx).Warn("payment.webhook_unknown_event",
			slog.String("payment_id", p.ID.String()), slog.String("event_type", string(event.Type)))
		return nil
	}
}

// applyAuthorized moves created → authorized: the hold is now real. If this
// loses the race for idx_payments_live_per_booking (another payment for the
// same booking got there first), the hold this payment placed must be
// released — this is the saga compensation from spec §6 applied to two
// concurrent checkouts on one booking instead of a lost table.
func (u *webhookUseCase) applyAuthorized(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment, event *domain.WebhookEvent) error {
	// Idempotency for this callback is PURPOSE-aware. A PRE-ORDER is captured
	// immediately on authorization (captureIfPreorder), so the authorized →
	// captured range is all "this callback's work is done or resumable":
	//   - already captured / refunded → nothing left to do, ack;
	//   - still only authorized → a previous immediate-capture attempt was
	//     declined (captureHold released the hold back to authorized) or never
	//     ran; a redelivery must RESUME the capture, not ack it as done —
	//     otherwise a failed pre-order capture is never retried and the money
	//     is left as an uncaptured hold that auto-expires (silent revenue loss).
	// A DEPOSIT is a plain created → authorized and is done once authorized.
	if p.Purpose.CapturesImmediately() {
		switch p.Status {
		case domain.PaymentCaptured, domain.PaymentRefunded, domain.PaymentPartiallyRefunded:
			return nil
		case domain.PaymentAuthorized:
			return u.captureIfPreorder(ctx, p)
		}
	} else if p.Status == domain.PaymentAuthorized {
		return nil // deposit already applied; events.Create dedups the common case
	}
	if err := domain.ValidatePaymentTransition(p.Status, domain.PaymentAuthorized); err != nil {
		return fmt.Errorf("webhook authorized on payment %s (currently %s): %w", p.ID, p.Status, err)
	}
	from := p.Status
	now := time.Now()
	txErr := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, from, domain.PaymentAuthorized, now); err != nil {
			return err
		}
		p.Status = domain.PaymentAuthorized
		p.AuthorizedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentAuthorized, now)
	})
	if txErr == nil {
		logging.FromContext(ctx).Info(logging.EventPaymentAuthorized, slog.String("payment_id", p.ID.String()))
		// The authorization is durably committed. The immediate pre-order
		// capture is a follow-on; if it fails, its error is PROPAGATED so
		// resolveAndApply leaves the webhook event UNPROCESSED (report item #9)
		// and a redelivery / the reconciler retries it — never swallowed.
		return u.captureIfPreorder(ctx, p)
	}
	if !errors.Is(txErr, domain.ErrAlreadyExists) {
		return txErr
	}
	return u.compensateLostRace(ctx, gw, p)
}

// captureIfPreorder captures a PRE-ORDER hold the instant it is authorized: the
// kitchen has to prepare the food, so a pre-order is taken at payment time
// rather than held until seating (a DEPOSIT, by contrast, stays a hold and is
// only captured on a late cancellation / no-show — see cancel.go). A DEPOSIT is
// a no-op (returns nil).
//
// It reuses CaptureOnSeating's exact CAS-guarded mechanic (captureHold), so it
// is idempotent: an already-captured pre-order finds status == captured and is
// a no-op, so a successful retry never double-captures. The capture error is
// RETURNED (not swallowed): a decline or an unknown outcome must leave the
// webhook event unprocessed so it is retried, otherwise the food is prepared
// while the money stays an uncaptured hold that silently auto-expires.
func (u *webhookUseCase) captureIfPreorder(ctx context.Context, p *domain.Payment) error {
	if !p.Purpose.CapturesImmediately() {
		return nil
	}
	cv := &captureVoidUseCase{payments: u.payments, ledger: u.ledger, outbox: u.outbox, gateways: u.gateways, tx: u.tx}
	if _, err := cv.captureHold(ctx, p); err != nil {
		logging.FromContext(ctx).Error("payment.preorder_immediate_capture_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		return fmt.Errorf("immediate capture of pre-order payment %s: %w", p.ID, err)
	}
	return nil
}

// compensateLostRace releases the hold this payment placed after it lost the
// booking-level race. It first re-reads the payment: if THIS SAME payment was
// already authorized by a different, still-valid callback (a legitimate
// duplicate, not a cross-payment race), nothing is released — voiding a
// guest's own already-applied hold would be the exact bug this function
// exists to prevent.
func (u *webhookUseCase) compensateLostRace(ctx context.Context, gw domain.PaymentGateway, p *domain.Payment) error {
	current, err := u.payments.GetByID(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("re-read payment %s before compensation: %w", p.ID, err)
	}
	if current.Status != domain.PaymentCreated {
		// This payment's own state already moved (e.g. a second, differently
		// identified delivery of a callback we already applied). Nothing to
		// compensate.
		return nil
	}
	if p.ProviderPaymentID == nil {
		return fmt.Errorf("compensate lost race for payment %s: no provider payment id", p.ID)
	}

	// External call, deliberately outside any DB transaction.
	if err := gw.Void(ctx, *p.ProviderPaymentID); err != nil {
		logging.FromContext(ctx).Error("payment.compensation_void_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
		// Answered as an error so the acquirer's own retry schedule tries the
		// callback again; Void on an acquirer is expected to be safe to call
		// again on a hold that is not yet released.
		return fmt.Errorf("void lost-race hold for payment %s: %w", p.ID, err)
	}

	now := time.Now()
	failureCode := "lost_booking_race"
	failureMessage := "another payment for the same booking was authorized first; this hold was released"
	txErr := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCreated, domain.PaymentFailed, now); err != nil {
			return err
		}
		p.Status = domain.PaymentFailed
		p.FailedAt = &now
		p.FailureCode = &failureCode
		p.FailureMessage = &failureMessage
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentFailed, now)
	})
	if txErr != nil {
		return txErr
	}
	logging.FromContext(ctx).Warn(logging.EventPaymentFailed,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("reason", failureCode),
	)
	return nil
}

// applyCaptured moves authorized → captured and books the split into the
// ledger (spec §9.2) in the SAME transaction as the status write.
func (u *webhookUseCase) applyCaptured(ctx context.Context, p *domain.Payment, event *domain.WebhookEvent) error {
	if p.Status == domain.PaymentCaptured {
		return nil
	}
	if err := domain.ValidatePaymentTransition(p.Status, domain.PaymentCaptured); err != nil {
		return fmt.Errorf("webhook captured on payment %s (currently %s): %w", p.ID, p.Status, err)
	}
	// Non-blocking item #4 (second review): captureLedgerEntries books the
	// payment's OWN full total, not whatever the acquirer actually reports
	// it cleared. If the acquirer only captures part of the hold (a partial
	// clearing FreedomPay's own docs show as possible, see mapPaymentStatus's
	// TODO(verify) on `partial`), booking the full amount here would make the
	// ledger silently disagree with what the bank actually moved. Refuse to
	// apply silently on a mismatch: log it and leave the event unprocessed
	// (resolveAndApply's caller does NOT call MarkProcessed on a non-nil
	// error) so it stays visible to a human / the reconciliation worker
	// instead of a wrong number quietly entering the books.
	if event.Amount.AmountMinor != 0 && event.Amount.AmountMinor != p.AmountMinor {
		logging.FromContext(ctx).Error("payment.webhook_captured_amount_mismatch",
			slog.String("payment_id", p.ID.String()),
			slog.Int64("payment_amount_minor", p.AmountMinor),
			slog.Int64("webhook_amount_minor", event.Amount.AmountMinor),
		)
		return fmt.Errorf(
			"webhook captured amount %d minor for payment %s does not match the payment's own total %d minor — a partial capture is not supported yet, needs reconciliation",
			event.Amount.AmountMinor, p.ID, p.AmountMinor)
	}
	from := p.Status
	now := time.Now()
	entries := captureLedgerEntries(*p, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return err
	}
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, from, domain.PaymentCaptured, now); err != nil {
			return err
		}
		if err := u.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		p.Status = domain.PaymentCaptured
		p.CapturedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentCaptured, now)
	})
	if err != nil {
		return err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentCaptured, slog.String("payment_id", p.ID.String()))
	return nil
}

func (u *webhookUseCase) applyFailed(ctx context.Context, p *domain.Payment, event *domain.WebhookEvent) error {
	if p.Status == domain.PaymentFailed {
		return nil
	}
	if err := domain.ValidatePaymentTransition(p.Status, domain.PaymentFailed); err != nil {
		return fmt.Errorf("webhook failed on payment %s (currently %s): %w", p.ID, p.Status, err)
	}
	from := p.Status
	now := time.Now()
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, from, domain.PaymentFailed, now); err != nil {
			return err
		}
		p.Status = domain.PaymentFailed
		p.FailedAt = &now
		if event.FailureCode != "" {
			p.FailureCode = &event.FailureCode
		}
		if event.FailureMessage != "" {
			p.FailureMessage = &event.FailureMessage
		}
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentFailed, now)
	})
	if err != nil {
		return err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentFailed, slog.String("payment_id", p.ID.String()))
	return nil
}

func (u *webhookUseCase) applyVoided(ctx context.Context, p *domain.Payment, event *domain.WebhookEvent) error {
	if p.Status == domain.PaymentVoided {
		return nil
	}
	if err := domain.ValidatePaymentTransition(p.Status, domain.PaymentVoided); err != nil {
		return fmt.Errorf("webhook voided on payment %s (currently %s): %w", p.ID, p.Status, err)
	}
	from := p.Status
	now := time.Now()
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, from, domain.PaymentVoided, now); err != nil {
			return err
		}
		p.Status = domain.PaymentVoided
		p.VoidedAt = &now
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentVoided, now)
	})
	if err != nil {
		return err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentVoided, slog.String("payment_id", p.ID.String()))
	return nil
}

func (u *webhookUseCase) applyExpired(ctx context.Context, p *domain.Payment, event *domain.WebhookEvent) error {
	if p.Status == domain.PaymentExpired {
		return nil
	}
	if err := domain.ValidatePaymentTransition(p.Status, domain.PaymentExpired); err != nil {
		return fmt.Errorf("webhook expired on payment %s (currently %s): %w", p.ID, p.Status, err)
	}
	from := p.Status
	now := time.Now()
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, from, domain.PaymentExpired, now); err != nil {
			return err
		}
		p.Status = domain.PaymentExpired
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentExpired, now)
	})
	if err != nil {
		return err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentExpired, slog.String("payment_id", p.ID.String()))
	return nil
}

// storeInvalidEvent records an unverified callback as evidence (spec §7:
// "signature did not verify → 401 and a payment_events row with
// signature_valid=false"). There is no ProviderEventID to key on — the
// signature failed before the payload could be trusted — so a hash of the raw
// body stands in, which still deduplicates an attacker or a misconfigured
// endpoint retrying the identical bytes.
func (u *webhookUseCase) storeInvalidEvent(ctx context.Context, provider domain.PaymentProvider, raw []byte, verr error) {
	sum := sha256.Sum256(raw)
	syntheticID := "invalid:" + hex.EncodeToString(sum[:])
	// Report item #16 (minor): the spec wants the payload stored as-is, not
	// just its length — an unverified callback is still evidence a human may
	// need to inspect (was it a misconfigured endpoint? an attacker probing
	// the signature? a legitimate delivery whose secret rotated?). Card data,
	// if any ever appeared in a body this malformed, is masked the same way
	// the verified path already masks it.
	payload, _ := json.Marshal(map[string]any{
		"raw_length":         len(raw),
		"verification_error": verr.Error(),
		"body":               maskRawWebhookBody(raw),
	})
	msg := verr.Error()
	row := &domain.PaymentEvent{
		ID: uuid.New(), Provider: provider, ProviderEventID: syntheticID,
		Payload: payload, SignatureValid: false, ReceivedAt: time.Now(),
		ProcessError: &msg,
	}
	if err := u.events.Create(ctx, row); err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		logging.FromContext(ctx).Error("payment.webhook_invalid_store_failed", slog.String("error", err.Error()))
	}
}

// maxStoredRawBody bounds how much of an unverified callback body is kept:
// enough to investigate a real delivery, small enough that an attacker
// spamming the endpoint cannot use payment_events as unbounded storage.
const maxStoredRawBody = 32 * 1024

// sensitiveBodyFields are masked wherever they appear in an unverified
// webhook body, regardless of provider — this path runs BEFORE a provider is
// even confirmed authentic, so it cannot rely on a provider-specific
// redaction helper (compare freedompay.redactedPayload, which only runs on a
// signature-verified message).
var sensitiveBodyFields = map[string]struct{}{
	"pg_card_pan": {}, "pg_card_exp": {}, "pg_card_owner": {}, "pg_card_brand": {},
	"pg_card_id": {}, "pg_card_token": {}, "pg_card_name": {}, "pg_card_hash": {},
	"pg_sig": {}, "cardnumber": {}, "card_number": {}, "cvv": {}, "cvc": {}, "signature": {},
}

// maskRawWebhookBody stores an unverified callback body as-is (report item
// #16, minor) instead of discarding it down to a length — but masks anything
// that looks like card data or a signature first, and bounds the size.
// It tries the two shapes a webhook body is ever sent in by this codebase's
// adapters (form-urlencoded, JSON); anything else is kept as a bounded,
// clearly-labelled opaque string rather than silently dropped.
func maskRawWebhookBody(raw []byte) any {
	if len(raw) > maxStoredRawBody {
		raw = raw[:maxStoredRawBody]
	}
	if values, err := url.ParseQuery(string(raw)); err == nil && len(values) > 0 {
		out := make(map[string]string, len(values))
		for k, vs := range values {
			if len(vs) == 0 {
				continue
			}
			if _, sensitive := sensitiveBodyFields[strings.ToLower(k)]; sensitive {
				out[k] = "[redacted]"
				continue
			}
			out[k] = vs[0]
		}
		return out
	}
	var asJSON map[string]any
	if err := json.Unmarshal(raw, &asJSON); err == nil {
		for k := range asJSON {
			if _, sensitive := sensitiveBodyFields[strings.ToLower(k)]; sensitive {
				asJSON[k] = "[redacted]"
			}
		}
		return asJSON
	}
	return map[string]string{"opaque_body_base64": base64.StdEncoding.EncodeToString(raw)}
}

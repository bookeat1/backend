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
	payments domain.PaymentRepository
	events   domain.PaymentEventRepository
	ledger   domain.PaymentLedgerRepository
	outbox   domain.PaymentOutboxRepository
	gateways gatewayResolver
	tx       domain.TxManager
}

// NewWebhookUseCase constructs the webhook-processing usecase.
func NewWebhookUseCase(
	payments domain.PaymentRepository,
	events domain.PaymentEventRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	tx domain.TxManager,
) WebhookUseCase {
	return &webhookUseCase{payments: payments, events: events, ledger: ledger, outbox: outbox, gateways: gateways, tx: tx}
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
	if p.Status == domain.PaymentAuthorized {
		return nil // already applied; a defensive no-op, events.Create already dedups the common case
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
		u.captureIfPreorder(ctx, p)
		return nil
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
// left untouched here.
//
// It reuses CaptureOnSeating's exact CAS-guarded mechanic (captureHold), so it
// is idempotent: a redelivered authorized webhook whose pre-order was already
// captured finds status == captured and is a no-op. The authorization is
// already durably committed by the caller, so a capture failure here (declined
// or unknown outcome) must NOT fail the whole webhook — it is logged and left
// for the reconciliation worker, exactly as a CaptureOnSeating failure is.
func (u *webhookUseCase) captureIfPreorder(ctx context.Context, p *domain.Payment) {
	if p.Purpose != domain.PurposePreorder {
		return
	}
	cv := &captureVoidUseCase{payments: u.payments, ledger: u.ledger, outbox: u.outbox, gateways: u.gateways, tx: u.tx}
	if _, err := cv.captureHold(ctx, p); err != nil {
		logging.FromContext(ctx).Error("payment.preorder_immediate_capture_failed",
			slog.String("payment_id", p.ID.String()), slog.String("error", err.Error()))
	}
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

package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
			// Redelivery of a callback we already stored (and, by the time
			// this returns, already processed or in the middle of being
			// processed by the request that inserted it first). Acknowledge
			// without reprocessing — this is the whole idempotency mechanism
			// (spec §7).
			logging.FromContext(ctx).Info(logging.EventPaymentWebhookReceived,
				slog.String("provider", string(provider)),
				slog.String("provider_event_id", event.ProviderEventID),
				slog.Bool("duplicate", true),
			)
			return nil
		}
		return err
	}

	logging.FromContext(ctx).Info(logging.EventPaymentWebhookReceived,
		slog.String("provider", string(provider)),
		slog.String("provider_event_id", event.ProviderEventID),
		slog.String("event_type", string(event.Type)),
	)

	p, err := u.resolvePayment(ctx, provider, event)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Never create a payment from a webhook (spec §7): store the
			// fact that we could not resolve it and stop. The acquirer still
			// gets a 200 — retrying will not make the payment exist.
			_ = u.events.MarkProcessed(ctx, row.ID, time.Now(), "unknown payment: no local record for this acquirer callback")
			logging.FromContext(ctx).Error("payment.webhook_unknown_payment",
				slog.String("provider", string(provider)),
				slog.String("provider_payment_id", event.ProviderPaymentID),
				slog.String("merchant_payment_id", event.MerchantPaymentID),
			)
			return nil
		}
		return err
	}

	if applyErr := u.apply(ctx, gw, p, event); applyErr != nil {
		_ = u.events.MarkProcessed(ctx, row.ID, time.Now(), applyErr.Error())
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
			return u.payments.GetByID(ctx, id)
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
		return nil
	}
	if !errors.Is(txErr, domain.ErrAlreadyExists) {
		return txErr
	}
	return u.compensateLostRace(ctx, gw, p)
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
	logging.FromContext(ctx).Info("payment.expired", slog.String("payment_id", p.ID.String()))
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
	payload, _ := json.Marshal(map[string]any{
		"raw_length":         len(raw),
		"verification_error": verr.Error(),
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

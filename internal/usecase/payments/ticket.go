package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// TicketPaymentUseCase is the MONEY half of an event-ticket purchase. It lives
// in usecase/payments (not usecase/tickets) so every line that moves money
// stays in the one package that owns the acquirer, the ledger and the CAS
// discipline — usecase/tickets owns capacity and the ticket lifecycle and calls
// this via a port. A ticket payment reuses the SAME payments table, gateway,
// ledger and webhook flow as a booking; the only difference is the subject
// (Payment.EventTicketID instead of BookingID) and the purpose
// (PurposeTicket, captured immediately like a pre-order).
type TicketPaymentUseCase interface {
	// CreateForTicket authorizes a hold for a ticket purchase. Idempotent by the
	// same idx_payments_idempotency guard as CreateForBooking: a retry with the
	// same key replays the existing payment instead of authorizing twice. The
	// hold is captured immediately by the webhook (PurposeTicket
	// CapturesImmediately), exactly like a pre-order.
	CreateForTicket(ctx context.Context, actor Actor, in TicketPaymentInput) (*domain.Payment, error)
	// RefundTicket makes the guest whole for a captured ticket payment (full
	// refund, no acquiring cost withheld — the simple documented policy for
	// tickets, configurable later). Idempotent via payment_refunds and the
	// captured→refunded CAS.
	RefundTicket(ctx context.Context, actor Actor, in TicketRefundInput) (*domain.Payment, error)
}

// TicketPaymentInput is a ticket checkout request. BaseAmountMinor is the
// venue's take (quantity × unit price) BEFORE the acquirer gross-up — the server
// computed it from the frozen ticket price, never the client.
type TicketPaymentInput struct {
	EventTicketID   uuid.UUID
	RestaurantID    uuid.UUID
	UserID          *uuid.UUID // nil = guest without an account
	BaseAmountMinor int64
	Currency        domain.Currency
	IdempotencyKey  string
	ReturnURL       string
	CallbackURL     string
	CustomerPhone   string
	CustomerEmail   string
}

// TicketRefundInput identifies the captured ticket payment to refund and who is
// asking. OwnerUserID is the ticket's buyer (nil = account-less guest checkout),
// used for the guest-owns-ticket authorization.
type TicketRefundInput struct {
	PaymentID      uuid.UUID
	OwnerUserID    *uuid.UUID
	Reason         *string
	IdempotencyKey string
}

type ticketPaymentUseCase struct {
	payments    domain.PaymentRepository
	refunds     domain.PaymentRefundRepository
	ledger      domain.PaymentLedgerRepository
	outbox      domain.PaymentOutboxRepository
	restaurants restaurantPaymentSettings
	gateways    gatewayResolver
	managers    managerChecker
	tx          domain.TxManager
	cfg         Config
}

// NewTicketPaymentUseCase constructs the ticket-payment usecase.
func NewTicketPaymentUseCase(
	payments domain.PaymentRepository,
	refunds domain.PaymentRefundRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	restaurants restaurantPaymentSettings,
	gateways gatewayResolver,
	managers managerChecker,
	tx domain.TxManager,
	cfg Config,
) TicketPaymentUseCase {
	return &ticketPaymentUseCase{
		payments: payments, refunds: refunds, ledger: ledger, outbox: outbox,
		restaurants: restaurants, gateways: gateways, managers: managers, tx: tx, cfg: cfg.withDefaults(),
	}
}

// CreateForTicket mirrors CreateForBooking's idempotency discipline exactly:
// the acquirer call carries our own key and happens OUTSIDE any DB transaction;
// the row is inserted only after the acquirer answered; two concurrent identical
// retries race on idx_payments_idempotency and the loser replays the winner.
func (u *ticketPaymentUseCase) CreateForTicket(ctx context.Context, actor Actor, in TicketPaymentInput) (*domain.Payment, error) {
	if in.EventTicketID == uuid.Nil {
		return nil, fmt.Errorf("%w: event ticket required", domain.ErrValidation)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}
	if in.BaseAmountMinor <= 0 {
		return nil, fmt.Errorf("%w: ticket amount must be positive", domain.ErrValidation)
	}
	// A guest with an account may only pay for their own ticket; an account-less
	// guest ticket authorizes any caller who reached here (same reasoning as
	// authorizeCreate — the transport layer verified the buyer's contact first).
	if !actor.staff() && in.UserID != nil && !actor.isUser(in.UserID) {
		return nil, fmt.Errorf("%w: ticket belongs to another guest", domain.ErrForbidden)
	}

	override, err := u.restaurants.GetPaymentOverride(ctx, in.RestaurantID)
	if err != nil {
		return nil, err
	}
	settings := resolveSettings(override, u.cfg)
	if !settings.Enabled {
		return nil, fmt.Errorf("%w: payments are not enabled for this restaurant", domain.ErrValidation)
	}

	currency := in.Currency
	if currency == "" {
		currency = domain.CurrencyKZT
	}
	base, err := domain.NewMoney(in.BaseAmountMinor, currency)
	if err != nil {
		return nil, err
	}
	fee, total, err := domain.GrossUpForAcquirer(base, settings.ServiceFeeBps)
	if err != nil {
		return nil, err
	}

	gw, err := u.gateways.Resolve(ctx, settings.Provider)
	if err != nil {
		return nil, err
	}
	provider := gw.Name()

	dbKey := "ticket:" + in.EventTicketID.String() + ":" + actorKey(actor) + ":" + in.IdempotencyKey
	if existing, err := u.replayTicket(ctx, provider, dbKey, in.EventTicketID); err != nil || existing != nil {
		return existing, err
	}

	paymentID := uuid.New()
	now := time.Now()
	expiresAt := now.Add(u.cfg.HoldTTL)
	ticketID := in.EventTicketID

	gwResp, err := gw.Authorize(ctx, domain.AuthorizeRequest{
		PaymentID:      paymentID,
		IdempotencyKey: dbKey,
		Amount:         total,
		Purpose:        domain.PurposeTicket,
		Description:    "BookEat: билеты на событие",
		HoldTTL:        u.cfg.HoldTTL,
		ReturnURL:      in.ReturnURL,
		CallbackURL:    in.CallbackURL,
		CustomerPhone:  in.CustomerPhone,
		CustomerEmail:  in.CustomerEmail,
		Metadata:       map[string]string{"event_ticket_id": ticketID.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("authorize ticket with %s: %w", provider, err)
	}

	p := &domain.Payment{
		ID: paymentID, EventTicketID: &ticketID, RestaurantID: in.RestaurantID, UserID: in.UserID,
		Provider: provider, ProviderPaymentID: nullableStr(gwResp.ProviderPaymentID), Purpose: domain.PurposeTicket,
		Status: domain.PaymentCreated, AmountMinor: total.AmountMinor, BaseAmountMinor: base.AmountMinor,
		FeeMinor: fee.AmountMinor, Currency: total.Currency, IdempotencyKey: dbKey,
		PaymentURL: nullableStr(gwResp.PaymentURL), ExpiresAt: &expiresAt,
		CreatedAt: now, UpdatedAt: now,
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.payments.Create(ctx, p); err != nil {
			return err
		}
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentCreated, now)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			if existing, rerr := u.payments.GetByIdempotencyKey(ctx, provider, dbKey); rerr == nil {
				return existing, nil
			}
		}
		return nil, err
	}

	logging.FromContext(ctx).Info(logging.EventPaymentCreated,
		slog.String("payment_id", p.ID.String()),
		slog.String("event_ticket_id", ticketID.String()),
		slog.String("provider", string(p.Provider)),
		slog.Int64("amount_minor", p.AmountMinor),
	)
	return p, nil
}

func (u *ticketPaymentUseCase) replayTicket(ctx context.Context, provider domain.PaymentProvider, dbKey string, ticketID uuid.UUID) (*domain.Payment, error) {
	existing, err := u.payments.GetByIdempotencyKey(ctx, provider, dbKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if existing.EventTicketID == nil || *existing.EventTicketID != ticketID {
		return nil, fmt.Errorf("%w: this idempotency key was used for a different ticket", domain.ErrAlreadyExists)
	}
	return existing, nil
}

// RefundTicket fully refunds a captured ticket payment. The sequencing mirrors
// RefundUseCase.settleWithRefund: the refund row is claimed and the acquirer is
// called OUTSIDE any DB transaction, its outcome persisted durably, and only
// then the ledger reversal + captured→refunded status committed together. It is
// a one-shot full refund, so payment_refunds' idempotency key and the
// captured→refunded CAS make a retry idempotent.
func (u *ticketPaymentUseCase) RefundTicket(ctx context.Context, actor Actor, in TicketRefundInput) (*domain.Payment, error) {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}
	p, err := u.payments.GetByID(ctx, in.PaymentID)
	if err != nil {
		return nil, err
	}
	if p.EventTicketID == nil {
		return nil, fmt.Errorf("%w: payment %s is not a ticket payment", domain.ErrValidation, p.ID)
	}
	if err := u.authorizeTicketRefund(ctx, actor, p, in.OwnerUserID); err != nil {
		return nil, err
	}
	if p.Status == domain.PaymentRefunded {
		return p, nil // already fully refunded — idempotent
	}
	if p.Status != domain.PaymentCaptured {
		return nil, fmt.Errorf("%w: payment is %s, only a captured ticket payment can be refunded", domain.ErrInvalidStatus, p.Status)
	}

	// Full make-whole refund: everything the guest paid goes back. No acquiring
	// cost is withheld for a ticket (documented simple policy; a future
	// event-level refund policy would set RestaurantMinor/AcquirerMinor here).
	settlement := domain.Settlement{GuestMinor: p.AmountMinor}
	now := time.Now()

	refund, err := u.claimTicketRefund(ctx, p, settlement, in, now)
	if err != nil {
		return nil, err
	}

	entries := settlementLedgerEntries(*p, settlement, &refund.ID, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return nil, err
	}
	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, domain.PaymentRefunded, now); err != nil {
			return err
		}
		p.Status = domain.PaymentRefunded
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentRefunded, now)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			// A concurrent identical refund already committed the terminal
			// status. Re-read and return it — idempotent, not a double refund.
			if current, rerr := u.payments.GetByID(ctx, p.ID); rerr == nil && current.Status == domain.PaymentRefunded {
				return current, nil
			}
		}
		return nil, fmt.Errorf("ticket refund succeeded at %s but local commit failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, err)
	}

	logging.FromContext(ctx).Info(logging.EventPaymentRefunded,
		slog.String("payment_id", p.ID.String()),
		slog.String("event_ticket_id", p.EventTicketID.String()),
		slog.Int64("guest_minor", settlement.GuestMinor),
	)
	return p, nil
}

// claimTicketRefund creates-or-resumes the payment_refunds row and makes the
// single acquirer call it is allowed to make, returning a Succeeded refund or an
// error for any non-terminal-success outcome (pending/failed/in-flight).
func (u *ticketPaymentUseCase) claimTicketRefund(ctx context.Context, p *domain.Payment, settlement domain.Settlement, in TicketRefundInput, now time.Time) (*domain.PaymentRefund, error) {
	existing, err := u.refunds.GetByIdempotencyKey(ctx, p.ID, in.IdempotencyKey)
	switch {
	case err == nil:
		// resume below
	case errors.Is(err, domain.ErrNotFound):
		existing = &domain.PaymentRefund{
			ID: uuid.New(), PaymentID: p.ID, AmountMinor: settlement.GuestMinor, Currency: p.Currency,
			Status: domain.RefundCreated, Reason: in.Reason, IdempotencyKey: in.IdempotencyKey,
			CreatedAt: now, UpdatedAt: now,
		}
		if cerr := u.refunds.Create(u.tx.Detach(ctx), existing); cerr != nil {
			if !errors.Is(cerr, domain.ErrAlreadyExists) {
				return nil, cerr
			}
			existing, err = u.refunds.GetByIdempotencyKey(ctx, p.ID, in.IdempotencyKey)
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, err
	}

	switch existing.Status {
	case domain.RefundSucceeded:
		return existing, nil
	case domain.RefundPending:
		return nil, fmt.Errorf(
			"refund for payment %s is in an unknown state after a previous timeout — reconcile before retrying: %w",
			p.ID, domain.ErrProviderOutcomeUnknown)
	case domain.RefundFailed:
		return nil, fmt.Errorf("refund for payment %s was declined by %s: %w", p.ID, p.Provider, domain.ErrProviderDeclined)
	case domain.RefundInFlight:
		return nil, fmt.Errorf("%w: a refund attempt for payment %s is already in flight", domain.ErrAlreadyExists, p.ID)
	}

	// RefundCreated: claim it and call the acquirer.
	if err := u.refunds.CompareAndSwapStatus(u.tx.Detach(ctx), existing.ID, domain.RefundCreated, domain.RefundInFlight, now); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: a refund attempt for payment %s is already in flight", domain.ErrAlreadyExists, p.ID)
		}
		return nil, err
	}
	existing.Status = domain.RefundInFlight

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		return nil, err
	}
	if p.ProviderPaymentID == nil {
		return nil, fmt.Errorf("payment %s has no provider payment id: %w", p.ID, domain.ErrValidation)
	}

	// External call, deliberately outside any DB transaction.
	gwResp, gwErr := gw.Refund(ctx, *p.ProviderPaymentID, domain.Money{AmountMinor: settlement.GuestMinor, Currency: p.Currency})
	switch {
	case gwErr == nil && gwResp.Status == domain.RefundSucceeded:
		existing.Status = domain.RefundSucceeded
		existing.ProviderRefundID = nullableStr(gwResp.ProviderRefundID)
	case gwErr == nil:
		existing.Status = domain.RefundPending
		msg := fmt.Sprintf("acquirer accepted the request but reported status %q, not succeeded", gwResp.Status)
		existing.FailureMessage = &msg
	case errors.Is(gwErr, domain.ErrProviderDeclined):
		msg := gwErr.Error()
		existing.Status = domain.RefundFailed
		existing.FailureMessage = &msg
	default:
		msg := gwErr.Error()
		existing.Status = domain.RefundPending
		existing.FailureMessage = &msg
	}
	existing.UpdatedAt = now

	if perr := u.refunds.Update(u.tx.Detach(ctx), existing); perr != nil {
		if gwErr != nil {
			return nil, fmt.Errorf("refund with %s: %w (recording the attempt also failed: %s)", p.Provider, gwErr, perr)
		}
		return nil, fmt.Errorf("refund succeeded at %s but recording it failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, perr)
	}
	if existing.Status != domain.RefundSucceeded {
		if gwErr != nil {
			return nil, fmt.Errorf("refund with %s: %w", p.Provider, gwErr)
		}
		return nil, fmt.Errorf("refund for payment %s left %s by %s, needs reconciliation", p.ID, existing.Status, p.Provider)
	}
	return existing, nil
}

// authorizeTicketRefund: venue staff need PermPaymentRefund at the payment's own
// restaurant (a hostess may not refund — same as booking refunds); a guest may
// refund their OWN ticket (or an account-less one, gated upstream by contact
// verification), never someone else's.
func (u *ticketPaymentUseCase) authorizeTicketRefund(ctx context.Context, actor Actor, p *domain.Payment, ownerUserID *uuid.UUID) error {
	if actor.staff() {
		return authorizeStaffPermission(ctx, u.managers, actor, p.RestaurantID, domain.PermPaymentRefund)
	}
	if ownerUserID != nil && !actor.isUser(ownerUserID) {
		return fmt.Errorf("%w: ticket belongs to another guest", domain.ErrForbidden)
	}
	return nil
}

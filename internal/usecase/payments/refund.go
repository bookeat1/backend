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

// RefundUseCase resolves a cancelled or a no-show booking's captured payment
// into its final split across guest / restaurant / platform / acquirer
// (spec §9.1, scenarios 5 and 6). It is a ONE-SHOT, whole-payment settlement:
// a payment can be settled exactly once — enforced by Payment.SettledAt (see
// domain.Payment's doc comment and report item #7), not merely by
// GetLiveByBookingID no longer returning it (that alone only holds for the
// refund outcome; a late-cancellation/no-show settlement deliberately leaves
// Status == captured, so it stays "live" forever without the SettledAt
// guard). Refunding a single pre-ordered line while the rest of the booking
// stands is a different operation and is explicitly out of scope here (spec
// §6) — see the report for the KNOWN GAP this leaves.
type RefundUseCase interface {
	Settle(ctx context.Context, actor Actor, bookingID uuid.UUID, in SettleInput) (*domain.Payment, error)
}

// SettleInput carries what only the caller can know: why the booking ended
// (Trigger) and, for the exceptional case, a manual override of the
// cancellation time.
type SettleInput struct {
	Trigger domain.RefundTrigger
	// ManualCancelledAt overrides the booking's own CancelledAt when deciding
	// whether a guest cancellation landed before or after the venue's
	// deadline. STAFF/ADMIN ONLY — Settle rejects it from any other actor.
	// It exists for the rare cancellation that happened outside the booking
	// flow (e.g. over the phone) and was never recorded on the booking row.
	//
	// Report item #15: before this, the caller supplied BOTH CancelledAt and
	// CancelDeadline directly, and — combined with the fact that a guest
	// booking with no account authorizes ANY caller who knows the booking id
	// — that let anyone manufacture an "in time" cancellation and walk away
	// with almost the full refund regardless of when the booking was
	// actually cancelled, or whether it was cancelled at all. Both values are
	// now server-derived: the deadline always comes from the venue's own
	// policy (cancelDeadlineResolver), and the cancellation time always comes
	// from the booking's own CancelledAt unless staff/admin explicitly
	// overrides it here.
	ManualCancelledAt *time.Time
	Reason            *string
	// IdempotencyKey guards the acquirer-facing refund call the same way
	// CreateInput.IdempotencyKey guards Authorize: a retry after a timeout
	// must not refund twice. It also guards the whole settlement (see
	// Payment.SettledAt): a retry with the SAME key resumes; a different key
	// against an already-settled payment is a conflict, never a second
	// settlement.
	IdempotencyKey string
}

type refundUseCase struct {
	payments domain.PaymentRepository
	refunds  domain.PaymentRefundRepository
	ledger   domain.PaymentLedgerRepository
	outbox   domain.PaymentOutboxRepository
	gateways gatewayResolver
	managers managerChecker
	bookings bookingReader
	deadline cancelDeadlineResolver
	tx       domain.TxManager
	cfg      Config
}

// NewRefundUseCase constructs the settlement usecase.
func NewRefundUseCase(
	payments domain.PaymentRepository,
	refunds domain.PaymentRefundRepository,
	ledger domain.PaymentLedgerRepository,
	outbox domain.PaymentOutboxRepository,
	gateways gatewayResolver,
	managers managerChecker,
	bookings bookingReader,
	deadline cancelDeadlineResolver,
	tx domain.TxManager,
	cfg Config,
) RefundUseCase {
	return &refundUseCase{
		payments: payments, refunds: refunds, ledger: ledger, outbox: outbox,
		gateways: gateways, managers: managers, bookings: bookings, deadline: deadline,
		tx: tx, cfg: cfg.withDefaults(),
	}
}

// Settle computes the settlement, claims exclusive ownership of it (report
// item #7), and either:
//   - writes nothing but an audit event when no money moves (a late
//     cancellation or a no-show: the venue already holds what it is entitled
//     to, see settlementLedgerEntries's doc comment on why this batch is
//     legitimately empty), or
//   - refunds the guest's share at the acquirer (external call, OUTSIDE any
//     DB transaction) and only then commits the refund row, the ledger delta
//     and the payment's terminal status together.
func (u *refundUseCase) Settle(ctx context.Context, actor Actor, bookingID uuid.UUID, in SettleInput) (*domain.Payment, error) {
	if !in.Trigger.Valid() {
		return nil, fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, in.Trigger)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}
	if in.ManualCancelledAt != nil && !actor.staff() {
		return nil, fmt.Errorf("%w: only venue staff or admin may override the cancellation time", domain.ErrForbidden)
	}

	p, err := u.payments.GetLiveByBookingID(ctx, bookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeSettle(ctx, u.managers, actor, in.Trigger, p); err != nil {
		return nil, err
	}
	if p.Status != domain.PaymentCaptured {
		return nil, fmt.Errorf(
			"%w: payment is %s, settle only applies to a captured payment (an authorized hold is voided, not settled)",
			domain.ErrInvalidStatus, p.Status)
	}

	// Report item #7: a payment already settled (by ANY trigger, including
	// the no-refund outcomes that never move Status away from captured) must
	// reject a DIFFERENT settlement attempt outright. The SAME key resumes —
	// its own idempotency mechanisms below (payment_refunds for the refund
	// branch, the outbox for the no-refund branch) decide whether it is
	// truly finished or still needs to continue (e.g. a refund stuck
	// `pending` after a timeout, report item #1).
	if p.SettledAt != nil {
		if p.SettlementIdempotencyKey == nil || *p.SettlementIdempotencyKey != in.IdempotencyKey {
			return nil, fmt.Errorf("%w: payment %s was already settled", domain.ErrAlreadyExists, p.ID)
		}
	}

	cancelledAt, cancelDeadline, err := u.resolveTiming(ctx, bookingID, in)
	if err != nil {
		return nil, err
	}

	settlement, err := domain.SettleRefund(*p, in.Trigger, cancelledAt, cancelDeadline,
		domain.RefundPolicy{AcquiringBps: u.cfg.RefundAcquiringBps})
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if p.SettledAt == nil {
		if claimErr := u.payments.ClaimSettlement(ctx, p.ID, in.IdempotencyKey, in.Trigger, now); claimErr != nil {
			if !errors.Is(claimErr, domain.ErrAlreadyExists) {
				return nil, claimErr
			}
			// Lost the claim race. Re-read: the SAME key means a concurrent
			// identical retry won it first — resume with its row instead of
			// erroring or settling a second time. A DIFFERENT key means a
			// genuine conflict.
			current, rerr := u.payments.GetByID(ctx, p.ID)
			if rerr != nil {
				return nil, rerr
			}
			if current.SettlementIdempotencyKey == nil || *current.SettlementIdempotencyKey != in.IdempotencyKey {
				return nil, fmt.Errorf("%w: payment %s was already settled", domain.ErrAlreadyExists, p.ID)
			}
			p = current
		} else {
			p.SettledAt = &now
			trig := in.Trigger
			p.SettledTrigger = &trig
			key := in.IdempotencyKey
			p.SettlementIdempotencyKey = &key
		}
	}

	if settlement.GuestMinor == 0 {
		return u.settleWithNoRefund(ctx, p, settlement, now)
	}
	return u.settleWithRefund(ctx, p, settlement, in, now)
}

// resolveTiming derives the server-trusted cancellation timing (report item
// #15). venue_cancel and no_show never read cancelledAt/cancelDeadline
// (domain.SettleRefund's switch decides those two outcomes before ever
// looking at the timing), so the booking lookup and policy resolution below
// only matter for RefundTriggerGuestCancel — but it is cheap and always
// correct to compute them for every trigger, and doing so unconditionally
// means a future change to SettleRefund's logic cannot silently start
// trusting an unresolved zero value.
func (u *refundUseCase) resolveTiming(ctx context.Context, bookingID uuid.UUID, in SettleInput) (cancelledAt, cancelDeadline time.Time, err error) {
	booking, err := u.bookings.GetByID(ctx, bookingID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	cancelDeadline, err = u.deadline.CancelDeadlineFor(ctx, *booking)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}

	if in.Trigger != domain.RefundTriggerGuestCancel {
		return time.Time{}, cancelDeadline, nil
	}
	if in.ManualCancelledAt != nil {
		if in.ManualCancelledAt.After(time.Now()) {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: manual cancellation time is in the future", domain.ErrValidation)
		}
		return *in.ManualCancelledAt, cancelDeadline, nil
	}
	if booking.CancelledAt == nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"%w: booking %s has no recorded cancellation — a guest-cancel settlement needs one (staff/admin may set SettleInput.ManualCancelledAt for an out-of-band cancellation)",
			domain.ErrValidation, booking.ID)
	}
	return *booking.CancelledAt, cancelDeadline, nil
}

// settleWithNoRefund handles the late-cancellation / no-show outcome: the
// venue keeps exactly what capture already booked it, so there is nothing new
// to move. It still records that the booking's fate was resolved, for audit
// and for the (separate, out-of-scope) payout register.
//
// The settlement claim (Payment.SettledAt) already happened in Settle before
// this is called, so a retry only needs to check whether the outbox event
// was already written — resuming a crash between the claim commit and this
// tx must not write a duplicate payment.settled event.
func (u *refundUseCase) settleWithNoRefund(ctx context.Context, p *domain.Payment, settlement domain.Settlement, now time.Time) (*domain.Payment, error) {
	already, err := u.outbox.ExistsForPayment(ctx, p.ID, domain.EventPaymentSettled)
	if err != nil {
		return nil, err
	}
	if already {
		return p, nil
	}

	entries := settlementLedgerEntries(*p, settlement, nil, now)
	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if len(entries) > 0 {
			// Defensive: the algorithm should never produce a non-empty
			// batch when GuestMinor is zero, but if it ever does, the
			// invariant is still checked before it is written.
			if err := domain.ValidateLedgerBalance(entries); err != nil {
				return err
			}
			if err := u.ledger.CreateBatch(ctx, entries); err != nil {
				return err
			}
		}
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentSettled, now)
	})
	if err != nil {
		return nil, err
	}
	logging.FromContext(ctx).Info(logging.EventPaymentSettled,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.Int64("restaurant_minor", settlement.RestaurantMinor),
		slog.Int64("platform_minor", settlement.PlatformMinor),
	)
	return p, nil
}

// settleWithRefund handles guest cancellation before the deadline and venue
// cancellation: money goes back to the guest.
//
// Sequencing is deliberate and split into two independently-durable steps,
// NOT one big transaction, because the acquirer call sits between them and a
// DB transaction must never wrap an external call:
//
//  1. claim the refund row (created -> in_flight, report item #2) then call
//     the acquirer, then immediately persist the refund row's true outcome on
//     its OWN write (u.tx.Detach) — this is the durable proof of what
//     happened, and it survives even if step 2 fails;
//  2. only then attempt the ledger delta + the payment's terminal status +
//     the outbox event as one atomic commit.
//
// If step 2 fails after step 1 succeeded, the error says so explicitly
// ("needs reconciliation") instead of silently reporting success — the
// KNOWN GAP is that closing that gap automatically is the reconciliation
// worker's job, not built here (see the report).
func (u *refundUseCase) settleWithRefund(ctx context.Context, p *domain.Payment, settlement domain.Settlement, in SettleInput, now time.Time) (*domain.Payment, error) {
	existing, err := u.refunds.GetByIdempotencyKey(ctx, p.ID, in.IdempotencyKey)
	switch {
	case err == nil:
		// A previous attempt with this key exists — resume it below instead
		// of asking the acquirer for a brand new refund.
	case errors.Is(err, domain.ErrNotFound):
		existing = &domain.PaymentRefund{
			ID: uuid.New(), PaymentID: p.ID, AmountMinor: settlement.GuestMinor, Currency: p.Currency,
			Status: domain.RefundCreated, Reason: in.Reason, IdempotencyKey: in.IdempotencyKey,
			CreatedAt: now, UpdatedAt: now,
		}

		// Report item #11: never refund more than what remains. Checked
		// right before the row that WOULD authorize the acquirer call is
		// created. This usecase is a one-shot whole-payment settlement (see
		// the type doc comment) so SucceededTotal is 0 in the designed
		// path — Payment.ClaimSettlement above is what actually makes a
		// second settlement attempt impossible — but this stays as the
		// belt-and-braces DB-facing check the review asked for, and it is
		// what a future itemised-refund feature would come to depend on.
		//
		// NOTE (disclosed deviation): the review asked for this check to run
		// "inside the same transaction as the refund insert". This package's
		// existing, owner-approved convention (see refund.go's original
		// design note, still true here) is that the refund row's insert is
		// deliberately NOT wrapped in a DB transaction — it is its own
		// durable write specifically so it survives even if the later
		// ledger/status commit fails. Wrapping the check and the insert
		// together would mean wrapping an insert that must survive on its
		// own, which would undo that property. The check therefore runs
		// immediately before the insert, not inside a shared transaction
		// with it; closing this gap for real needs the same real-Postgres
		// SELECT ... FOR UPDATE the team-memory note flags as not built yet.
		total, terr := u.refunds.SucceededTotal(ctx, p.ID)
		if terr != nil {
			return nil, terr
		}
		if total+settlement.GuestMinor > p.AmountMinor {
			return nil, fmt.Errorf("%w: refund of %d plus already-succeeded %d exceeds payment total %d",
				domain.ErrValidation, settlement.GuestMinor, total, p.AmountMinor)
		}

		if cerr := u.refunds.Create(u.tx.Detach(ctx), existing); cerr != nil {
			if !errors.Is(cerr, domain.ErrAlreadyExists) {
				return nil, cerr
			}
			// Lost the race to insert: a concurrent identical retry won.
			// Resume ITS row instead of creating a second attempt.
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
		// Already done — fall through to the (idempotent) ledger/status
		// commit below.
	case domain.RefundPending:
		// Report item #1: the previous attempt's outcome at the acquirer is
		// UNKNOWN (a timeout, a 5xx, or a malformed answer). Calling Refund
		// again here would risk a second real-world refund if the first one
		// actually landed. The only safe next step is reading the
		// acquirer's own state or waiting for a webhook — neither is wired
		// up yet (KNOWN GAP, reconciliation worker), so this surfaces as an
		// explicit, loud error instead of silently retrying.
		return nil, fmt.Errorf(
			"refund for payment %s is in an unknown state after a previous timeout — reconcile via %s's status read before retrying, do not call Refund again: %w",
			p.ID, p.Provider, domain.ErrProviderOutcomeUnknown)
	case domain.RefundFailed:
		msg := "declined"
		if existing.FailureMessage != nil {
			msg = *existing.FailureMessage
		}
		return nil, fmt.Errorf("refund for payment %s was declined by %s (%s): %w", p.ID, p.Provider, msg, domain.ErrProviderDeclined)
	case domain.RefundInFlight:
		// Report item #2: another request currently holds the claim on this
		// exact refund attempt (its own gateway call is in progress right
		// now). This call lost the race and must not call the acquirer too.
		return nil, fmt.Errorf("%w: a refund attempt for payment %s is already in flight", domain.ErrAlreadyExists, p.ID)
	case domain.RefundCreated:
		if err := u.claimAndCallGateway(ctx, p, existing, settlement, now); err != nil {
			return nil, err
		}
	}

	entries := settlementLedgerEntries(*p, settlement, &existing.ID, now)
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return nil, err
	}
	// This usecase always runs a whole-payment, one-shot settlement: a
	// partial pre-order refund is a different, unimplemented operation
	// (spec §6), so the terminal status here is always the full "refunded",
	// never "partially_refunded" — even when the guest is not made whole
	// (early cancellation withholds the acquiring cost): there is no further
	// refund action left to take against this payment.
	newStatus := domain.PaymentRefunded

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.ledger.CreateBatch(ctx, entries); err != nil {
			return err
		}
		if err := u.payments.CompareAndSwapStatus(ctx, p.ID, domain.PaymentCaptured, newStatus, now); err != nil {
			return err
		}
		p.Status = newStatus
		return publishPaymentEvent(ctx, u.outbox, p, domain.EventPaymentRefunded, now)
	})
	if err != nil {
		return nil, fmt.Errorf("refund succeeded at %s but local ledger/status commit failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, err)
	}

	logging.FromContext(ctx).Info(logging.EventPaymentRefunded,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.Int64("guest_minor", settlement.GuestMinor),
		slog.Int64("acquirer_minor", settlement.AcquirerMinor),
	)
	return p, nil
}

// claimAndCallGateway claims the refund row exclusively (created -> in_flight,
// report item #2) and makes the single acquirer call this refund is allowed
// ever to make. On return, existing.Status is one of Succeeded / Failed /
// Pending, and that outcome is already durably recorded — the caller (
// settleWithRefund) only needs to act on Succeeded; anything else is
// returned as an error.
func (u *refundUseCase) claimAndCallGateway(ctx context.Context, p *domain.Payment, existing *domain.PaymentRefund, settlement domain.Settlement, now time.Time) error {
	if err := u.refunds.CompareAndSwapStatus(u.tx.Detach(ctx), existing.ID, domain.RefundCreated, domain.RefundInFlight, now); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return fmt.Errorf("%w: a refund attempt for payment %s is already in flight", domain.ErrAlreadyExists, p.ID)
		}
		return err
	}
	existing.Status = domain.RefundInFlight

	gw, err := u.gateways.ForRefund(p.Provider)
	if err != nil {
		return err
	}
	if p.ProviderPaymentID == nil {
		return fmt.Errorf("payment %s has no provider payment id: %w", p.ID, domain.ErrValidation)
	}

	// External call, deliberately outside any DB transaction.
	//
	// Report item #3 (correcting a previously false comment here): this call
	// carries NO idempotency key the acquirer honours. domain.PaymentGateway.
	// Refund's signature is `Refund(ctx, providerPaymentID, amount)` — no key
	// parameter — and FreedomPay's /g2g/refund body is only pg_payment_id /
	// pg_amount / pg_currency (see freedompay/gateway.go's Refund). The ONLY
	// protection against a duplicate refund is local: the created->in_flight
	// claim just above, plus payment_refunds' own idempotency_key unique
	// index, plus refusing to retry a `pending` row (report item #1). Nothing
	// here relies on the acquirer resolving a repeated request to the same
	// money movement.
	gwResp, gwErr := gw.Refund(ctx, *p.ProviderPaymentID, domain.Money{AmountMinor: settlement.GuestMinor, Currency: p.Currency})

	switch {
	case gwErr == nil:
		// Report item #12: an HTTP-level success does not by itself mean
		// "money moved" — only the gateway's OWN explicit status word does.
		// Anything else (an unmapped/unknown status, or an explicit
		// "processing") is treated exactly like a timeout: pending, credit
		// nothing, wait for a webhook or a reconciliation read.
		if gwResp.Status == domain.RefundSucceeded {
			existing.Status = domain.RefundSucceeded
			existing.ProviderRefundID = nullableStr(gwResp.ProviderRefundID)
		} else {
			existing.Status = domain.RefundPending
			msg := fmt.Sprintf("acquirer accepted the request but reported status %q, not succeeded", gwResp.Status)
			existing.FailureMessage = &msg
		}
	case errors.Is(gwErr, domain.ErrProviderDeclined):
		// A definite, explicit "no" (report item #1): safe to record as a
		// terminal failure.
		msg := gwErr.Error()
		existing.Status = domain.RefundFailed
		existing.FailureMessage = &msg
	default:
		// domain.ErrProviderOutcomeUnknown (timeout / 5xx / malformed
		// answer) or any other error this usecase does not specifically
		// recognise: treated as UNKNOWN, never as a definite failure. A
		// definite failure would look safe to retry from scratch, which is
		// exactly how a timed-out refund that actually landed gets sent
		// twice.
		msg := gwErr.Error()
		existing.Status = domain.RefundPending
		existing.FailureMessage = &msg
	}
	existing.UpdatedAt = now

	// Durable BEFORE the ledger/status commit in settleWithRefund — this row
	// is the proof of what actually happened if that commit fails.
	if perr := u.refunds.Update(u.tx.Detach(ctx), existing); perr != nil {
		logging.FromContext(ctx).Error("payment.refund_attempt_not_recorded",
			slog.String("payment_id", p.ID.String()), slog.String("error", perr.Error()))
		if gwErr != nil {
			return fmt.Errorf("refund with %s: %w (recording the attempt also failed: %s)", p.Provider, gwErr, perr)
		}
		return fmt.Errorf("refund succeeded at %s but recording it failed, payment %s needs reconciliation: %w",
			p.Provider, p.ID, perr)
	}

	if existing.Status != domain.RefundSucceeded {
		if gwErr != nil {
			return fmt.Errorf("refund with %s: %w", p.Provider, gwErr)
		}
		return fmt.Errorf("refund for payment %s left %s by %s, needs reconciliation", p.ID, existing.Status, p.Provider)
	}
	return nil
}

// authorizeSettle decides who may trigger which outcome: a guest may only
// cancel their own booking (or an account-less guest booking, same reasoning
// as authorizeCreate); a venue-caused cancellation or a no-show is a staff
// call only — a guest must never be able to mark themselves a no-show to
// dodge the late-cancellation forfeit. Every staff path is additionally
// scoped to the payment's own restaurant (report item #13): a manager of
// venue A must not be able to settle venue B's payment just by knowing its
// booking id.
func authorizeSettle(ctx context.Context, managers managerChecker, actor Actor, trigger domain.RefundTrigger, p *domain.Payment) error {
	switch trigger {
	case domain.RefundTriggerGuestCancel:
		if actor.staff() {
			return authorizeStaffForRestaurant(ctx, managers, actor, p.RestaurantID)
		}
		if p.UserID != nil && !actor.isUser(p.UserID) {
			return fmt.Errorf("%w: payment belongs to another guest", domain.ErrForbidden)
		}
		return nil
	case domain.RefundTriggerVenueCancel, domain.RefundTriggerNoShow:
		return authorizeStaffForRestaurant(ctx, managers, actor, p.RestaurantID)
	default:
		return fmt.Errorf("%w: unknown refund trigger %q", domain.ErrValidation, trigger)
	}
}

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

// CreateUseCase starts (or replays) the payment for a booking.
type CreateUseCase interface {
	CreateForBooking(ctx context.Context, actor Actor, in CreateInput) (*domain.Payment, error)
}

// CreateInput is a checkout request.
type CreateInput struct {
	BookingID uuid.UUID
	// IdempotencyKey is the client's retry token (e.g. an Idempotency-Key
	// header). Required: without it a lost response and a client retry would
	// place a second hold. Scoped to the booking below it, so the same
	// client-chosen string used for a different booking is a different key —
	// same reasoning as bookings.IdempotencyKey.
	IdempotencyKey string
	// ReturnURL is where the guest lands after the hosted payment page.
	ReturnURL string
	// CallbackURL is our webhook route for whichever provider gets resolved.
	// The transport layer builds it per-provider (it must match the route the
	// signature is computed against, see freedompay.Config.ResultScriptName).
	CallbackURL string
}

type createUseCase struct {
	payments    domain.PaymentRepository
	outbox      domain.PaymentOutboxRepository
	bookings    bookingReader
	items       bookingItemReader
	restaurants restaurantPaymentSettings
	gateways    gatewayResolver
	managers    managerChecker
	tx          domain.TxManager
	cfg         Config
}

// NewCreateUseCase constructs the payment-creation usecase.
func NewCreateUseCase(
	payments domain.PaymentRepository,
	outbox domain.PaymentOutboxRepository,
	bookings bookingReader,
	items bookingItemReader,
	restaurants restaurantPaymentSettings,
	gateways gatewayResolver,
	managers managerChecker,
	tx domain.TxManager,
	cfg Config,
) CreateUseCase {
	return &createUseCase{
		payments: payments, outbox: outbox, bookings: bookings, items: items,
		restaurants: restaurants, gateways: gateways, managers: managers, tx: tx, cfg: cfg.withDefaults(),
	}
}

// CreateForBooking computes the amount, resolves an acquirer, places a hold
// and stores the intent. It is idempotent by construction:
//
//   - the acquirer call always carries our own idempotency key first (spec
//     §8: "a retry after a timeout resolves to the same payment"), so even a
//     request that never reaches the "insert the row" step below is safe to
//     retry — the acquirer itself resolves the retry to the same hold;
//   - the row is only inserted AFTER the acquirer answered, so nothing is ever
//     written locally for a call that failed;
//   - two concurrent callers using the SAME key race on
//     idx_payments_idempotency (UNIQUE (provider, idempotency_key)); the
//     loser's insert fails with ErrAlreadyExists and this method replays the
//     winner's row instead of erroring — the same pattern as
//     bookings.idempotentCreate.
//
// The acquirer call happens OUTSIDE any database transaction (hard rule: an
// external call never runs inside a DB transaction) — only the local insert +
// outbox event are transactional.
func (u *createUseCase) CreateForBooking(ctx context.Context, actor Actor, in CreateInput) (*domain.Payment, error) {
	if in.BookingID == uuid.Nil {
		return nil, fmt.Errorf("%w: booking required", domain.ErrValidation)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, fmt.Errorf("%w: idempotency key required", domain.ErrValidation)
	}

	booking, err := u.bookings.GetByID(ctx, in.BookingID)
	if err != nil {
		return nil, err
	}
	if err := authorizeCreate(ctx, u.managers, actor, booking); err != nil {
		return nil, err
	}
	if booking.Status != domain.BookingPending && booking.Status != domain.BookingConfirmed {
		return nil, fmt.Errorf("%w: booking is %s, no payment can be taken", domain.ErrValidation, booking.Status)
	}

	override, err := u.restaurants.GetPaymentOverride(ctx, booking.RestaurantID)
	if err != nil {
		return nil, err
	}
	settings := resolveSettings(override, u.cfg)
	if !settings.Enabled {
		return nil, fmt.Errorf("%w: payments are not enabled for this restaurant", domain.ErrValidation)
	}

	purpose, base, err := u.resolveAmount(ctx, *booking, settings)
	if err != nil {
		return nil, err
	}
	// Gross up so the venue nets the full base after the acquirer withholds its
	// cut of the total (ServiceFeeBps is that acquirer rate). A plain additive
	// markup would leave the venue short; see domain.GrossUpForAcquirer.
	fee, total, err := domain.GrossUpForAcquirer(base, settings.ServiceFeeBps)
	if err != nil {
		return nil, err
	}

	gw, err := u.gateways.Resolve(ctx, settings.Provider)
	if err != nil {
		return nil, err
	}
	provider := gw.Name()

	// Scoped to the booking AND the actor (report item, minor): scoping to
	// the booking alone caught a collision across two different bookings,
	// but not across two different ACTORS on the SAME booking (e.g. venue
	// staff creating a payment link, and the guest paying independently) who
	// happen to pick the same client-chosen idempotency string — that used
	// to silently collapse into one payment, replaying whichever actor's
	// call landed first as if it were the other's. actorKey makes that an
	// explicit, distinct key instead.
	dbKey := in.BookingID.String() + ":" + actorKey(actor) + ":" + in.IdempotencyKey

	if existing, err := u.replay(ctx, provider, dbKey, in.BookingID); err != nil || existing != nil {
		return existing, err
	}
	if _, err := u.payments.GetLiveByBookingID(ctx, in.BookingID); err == nil {
		return nil, fmt.Errorf("%w: this booking already has an active payment", domain.ErrAlreadyExists)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	paymentID := uuid.New()
	now := time.Now()
	expiresAt := now.Add(u.cfg.HoldTTL)

	gwResp, err := gw.Authorize(ctx, domain.AuthorizeRequest{
		PaymentID:      paymentID,
		BookingID:      in.BookingID,
		IdempotencyKey: dbKey,
		Amount:         total,
		Purpose:        purpose,
		Description:    descriptionFor(purpose),
		HoldTTL:        u.cfg.HoldTTL,
		ReturnURL:      in.ReturnURL,
		CallbackURL:    in.CallbackURL,
		CustomerPhone:  booking.PhoneNormalized,
		CustomerEmail:  booking.Email,
	})
	if err != nil {
		return nil, fmt.Errorf("authorize with %s: %w", provider, err)
	}

	p := &domain.Payment{
		ID: paymentID, BookingID: in.BookingID, RestaurantID: booking.RestaurantID, UserID: booking.UserID,
		Provider: provider, ProviderPaymentID: nullableStr(gwResp.ProviderPaymentID), Purpose: purpose,
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
			// Lost the race to insert: a concurrent identical retry won. The
			// acquirer resolved both Authorize calls to the same hold (same
			// idempotency key), so replaying the winner's row is correct —
			// nothing was double-charged, only the local bookkeeping raced.
			if existing, rerr := u.payments.GetByIdempotencyKey(ctx, provider, dbKey); rerr == nil {
				return existing, nil
			}
		}
		return nil, err
	}

	logging.FromContext(ctx).Info(logging.EventPaymentCreated,
		slog.String("payment_id", p.ID.String()),
		slog.String("booking_id", p.BookingID.String()),
		slog.String("provider", string(p.Provider)),
		slog.String("purpose", string(p.Purpose)),
		slog.Int64("amount_minor", p.AmountMinor),
	)
	return p, nil
}

// replay returns the stored payment for dbKey when it exists, nil otherwise.
// A hit for a DIFFERENT booking is a client bug — the key is scoped per
// booking so this should be unreachable, but a defensive check costs nothing.
func (u *createUseCase) replay(ctx context.Context, provider domain.PaymentProvider, dbKey string, bookingID uuid.UUID) (*domain.Payment, error) {
	existing, err := u.payments.GetByIdempotencyKey(ctx, provider, dbKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if existing.BookingID != bookingID {
		return nil, fmt.Errorf("%w: this idempotency key was used for a different booking", domain.ErrAlreadyExists)
	}
	return existing, nil
}

// resolveAmount decides what the guest owes: pre-ordered items when the venue
// requires pre-payment for them, otherwise the venue's deposit. A booking with
// neither is not payable — ErrValidation, not a silent zero-amount payment.
func (u *createUseCase) resolveAmount(ctx context.Context, b domain.Booking, settings domain.PaymentSettings) (domain.PaymentPurpose, domain.Money, error) {
	if settings.PreorderPaymentRequired {
		items, err := u.items.ListByBooking(ctx, b.ID)
		if err != nil {
			return "", domain.Money{}, err
		}
		var total int64
		for _, it := range items {
			if it.Status == domain.BookingItemCancelled {
				continue
			}
			total += it.TotalMinor()
		}
		if total > 0 {
			m, err := domain.NewMoney(total, domain.CurrencyKZT)
			return domain.PurposePreorder, m, err
		}
	}
	if settings.DepositRequired {
		m, err := domain.NewMoney(settings.DepositAmountMinor, domain.CurrencyKZT)
		if err != nil {
			return "", domain.Money{}, err
		}
		// Non-blocking item #6 (second review): domain.NewMoney only rejects
		// a NEGATIVE amount — zero is, by design, a valid Money value
		// elsewhere in this domain (e.g. a zero service fee at 0 bps). It is
		// NOT valid here: DepositRequired with a misconfigured or unset
		// DepositAmountMinor (restaurant override left at its NULL/default
		// and the global env default never set) would silently create a
		// real payment row, place a real (zero-amount) hold at the acquirer,
		// and let the booking proceed as if a deposit had been taken.
		// Reject explicitly instead of trusting "deposit required" to imply
		// "deposit amount is sane".
		if m.IsZero() {
			return "", domain.Money{}, fmt.Errorf(
				"%w: this restaurant requires a deposit but its configured deposit amount is zero — payment settings are misconfigured",
				domain.ErrValidation)
		}
		return domain.PurposeDeposit, m, nil
	}
	return "", domain.Money{}, fmt.Errorf("%w: this booking requires no payment", domain.ErrValidation)
}

// authorizeCreate decides who may start a payment for a booking: the venue's
// own staff (creating a payment link on a guest's behalf, scoped to their OWN
// restaurant — report item #13), the booking's owner, or — for a guest
// booking with no account — anyone who reached this call with the booking id
// (the transport layer only exposes it after the booking's own contact
// verification; there is no separate account to check ownership against).
func authorizeCreate(ctx context.Context, managers managerChecker, actor Actor, b *domain.Booking) error {
	if actor.staff() {
		return authorizeStaffForRestaurant(ctx, managers, actor, b.RestaurantID)
	}
	if b.UserID == nil {
		return nil
	}
	if !actor.isUser(b.UserID) {
		return fmt.Errorf("%w: booking belongs to another guest", domain.ErrForbidden)
	}
	return nil
}

// actorKey is a stable, distinct discriminator for the idempotency-key scope
// (report item, minor): a STAFF actor is keyed by their own user id — venue
// staff creating a payment link is a genuinely different flow from the guest
// paying independently, and the two must never collapse into one payment
// just because they picked the same client-chosen idempotency string.
//
// Every non-staff actor collapses into one shared "guest" bucket per booking,
// REGARDLESS of whether they are logged in (report item #5, second review):
// spec §6 only ever has one guest (with or without an account) per booking,
// so there is no further identity to distinguish between them, and — this is
// the part the previous version of this function got wrong — keying by
// UserID meant the SAME physical client retrying a checkout first
// anonymously and then, having logged in mid-flow, again as an authenticated
// user produced TWO DIFFERENT dbKeys. GetLiveByBookingID only reports
// authorized/captured payments as "live" (a `created` payment is deliberately
// not live, see idx_payments_live_per_booking's comment — a guest may abandon
// a checkout), so a second call with a different actorKey arriving before the
// first hold's webhook confirmation would sail past both the idempotency
// replay AND the live-payment check and authorize a second hold. Keying every
// non-staff actor identically closes that window without touching the
// intentional "created is not live" design.
func actorKey(actor Actor) string {
	if actor.staff() {
		// RoleAdmin may act without its own UserID being relevant to THIS
		// check (authorizeStaffForRestaurant admits any admin regardless);
		// guard the nil case rather than assume every staff actor carries one.
		if actor.UserID != nil {
			return "staff:" + actor.UserID.String()
		}
		return "staff"
	}
	return "guest"
}

// descriptionFor is the guest-facing payment description. Service wording
// only (spec §3, §9.4): never "card fee" / "acquiring".
func descriptionFor(purpose domain.PaymentPurpose) string {
	switch purpose {
	case domain.PurposePreorder:
		return "BookEat: предзаказ и сервисный сбор"
	default:
		return "BookEat: депозит за бронь и сервисный сбор"
	}
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

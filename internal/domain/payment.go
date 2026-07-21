package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PaymentStatus is the lifecycle state of a payment, stored as VARCHAR.
type PaymentStatus string

const (
	// PaymentCreated is the intent: a row exists, the acquirer has not been
	// called yet or has not answered. No money is involved.
	PaymentCreated PaymentStatus = "created"
	// PaymentAuthorized is a hold: the guest's funds are blocked but not taken.
	PaymentAuthorized PaymentStatus = "authorized"
	// PaymentCaptured is money actually taken from the guest.
	PaymentCaptured PaymentStatus = "captured"
	// PaymentVoided is a released hold — the guest was never charged.
	PaymentVoided PaymentStatus = "voided"
	// PaymentPartiallyRefunded means part of a captured payment went back.
	PaymentPartiallyRefunded PaymentStatus = "partially_refunded"
	// PaymentRefunded means the whole captured amount went back.
	PaymentRefunded PaymentStatus = "refunded"
	// PaymentFailed is a rejection by the acquirer or by the issuer.
	PaymentFailed PaymentStatus = "failed"
	// PaymentExpired is a hold the acquirer let lapse before we captured it.
	PaymentExpired PaymentStatus = "expired"
)

// paymentTransitions is the allowed payment status transition table. A status
// present with an empty set is valid but terminal.
//
//	created ──authorize──▶ authorized ──capture──▶ captured
//	   │                       │                      │
//	   │                       ├──void──▶ voided      ├──refund(part)──▶ partially_refunded
//	   │                       ├──expire──▶ expired   └──refund(full)──▶ refunded
//	   └──fail──▶ failed       └──fail──▶ failed      partially_refunded ──▶ refunded
//
// Notes that are easy to get wrong:
//   - authorized → failed exists because an acquirer can reject a capture on an
//     otherwise valid hold (issuer declines, hold already consumed);
//   - a second partial refund does NOT change the status, so it is not a
//     transition at all — the usecase only calls ValidatePaymentTransition when
//     the status actually changes;
//   - there is no way back into `created` and no way out of `voided` /
//     `expired` / `failed` / `refunded`: money that was never taken cannot be
//     refunded, and money already returned cannot be returned twice.
var paymentTransitions = map[PaymentStatus]map[PaymentStatus]struct{}{
	PaymentCreated: {
		PaymentAuthorized: {},
		PaymentFailed:     {},
		PaymentExpired:    {},
	},
	PaymentAuthorized: {
		PaymentCaptured: {},
		PaymentVoided:   {},
		PaymentExpired:  {},
		PaymentFailed:   {},
	},
	PaymentCaptured: {
		PaymentPartiallyRefunded: {},
		PaymentRefunded:          {},
	},
	PaymentPartiallyRefunded: {
		PaymentRefunded: {},
	},
	PaymentVoided:   {},
	PaymentExpired:  {},
	PaymentFailed:   {},
	PaymentRefunded: {},
}

// Valid reports whether s is a known payment status.
func (s PaymentStatus) Valid() bool {
	_, ok := paymentTransitions[s]
	return ok
}

// Terminal reports whether no further transition is allowed from s.
func (s PaymentStatus) Terminal() bool { return len(paymentTransitions[s]) == 0 }

// HoldsMoney reports whether a payment in this status is holding or has taken
// the guest's money. Used to decide whether a cancellation has to reach the
// acquirer at all, and mirrors the partial unique index
// idx_payments_live_per_booking in migrations/0007_payments.sql — keep both in
// sync.
func (s PaymentStatus) HoldsMoney() bool {
	return s == PaymentAuthorized || s == PaymentCaptured
}

// Refundable reports whether money can still be sent back for this status.
func (s PaymentStatus) Refundable() bool {
	return s == PaymentCaptured || s == PaymentPartiallyRefunded
}

// CanPaymentTransition reports whether from → to is an allowed payment status
// transition.
func CanPaymentTransition(from, to PaymentStatus) bool {
	_, ok := paymentTransitions[from][to]
	return ok
}

// ValidatePaymentTransition returns nil when from → to is allowed,
// ErrValidation when either status is unknown, and ErrInvalidStatus otherwise
// (mapped to HTTP 422).
func ValidatePaymentTransition(from, to PaymentStatus) error {
	if !from.Valid() || !to.Valid() {
		return ErrValidation
	}
	if !CanPaymentTransition(from, to) {
		return ErrInvalidStatus
	}
	return nil
}

// PaymentPurpose says what the guest is paying for, stored as VARCHAR.
type PaymentPurpose string

const (
	// PurposeDeposit is a deposit that guarantees the table.
	PurposeDeposit PaymentPurpose = "deposit"
	// PurposePreorder is payment for pre-ordered menu items.
	PurposePreorder PaymentPurpose = "preorder"
)

// Valid reports whether p is a known payment purpose.
func (p PaymentPurpose) Valid() bool {
	return p == PurposeDeposit || p == PurposePreorder
}

// Payment is one attempt to take money for a booking. RestaurantID and UserID
// are denormalised out of the booking on purpose: a payment is a financial
// record and must stay readable long after anything else moves.
//
// AmountMinor always equals BaseAmountMinor + FeeMinor; the database enforces
// it too (chk_payments_amount_split). The server is the only party that ever
// computes these numbers (spec §8).
type Payment struct {
	ID                uuid.UUID
	BookingID         uuid.UUID
	RestaurantID      uuid.UUID
	UserID            *uuid.UUID // nil = guest checkout without an account
	Provider          PaymentProvider
	ProviderPaymentID *string // nil until the acquirer answers Authorize
	Purpose           PaymentPurpose
	Status            PaymentStatus
	AmountMinor       int64 // total charged to the guest
	BaseAmountMinor   int64 // deposit / pre-order, without the service fee
	FeeMinor          int64 // BookEat service fee
	Currency          Currency
	IdempotencyKey    string // the key we hand to the acquirer
	PaymentURL        *string
	AuthorizedAt      *time.Time
	CapturedAt        *time.Time
	VoidedAt          *time.Time
	FailedAt          *time.Time
	ExpiresAt         *time.Time // when the hold lapses
	FailureCode       *string
	FailureMessage    *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Total returns the full amount charged to the guest.
func (p Payment) Total() Money { return Money{AmountMinor: p.AmountMinor, Currency: p.Currency} }

// Base returns the deposit / pre-order part, without the service fee.
func (p Payment) Base() Money { return Money{AmountMinor: p.BaseAmountMinor, Currency: p.Currency} }

// Fee returns the BookEat service fee part.
func (p Payment) Fee() Money { return Money{AmountMinor: p.FeeMinor, Currency: p.Currency} }

// PaymentFilter narrows a payment listing. Zero-value fields are ignored.
type PaymentFilter struct {
	BookingID    *uuid.UUID
	RestaurantID *uuid.UUID
	UserID       *uuid.UUID
	Statuses     []PaymentStatus
	From         *time.Time // created_at >= From
	To           *time.Time // created_at <  To
	Page         int        // 1-based; <=0 means 1
	PerPage      int        // <=0 means default (20), capped at 100
}

// PaymentRepository persists payments. Get* return ErrNotFound when absent.
type PaymentRepository interface {
	Create(ctx context.Context, p *Payment) error
	Update(ctx context.Context, p *Payment) error
	GetByID(ctx context.Context, id uuid.UUID) (*Payment, error)
	// GetByProviderPaymentID resolves an acquirer-side id. Webhooks use it and
	// must NOT create anything when it returns ErrNotFound (spec §7).
	GetByProviderPaymentID(ctx context.Context, provider PaymentProvider, providerPaymentID string) (*Payment, error)
	// GetLiveByBookingID returns the booking's authorized or captured payment,
	// backed by idx_payments_live_per_booking.
	GetLiveByBookingID(ctx context.Context, bookingID uuid.UUID) (*Payment, error)
	// List returns payments matching f plus the total count, newest first.
	List(ctx context.Context, f PaymentFilter) ([]Payment, int, error)
	// UpdateStatus writes the new status and its timestamp column. Call inside
	// a TxManager together with the ledger and outbox inserts — a status change
	// that is not accompanied by both is a hole in the audit trail.
	UpdateStatus(ctx context.Context, id uuid.UUID, status PaymentStatus, at time.Time) error
	// ClaimStale locks up to limit payments in the given statuses whose
	// created_at is older than before, using FOR UPDATE SKIP LOCKED, oldest
	// first. This is the reconciliation worker's input: a webhook may never
	// arrive, and money must not be lost to a lost HTTP request (spec §5).
	ClaimStale(ctx context.Context, statuses []PaymentStatus, before time.Time, limit int) ([]Payment, error)
}

// PaymentSettings is the resolved payment policy for one restaurant: the global
// env defaults with the venue's non-NULL overrides applied. Resolution lives in
// usecase/payments; this type only carries the result.
type PaymentSettings struct {
	Enabled                 bool
	DepositRequired         bool
	DepositAmountMinor      int64
	PreorderPaymentRequired bool
	ServiceFeeBps           int             // 350 = 3.5%
	Provider                PaymentProvider // must be an enabled one, else the default
}

// PaymentSettingsOverride is a restaurant's optional per-field override of the
// global payment settings. A nil field means "use the global default".
type PaymentSettingsOverride struct {
	PaymentsEnabled         *bool
	DepositRequired         *bool
	DepositAmountMinor      *int64
	PreorderPaymentRequired *bool
	ServiceFeeBps           *int
	Provider                *PaymentProvider
}

// ---------------------------------------------------------------------------
// PaymentGateway — the acquirer port (spec §2)
// ---------------------------------------------------------------------------

// PaymentGateway is the only thing the domain knows about acquirers. FreedomPay
// and TipTopPay are adapters behind it (infrastructure/payment/...); adding a
// third acquirer must not require touching a single line in this package.
//
// Every implementation must be an anti-corruption layer: provider status codes,
// error codes and payloads are translated into the domain types below and never
// leak outward.
//
// All calls carry our own idempotency key, so a retry after a timeout resolves
// to the same payment instead of creating a second one (spec §8).
type PaymentGateway interface {
	// Authorize places a hold. It never captures — the two-stage flow is
	// mandatory (spec §2).
	Authorize(ctx context.Context, req AuthorizeRequest) (*GatewayPayment, error)
	// Capture takes an amount up to the held one. Partial capture is allowed
	// (e.g. a pre-order line the venue could not serve).
	Capture(ctx context.Context, providerPaymentID string, amount Money) (*GatewayPayment, error)
	// Void releases a hold that was never captured.
	Void(ctx context.Context, providerPaymentID string) error
	// Refund sends money back from a captured payment. Refunding more than the
	// remainder is rejected before this is ever called.
	Refund(ctx context.Context, providerPaymentID string, amount Money) (*GatewayRefund, error)
	// Get reads the acquirer's own view of a payment. This is the
	// reconciliation path: the webhook is the primary signal, never the only
	// one.
	Get(ctx context.Context, providerPaymentID string) (*GatewayPayment, error)
	// VerifyWebhook validates the signature and translates the raw body into a
	// domain event. It must do the signature check FIRST and return an error
	// without interpreting an unverified payload (spec §7).
	VerifyWebhook(raw []byte, headers map[string]string) (*WebhookEvent, error)
	// Name is the provider code this adapter serves.
	Name() PaymentProvider
}

// AuthorizeRequest is everything an acquirer needs to place a hold, expressed
// in domain terms only. It carries no provider-specific field: anything an
// acquirer needs beyond this (merchant ids, terminal codes, secrets) comes from
// that adapter's own env configuration, never from here.
type AuthorizeRequest struct {
	PaymentID      uuid.UUID
	BookingID      uuid.UUID
	IdempotencyKey string
	Amount         Money // total, fee included
	Purpose        PaymentPurpose
	Description    string        // shown to the guest; service wording only (spec §9.4)
	HoldTTL        time.Duration // zero = the acquirer's own default
	ReturnURL      string        // where the guest lands after the payment page
	CallbackURL    string        // our webhook endpoint for this provider
	CustomerPhone  string        // E.164
	CustomerEmail  string
	// Metadata is passed through to the acquirer and echoed back in webhooks.
	// It must never contain card data or anything secret (spec §8).
	Metadata map[string]string
}

// GatewayPayment is the acquirer's view of a payment, already translated into
// domain types by the adapter. Raw keeps the original payload for the audit
// trail; it must be free of card data before it gets here.
type GatewayPayment struct {
	ProviderPaymentID string
	Status            PaymentStatus
	Amount            Money
	PaymentURL        string // where to send the guest, when the flow needs it
	AuthorizedAt      *time.Time
	CapturedAt        *time.Time
	ExpiresAt         *time.Time
	FailureCode       string
	FailureMessage    string
	Raw               json.RawMessage
}

// GatewayRefund is the acquirer's view of a refund.
type GatewayRefund struct {
	ProviderRefundID string
	Status           RefundStatus
	Amount           Money
	FailureCode      string
	FailureMessage   string
	Raw              json.RawMessage
}

// WebhookEventType is the normalised meaning of an acquirer callback. Provider
// event names are mapped onto this closed set by the adapter.
type WebhookEventType string

const (
	WebhookPaymentAuthorized WebhookEventType = "payment.authorized"
	WebhookPaymentCaptured   WebhookEventType = "payment.captured"
	WebhookPaymentVoided     WebhookEventType = "payment.voided"
	WebhookPaymentFailed     WebhookEventType = "payment.failed"
	WebhookPaymentExpired    WebhookEventType = "payment.expired"
	WebhookRefundSucceeded   WebhookEventType = "refund.succeeded"
	WebhookRefundFailed      WebhookEventType = "refund.failed"
	// WebhookUnknown is a callback we recognise as authentic but do not act on.
	// It is still stored (payment_events) — silence about a signed message from
	// an acquirer is how money goes missing unnoticed.
	WebhookUnknown WebhookEventType = "unknown"
)

// WebhookEvent is a verified acquirer callback in domain terms. SignatureValid
// is carried explicitly rather than implied: a failed verification is still
// recorded as evidence in payment_events (spec §7).
type WebhookEvent struct {
	Provider          PaymentProvider
	ProviderEventID   string
	ProviderPaymentID string
	// MerchantPaymentID is OUR payment id, echoed back by the acquirer
	// (TipTopPay `InvoiceId`, FreedomPay `pg_order_id`). It exists because the
	// acquirer-side id is not always known when the guest starts paying — a
	// hosted payment page is created first and only produces a transaction id
	// once the card is charged. Resolving a callback by our own id is what
	// makes "a webhook never creates a payment from thin air" (spec §7)
	// implementable for both providers.
	MerchantPaymentID string
	ProviderRefundID  string
	Type              WebhookEventType
	Status            PaymentStatus
	Amount            Money
	OccurredAt        time.Time
	SignatureValid    bool
	FailureCode       string
	FailureMessage    string
	Payload           json.RawMessage
}

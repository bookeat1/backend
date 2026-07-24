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
	// PaymentCapturing is a claimed-but-not-yet-confirmed capture attempt: the
	// venue seated the guest and this usecase call won the CAS claim on the
	// hold, but the acquirer has not answered yet. It exists purely to make
	// two concurrent CaptureOnSeating calls for the SAME booking race-safe
	// (report item #5): only the CAS winner may call the acquirer; the loser
	// gets ErrAlreadyExists and must not call Capture too. It is always
	// transient — CaptureOnSeating resolves it to `captured` on a definite
	// acquirer answer (success or an explicit decline, domain.ErrProviderDeclined)
	// but, per the second review (item #1), is deliberately LEFT here when the
	// acquirer's answer is unknown (domain.ErrProviderOutcomeUnknown) — see
	// the KNOWN GAP note on PaymentVoiding below, which applies here too.
	PaymentCapturing PaymentStatus = "capturing"
	// PaymentCaptured is money actually taken from the guest.
	PaymentCaptured PaymentStatus = "captured"
	// PaymentVoiding is a claimed-but-not-yet-confirmed hold release: the venue
	// rejected the seating and this usecase call won the CAS claim on the
	// hold, but the acquirer has not answered yet. Symmetric to
	// PaymentCapturing and exists for the same reason (payments review
	// 2026-07-23, non-blocking item #1): two concurrent VoidOnRejection calls
	// for the SAME booking must not both call gw.Void — only the CAS winner
	// may call the acquirer. It is always transient — VoidOnRejection
	// resolves it to `voided` on acquirer success or releases it back to
	// `authorized` on a definite acquirer decline, never leaving a payment
	// parked here on purpose.
	//
	// KNOWN GAP: neither this nor PaymentCapturing has an automatic way out
	// when the acquirer's answer is genuinely unknown (a timeout / 5xx) — the
	// payment is deliberately left here pending manual or
	// reconciliation-worker resolution. The reconciliation worker is a
	// separate, not-yet-built task (see the payments review report); DO NOT
	// run this code in production before it exists, or a lost acquirer answer
	// strands a payment here with no automatic recovery.
	PaymentVoiding PaymentStatus = "voiding"
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
//	created ──authorize──▶ authorized ──capture──▶ capturing ──▶ captured
//	   │                       │                                    │
//	   │                       ├──void──▶ voiding ──▶ voided        ├──refund(part)──▶ partially_refunded
//	   │                       ├──expire──▶ expired                 └──refund(full)──▶ refunded
//	   └──fail──▶ failed       └──fail──▶ failed      partially_refunded ──▶ refunded
//
// Notes that are easy to get wrong:
//   - authorized → failed exists because an acquirer can reject a capture on an
//     otherwise valid hold (issuer declines, hold already consumed);
//   - authorized → captured and authorized → voided (skipping the
//     capturing/voiding claim) exist because a webhook may report the
//     acquirer's own outcome directly, without ever going through this
//     usecase's local claim;
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
		PaymentCapturing: {},
		PaymentCaptured:  {}, // a webhook may report capture directly, without going through the local `capturing` claim
		PaymentVoiding:   {},
		PaymentVoided:    {}, // a webhook may report void directly, without going through the local `voiding` claim
		PaymentExpired:   {},
		PaymentFailed:    {},
	},
	PaymentCapturing: {
		PaymentCaptured:   {}, // acquirer gave a definite answer: confirmed
		PaymentAuthorized: {}, // acquirer declined outright: release the claim, the hold is unchanged
		// NOTE: there is deliberately no transition OUT of `capturing` for an
		// UNKNOWN acquirer outcome (report item #1) — the usecase simply does
		// not call CompareAndSwapStatus in that case, so the payment stays
		// here until a human or the reconciliation worker resolves it.
	},
	PaymentVoiding: {
		PaymentVoided:     {}, // acquirer confirmed the release
		PaymentAuthorized: {}, // acquirer declined outright: release the claim, the hold is unchanged
		// Same NOTE as PaymentCapturing above applies symmetrically.
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
// idx_payments_live_per_booking (as recreated by migrations/0009, on top of
// 0007/0008) — keep both in sync. PaymentVoiding counts as holding money too:
// the hold is still in place until the acquirer confirms its release, so a
// second payment must not be allowed to start for the same booking while one
// is mid-void, exactly like mid-capture.
func (s PaymentStatus) HoldsMoney() bool {
	return s == PaymentAuthorized || s == PaymentCapturing || s == PaymentVoiding || s == PaymentCaptured
}

// Refundable reports whether money can still be sent back for this status.
func (s PaymentStatus) Refundable() bool {
	return s == PaymentCaptured || s == PaymentPartiallyRefunded
}

// SettleResolvable reports whether RefundUseCase.Settle should be able to
// find a payment in this status for its booking at all. This is
// deliberately WIDER than HoldsMoney: Settle must resolve both a payment it
// may act on for the first time (HoldsMoney — captured is the only one it
// will actually accept, the rest it rejects with a precise "invalid status"
// error) AND a payment it has already fully or partially settled (refunded /
// partially_refunded), so that a retried Settle call — same booking, same
// idempotency key — finds its own past result instead of a bare "not found".
// See GetSettleableByBookingID and RefundUseCase's doc comment.
func (s PaymentStatus) SettleResolvable() bool {
	return s.HoldsMoney() || s == PaymentRefunded || s == PaymentPartiallyRefunded
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
	// SettledAt is the terminal settlement marker (report item #7): it is set
	// exactly once, by RefundUseCase.Settle, regardless of which outcome fired
	// (a guest refund, a venue-cancel refund, or a late-cancellation/no-show
	// that moves no money and leaves Status == captured). Its purpose is
	// narrow but critical: a late-cancellation/no-show settlement legitimately
	// does NOT change Status away from captured (see the deviations note), so
	// Status alone cannot answer "has this payment already been settled?" —
	// without this field a second Settle call with a different trigger and a
	// different idempotency key would sail through the `Status == captured`
	// check and refund the guest a second time on top of what the venue
	// already kept. SettledAt is the CAS anchor for PaymentRepository.
	// ClaimSettlement: nil means "not yet settled", set means "settled, and
	// SettlementIdempotencyKey says which request settled it".
	SettledAt *time.Time
	// SettledTrigger records which outcome fired, for audit.
	SettledTrigger *RefundTrigger
	// SettlementIdempotencyKey is the key of the Settle call that won the
	// claim, so a legitimate retry (same key) can be told apart from a second,
	// different settlement attempt (different key) after SettledAt is already
	// non-nil.
	SettlementIdempotencyKey *string
	// StatusChangedAt is the lease clock the reconciliation worker relies on
	// (migration 0010): stamped every time Status actually changes, by
	// UpdateStatus and CompareAndSwapStatus alike. created_at cannot serve
	// this purpose — a payment that has sat in `authorized` for days and only
	// just moved into `capturing` looks identical to one stuck in `capturing`
	// for days, if the only clock available is created_at. A transient state
	// (capturing/voiding) only counts as "stuck" once it has held that status
	// longer than the worker's configured threshold, measured from here.
	StatusChangedAt time.Time
	// ReconcileAttempts counts consecutive times the reconciliation worker
	// asked the acquirer about this payment and got back an outcome it could
	// not act on (domain.ErrProviderOutcomeUnknown, a transport error, or an
	// acquirer status this build does not recognise). It is reset to 0 by any
	// real CompareAndSwapStatus/UpdateStatus transition — a payment that
	// moved on its own no longer needs the count that was building up while
	// it was stuck.
	ReconcileAttempts int
	// LastReconcileAttemptAt is when ReconcileAttempts was last bumped. Used
	// to back off exponentially between attempts instead of re-asking the
	// acquirer every single tick for a payment that keeps coming back
	// unknown.
	LastReconcileAttemptAt *time.Time
	// NeedsManualReview is set once ReconcileAttempts reaches the worker's
	// configured maximum. It is a terminal flag for the WORKER, not for the
	// payment's own state machine: the worker stops calling the acquirer for
	// this row (avalanche protection) and only logs it once per tick, until a
	// human resolves it out of band. It is cleared automatically the moment a
	// real status transition succeeds (same reset as ReconcileAttempts).
	NeedsManualReview bool
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
	// backed by idx_payments_live_per_booking. Used to decide whether a NEW
	// payment may still be authorized for this booking (CreateForBooking) —
	// a refunded/voided/failed payment must NOT count as "still live" there.
	GetLiveByBookingID(ctx context.Context, bookingID uuid.UUID) (*Payment, error)
	// GetSettleableByBookingID returns the booking's payment in any status
	// PaymentStatus.SettleResolvable reports true for — i.e. it also finds an
	// already fully/partially refunded payment, which GetLiveByBookingID
	// does not. RefundUseCase.Settle uses this (not GetLiveByBookingID) as
	// its very first lookup specifically so a retried Settle call for the
	// "full refund" outcome resumes idempotently instead of 404ing once the
	// payment has moved to `refunded` (see RefundUseCase's doc comment).
	GetSettleableByBookingID(ctx context.Context, bookingID uuid.UUID) (*Payment, error)
	// GetByIdempotencyKey resolves our own retry token, backed by
	// idx_payments_idempotency (UNIQUE (provider, idempotency_key)). This is
	// what makes "create payment" idempotent (usecase/payments): a retry with
	// the same key looks its own row up here and replays it instead of
	// authorizing a second hold. Returns ErrNotFound when unused.
	GetByIdempotencyKey(ctx context.Context, provider PaymentProvider, idempotencyKey string) (*Payment, error)
	// List returns payments matching f plus the total count, newest first.
	List(ctx context.Context, f PaymentFilter) ([]Payment, int, error)
	// UpdateStatus writes the new status and its timestamp column. Call inside
	// a TxManager together with the ledger and outbox inserts — a status change
	// that is not accompanied by both is a hole in the audit trail. Prefer
	// CompareAndSwapStatus for any transition a concurrent request could also
	// be making; UpdateStatus alone is a blind write.
	UpdateStatus(ctx context.Context, id uuid.UUID, status PaymentStatus, at time.Time) error
	// CompareAndSwapStatus is UpdateStatus with a precondition on the CURRENT
	// status, implemented as a single `UPDATE payments SET status = $to, ...
	// WHERE id = $id AND status = $from` — the database-level guard the team
	// convention requires for anything that moves money ("unique constraints
	// instead of мы проверили перед вставкой"). It returns ErrAlreadyExists
	// when zero rows matched, which covers two cases the caller must treat
	// identically: a concurrent transition already moved the row away from
	// `from`, or another payment for the same booking already won
	// idx_payments_live_per_booking. Both mean "this transition lost the race"
	// — the caller compensates (e.g. Void the loser's hold), it never retries
	// blindly.
	CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to PaymentStatus, at time.Time) error
	// ClaimSettlement is the CAS anchor behind Payment.SettledAt: a single
	// `UPDATE payments SET settled_at = $at, settled_trigger = $trigger,
	// settlement_idempotency_key = $key WHERE id = $id AND settled_at IS
	// NULL`. It returns ErrAlreadyExists when zero rows matched — the payment
	// was already settled, by this same request (replay, same key: read the
	// stored row and treat as success) or by a different one (a real conflict,
	// report item #7).
	ClaimSettlement(ctx context.Context, id uuid.UUID, idempotencyKey string, trigger RefundTrigger, at time.Time) error
	// ClaimStale selects up to limit payments in the given statuses whose
	// StatusChangedAt is older than before, oldest first. This is the
	// reconciliation worker's input (usecase/payments.Reconciler): a webhook
	// may never arrive, or a process may die between claiming a transient
	// state (capturing/voiding) and resolving it, and money must not be lost
	// to a lost HTTP request or a lost process (spec §5).
	//
	// A real Postgres implementation may use FOR UPDATE SKIP LOCKED to avoid
	// two worker instances doing duplicate acquirer reads for the same row in
	// the same instant, but MUST NOT hold that lock (or any transaction)
	// across the acquirer call the worker makes next — the hard rule that an
	// external call never runs inside a DB transaction applies here exactly
	// like everywhere else in this package. Correctness therefore does not
	// come from this lock: it comes from every write the worker makes
	// afterwards being CAS-guarded (CompareAndSwapStatus / RecordReconcileAttempt),
	// so two callers racing on the same stale row both may read, but only one
	// may write.
	ClaimStale(ctx context.Context, statuses []PaymentStatus, before time.Time, limit int) ([]Payment, error)
	// ClaimExpiredHolds selects up to limit `authorized` payments whose
	// ExpiresAt is before the given time, oldest first (backed by
	// idx_payments_expires from migration 0007). Same non-locking-across-the-
	// acquirer-call caveat as ClaimStale.
	ClaimExpiredHolds(ctx context.Context, before time.Time, limit int) ([]Payment, error)
	// RecordReconcileAttempt is the CAS-guarded write behind ReconcileAttempts
	// / LastReconcileAttemptAt / NeedsManualReview (migration 0010): a single
	// `UPDATE payments SET reconcile_attempts = reconcile_attempts + 1,
	// last_reconcile_attempt_at = $at,
	// needs_manual_review = (reconcile_attempts + 1 >= $maxAttempts)
	// WHERE id = $id AND status = $expectedStatus RETURNING reconcile_attempts,
	// needs_manual_review`. It returns ErrAlreadyExists when zero rows
	// matched — the payment's status already moved away from expectedStatus
	// (resolved by a webhook, another worker pass, or a direct call) between
	// the worker's read and this write, so there is nothing to bump; the
	// caller treats that as "already resolved", not as a reconciliation
	// failure.
	RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus PaymentStatus, at time.Time, maxAttempts int) (attempts int, needsManualReview bool, err error)
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
	// FreeCancelWindow is the per-restaurant free-cancellation window used by
	// the MONEY path (migration 0034/0035, restaurants.free_cancel_window_minutes):
	// a deposit HOLD is released to the guest (voided) only when the booking is
	// cancelled EARLIER than this before starts_at; a later cancellation or a
	// no-show forfeits the deposit to the venue (the hold is captured). Always
	// present (the column is NOT NULL, owner-confirmed default 120m).
	FreeCancelWindow time.Duration
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
	// FreeCancelWindowMinutes overrides the money-path free-cancellation
	// window per restaurant (restaurants.free_cancel_window_minutes). Unlike
	// the other fields it maps to a NOT NULL column, so in practice it is
	// never nil once read from Postgres; it stays a pointer only to keep this
	// struct a uniform "nil = use the global default" override shape and so an
	// in-memory / test override can still say "inherit the default".
	FreeCancelWindowMinutes *int
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

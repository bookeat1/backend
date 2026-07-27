package domain

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Restaurant payouts (spec: restaurant settlement / выплаты заведениям)
//
// BookEat is the merchant of record: a guest's money lands in BookEat's
// acquirer account, and the venue's share is CREDITED to AccountRestaurant in
// the payment ledger at capture time (see payment_ledger.go). This file models
// paying that owed balance back out to the restaurant through an acquirer
// payout product (FreedomPay's "выплаты"), with the same money-safety
// discipline the rest of the payments code uses: DB-level CAS for every status
// change, an idempotency key so a retried send never double-pays, and a claim
// table (PayoutItem) that makes a ledger entry payable through AT MOST ONE
// live payout.
// ---------------------------------------------------------------------------

// PayoutStatus is the lifecycle state of a single restaurant payout, VARCHAR.
type PayoutStatus string

const (
	// PayoutPending is a computed-but-not-yet-sent payout: the owed ledger
	// entries have been claimed into it (PayoutItem rows) and the amount is
	// frozen, but no acquirer call has been made. No money has moved.
	PayoutPending PayoutStatus = "pending"
	// PayoutSent is the transient "claimed and dispatched, awaiting a definite
	// answer" state — the exact analogue of PaymentCapturing. SendPayout wins
	// the CAS pending→sent BEFORE it calls the acquirer, so a second concurrent
	// send finds the row already `sent` and never calls the acquirer twice. A
	// lost/timed-out acquirer answer leaves the payout HERE (never guessed as
	// paid); the payout reconciler resolves it later via GetPayout.
	PayoutSent PayoutStatus = "sent"
	// PayoutPaid is a confirmed successful payout — money reached the venue.
	PayoutPaid PayoutStatus = "paid"
	// PayoutFailed is a payout the acquirer definitively rejected. Its claimed
	// ledger entries are RELEASED (PayoutItem rows deleted) so the same money is
	// owed again and a later payout can re-claim it. A timeout/unknown outcome
	// is NOT a failure and never lands here.
	PayoutFailed PayoutStatus = "failed"
)

// payoutTransitions is the allowed payout status transition table. A status
// present with an empty set is terminal.
//
//	pending ──send──▶ sent ──confirm──▶ paid
//	   │               └──decline──▶ failed
//	   └──decline/cancel──▶ failed
var payoutTransitions = map[PayoutStatus]map[PayoutStatus]struct{}{
	PayoutPending: {
		PayoutSent:   {},
		PayoutFailed: {},
	},
	PayoutSent: {
		PayoutPaid:   {},
		PayoutFailed: {},
	},
	PayoutPaid:   {},
	PayoutFailed: {},
}

// Valid reports whether s is a known payout status.
func (s PayoutStatus) Valid() bool {
	_, ok := payoutTransitions[s]
	return ok
}

// Terminal reports whether no further transition is allowed from s.
func (s PayoutStatus) Terminal() bool { return len(payoutTransitions[s]) == 0 }

// CanPayoutTransition reports whether from → to is an allowed transition.
func CanPayoutTransition(from, to PayoutStatus) bool {
	_, ok := payoutTransitions[from][to]
	return ok
}

// ValidatePayoutTransition returns nil for an allowed transition, ErrValidation
// for an unknown status, ErrInvalidStatus otherwise.
func ValidatePayoutTransition(from, to PayoutStatus) error {
	if !from.Valid() || !to.Valid() {
		return ErrValidation
	}
	if !CanPayoutTransition(from, to) {
		return ErrInvalidStatus
	}
	return nil
}

// PayoutMethod is how a restaurant's money is delivered by the provider.
// Increment 1 supports a tokenized saved-card payout only: BookEat never stores
// a raw PAN (PCI) — only a provider-issued card TOKEN plus a masked identifier
// for display. An IBAN/bank-account method is a documented extension that needs
// its own requisite columns (IIN/KBe/KNP/BIK) and is intentionally out of scope
// here.
type PayoutMethod string

const (
	// PayoutMethodFreedomPayCardToken is a payout to a card the venue registered
	// with FreedomPay, addressed by the provider's card token (a UUID), never by
	// its PAN. See infrastructure/payment/freedompay: /g2g/reg2reg.
	PayoutMethodFreedomPayCardToken PayoutMethod = "freedompay_card_token"
)

// Valid reports whether m is a supported payout method.
func (m PayoutMethod) Valid() bool {
	return m == PayoutMethodFreedomPayCardToken
}

// rawPANPattern matches a bare 13–19 digit card number (optionally spaced or
// dashed). It exists to REJECT a raw PAN ever being stored as a token or a
// "masked" identifier — a hard PCI guard, not a formatting nicety.
var rawPANPattern = regexp.MustCompile(`^[\s-]*(?:\d[\s-]*){13,19}$`)

// looksLikeRawPAN reports whether s is a bare card number. Used to refuse
// storing one anywhere in a payout destination.
func looksLikeRawPAN(s string) bool {
	return rawPANPattern.MatchString(strings.TrimSpace(s))
}

// PayoutDestination is where a restaurant's owed money is sent. It holds a
// provider-issued TOKEN and a MASKED identifier only — never a raw card number.
// One live destination per restaurant (a new one replaces the old in place).
type PayoutDestination struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	Provider     PaymentProvider
	Method       PayoutMethod
	// Token is the provider's opaque handle for the payout instrument
	// (FreedomPay card token, a UUID). It is NOT a secret in the acquirer-key
	// sense, but it is the only address of the money, so it is treated with the
	// same care and never logged in full.
	Token string
	// ProviderCustomerRef is the merchant-side user id the token is registered
	// under at the provider (FreedomPay pg_user_id). FreedomPay's tokenized
	// payout (/g2g/reg2reg) addresses a saved card by (pg_user_id,
	// pg_card_token_to), so the token alone is not enough — this pairs with it.
	// Not a card number and not a secret. May be empty until tokenization is
	// wired; a live token payout requires it.
	ProviderCustomerRef string
	// MaskedIdentifier is a human-facing masked hint (e.g. "440043******1234").
	// Display only; never a full PAN.
	MaskedIdentifier string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Validate enforces the destination invariants, most importantly the PCI guard:
// no field may contain a raw PAN.
func (d PayoutDestination) Validate() error {
	if d.RestaurantID == uuid.Nil {
		return ErrValidation
	}
	if d.Provider != ProviderFreedomPay {
		return ErrValidation
	}
	if !d.Method.Valid() {
		return ErrValidation
	}
	token := strings.TrimSpace(d.Token)
	if token == "" {
		return ErrValidation
	}
	// The card token must be an opaque provider handle (a UUID), never a PAN.
	if _, err := uuid.Parse(token); err != nil {
		return ErrValidation
	}
	if looksLikeRawPAN(d.Token) || looksLikeRawPAN(d.MaskedIdentifier) {
		return ErrValidation
	}
	return nil
}

// VerifyOwnedBy proves this destination is the card registered TO restaurantID
// and that the card is identified completely enough for the provider to address
// it. It is the ownership gate of the money-out path: nothing may put a card
// handle on a payout, and nothing may dispatch one, without passing it.
//
// A nil receiver is a legitimate input — it is what "this venue has no card on
// file" looks like after a repository ErrNotFound — and it answers the missing
// code rather than panicking, so every caller has exactly one branch to write.
//
// The three outcomes are deliberately distinct codes because the operator's
// next action differs: ask the venue for a card (missing), stop and investigate
// (mismatch — money was about to go to somebody else's card), finish the
// tokenization step (incomplete).
func (d *PayoutDestination) VerifyOwnedBy(restaurantID uuid.UUID) error {
	if d == nil {
		return WithCode(CodePayoutDestinationMissing,
			fmt.Errorf("%w: restaurant %s has no payout destination", ErrNotFound, restaurantID))
	}
	if restaurantID == uuid.Nil || d.RestaurantID != restaurantID {
		return WithCode(CodePayoutDestinationMismatch,
			fmt.Errorf("%w: payout destination belongs to restaurant %s, not %s",
				ErrValidation, d.RestaurantID, restaurantID))
	}
	if err := d.Validate(); err != nil {
		return WithCode(CodePayoutDestinationIncomplete,
			fmt.Errorf("%w: payout destination of restaurant %s is not a usable card handle",
				ErrValidation, restaurantID))
	}
	// A tokenized payout is addressed by the PAIR (provider user id, card
	// token): the token alone does not say whose card it is. A destination
	// missing the user id is therefore not merely incomplete paperwork — it is
	// a card we cannot prove is addressable at all, and money must not be
	// dispatched against it. Refused HERE, before the acquirer is called, so it
	// can never become a payout stranded in `sent`.
	if d.Method == PayoutMethodFreedomPayCardToken && strings.TrimSpace(d.ProviderCustomerRef) == "" {
		return WithCode(CodePayoutDestinationIncomplete,
			fmt.Errorf("%w: payout destination of restaurant %s has no provider user id, a tokenized card cannot be addressed without it",
				ErrValidation, restaurantID))
	}
	return nil
}

// VerifyPayoutDestination proves the card FROZEN on a payout is still the card
// registered to that payout's restaurant.
//
// The snapshot on the payout row (DestinationToken / DestinationCustomerRef /
// Method) exists so a later change of the venue's destination does not rewrite
// history — but "do not rewrite history" must never turn into "keep paying an
// address the venue has since replaced". Between generation and dispatch the
// venue can change its card (a manager's card swapped back to the owner's, a
// compromised destination corrected), and the money must follow the card the
// venue owns NOW, not the one that happened to be on file when the payout row
// was written. A payout whose snapshot no longer matches is refused, never
// silently re-addressed: re-pointing money at a different card automatically is
// exactly the behaviour an attacker would want.
//
// current is the destination read for p.RestaurantID, or nil when there is
// none.
func VerifyPayoutDestination(p Payout, current *PayoutDestination) error {
	if err := current.VerifyOwnedBy(p.RestaurantID); err != nil {
		return err
	}
	sameCard := strings.TrimSpace(p.DestinationToken) == strings.TrimSpace(current.Token) &&
		strings.TrimSpace(p.DestinationCustomerRef) == strings.TrimSpace(current.ProviderCustomerRef) &&
		p.Method == current.Method
	if !sameCard {
		// The token is never named in the message: it is the address of the
		// money and this text reaches logs and the failure_reason column.
		return WithCode(CodePayoutDestinationMismatch,
			fmt.Errorf("%w: payout %s addresses a card that is not the current payout destination of restaurant %s",
				ErrValidation, p.ID, p.RestaurantID))
	}
	return nil
}

// PayoutDestinationRepository persists one destination per restaurant.
type PayoutDestinationRepository interface {
	// Upsert stores the restaurant's destination, replacing any existing one in
	// place (INSERT ... ON CONFLICT (restaurant_id) DO UPDATE).
	Upsert(ctx context.Context, d *PayoutDestination) error
	// Get returns the restaurant's destination or ErrNotFound.
	Get(ctx context.Context, restaurantID uuid.UUID) (*PayoutDestination, error)
}

// Payout is one settlement to one restaurant for a set of ledger entries. Money
// is integer minor units, never a float.
//
// Three amounts, deliberately kept apart (see payout_fee.go):
//
//	GrossAmountMinor — the settled money claimed into this payout (= the sum of
//	                   its PayoutItem contributions).
//	FeeMinor         — what the acquirer charges US for moving it (1.9%, min
//	                   300 ₸ at FreedomPay's 14.07.2026 tariff).
//	AmountMinor      — what is actually dispatched to the venue's card. Equal to
//	                   the gross when the PLATFORM bears the fee (the default),
//	                   gross − fee when the VENUE bears it.
type Payout struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	AmountMinor  int64
	// GrossAmountMinor is the settled money claimed into this payout, before
	// the acquirer's payout fee. It equals the sum of the payout's items and is
	// what the venue's statement calls "owed for the period".
	GrossAmountMinor int64
	// FeeMinor is the acquirer's cost of moving this payout, computed at
	// generation time from the configured rate + floor and FROZEN on the row:
	// a later tariff change must not rewrite what a past payout cost. Zero only
	// when the fee configuration is zero.
	FeeMinor int64
	// FeeBearer records who paid FeeMinor for THIS payout, snapshotted from the
	// policy in force when it was generated.
	FeeBearer PayoutFeeBearer
	// PeriodDate is the VENUE-LOCAL calendar day whose end-of-day this payout
	// settles — the key of the once-per-day-per-venue guarantee (a partial
	// UNIQUE index on (restaurant_id, currency, period_date) over the
	// non-failed rows). It is a pure calendar label, normalised to UTC midnight
	// so it never drifts with the server's zone. nil for a payout generated by
	// hand outside the daily schedule: those consume no day.
	PeriodDate *time.Time
	// PeriodEndAt is the exact instant that local day ended (the venue-local
	// midnight that opened the following day). Every ledger entry created
	// STRICTLY BEFORE it is in scope for this payout — including money rolled
	// over from earlier days that was below the payout minimum, which is why
	// there is no matching PeriodStartAt: the lower bound of a payout is "not
	// yet claimed", not a date.
	PeriodEndAt *time.Time
	// ForcedByAge marks a payout the daily pass produced even though the
	// venue's balance was BELOW its payout threshold, because the venue's
	// oldest unpaid money had been held for its full max-hold window. Such a
	// payout still pays the acquirer's fee — that is the deliberate trade of
	// the setting (a small, relatively expensive payout beats holding a venue's
	// money indefinitely), so it is recorded on the row and shown on the
	// statement rather than left to be inferred from the amount.
	ForcedByAge bool
	Currency    Currency
	Status      PayoutStatus
	Method      PayoutMethod
	// DestinationToken is a snapshot of the destination the payout was sent to,
	// so a later change of the venue's destination does not rewrite history.
	DestinationToken string
	// DestinationCustomerRef is a snapshot of the destination's provider user
	// id (FreedomPay pg_user_id), paired with DestinationToken at send time.
	DestinationCustomerRef string
	// ProviderRef is the acquirer-side payout id (pg_payment_id), set once the
	// send is dispatched. nil until then.
	ProviderRef *string
	// IdempotencyKey is our own key, unique per payout, handed to the acquirer
	// (pg_order_id / pg_idempotency_key) so a retried send resolves to the same
	// provider payout instead of moving money twice.
	IdempotencyKey string
	FailureCode    *string
	FailureReason  *string
	// StatusChangedAt is the reconciler's lease clock, stamped on every real
	// status change — same role as Payment.StatusChangedAt.
	StatusChangedAt        time.Time
	ReconcileAttempts      int
	LastReconcileAttemptAt *time.Time
	NeedsManualReview      bool
	SentAt                 *time.Time
	PaidAt                 *time.Time
	FailedAt               *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Amount returns the amount actually dispatched to the venue as Money — the
// number handed to the acquirer, net of the fee when the venue bears it.
func (p Payout) Amount() Money { return Money{AmountMinor: p.AmountMinor, Currency: p.Currency} }

// Gross returns the settled money claimed into the payout, before the fee.
func (p Payout) Gross() Money {
	return Money{AmountMinor: p.GrossAmountMinor, Currency: p.Currency}
}

// Fee returns the acquirer's cost of this payout.
func (p Payout) Fee() Money { return Money{AmountMinor: p.FeeMinor, Currency: p.Currency} }

// PayoutItem links one ledger entry to the payout that pays it. The UNIQUE
// constraint on LedgerEntryID (DB side) is the single arbiter that a captured
// restaurant credit is settled through at most one LIVE payout: a failed payout
// deletes its items to release them, so "live" == pending|sent|paid.
type PayoutItem struct {
	ID            uuid.UUID
	PayoutID      uuid.UUID
	LedgerEntryID uuid.UUID
	RestaurantID  uuid.UUID
	// AmountSignedMinor is the entry's contribution to the payout, signed:
	// a restaurant CREDIT (money owed to the venue) is positive, a restaurant
	// DEBIT (a refund/correction that reduces what is owed) is negative. The sum
	// over a payout's items equals Payout.GrossAmountMinor — the amount BEFORE
	// the payout fee, not Payout.AmountMinor, which may be net of a
	// venue-borne fee.
	AmountSignedMinor int64
	Currency          Currency
	CreatedAt         time.Time
}

// OwedBalance is the computed unpaid balance owed to one restaurant in one
// currency, together with the concrete ledger entries that make it up.
type OwedBalance struct {
	RestaurantID uuid.UUID
	Currency     Currency
	AmountMinor  int64       // net owed = credits - debits over the unclaimed entries
	Entries      []OwedEntry // the exact ledger entries this balance is built from
}

// OwedEntry is one unclaimed restaurant-account ledger line contributing to an
// OwedBalance.
type OwedEntry struct {
	LedgerEntryID     uuid.UUID
	AmountSignedMinor int64 // credit positive, debit negative
	Currency          Currency
	// CreatedAt is when the ledger entry was booked — i.e. HOW LONG this money
	// has been waiting to be paid out. It is what the daily pass's max-hold
	// rule measures; zero means the source could not tell us, and the age rule
	// then deliberately does not fire (see OwedBalance.OldestEntryAt).
	CreatedAt time.Time
}

// OldestEntryAt returns when the OLDEST money in this balance was booked — the
// age the max-hold rule is measured against.
//
// Derived from the entries rather than stored as a field on purpose: the
// entries ARE the balance, so a separate field could drift out of sync with
// them. Entries with a zero CreatedAt (a source that does not carry the
// instant) are ignored, and a balance with no usable timestamp at all returns
// the zero time — which callers must read as "age unknown, do not force a
// payout", never as "infinitely old".
func (b OwedBalance) OldestEntryAt() time.Time {
	var oldest time.Time
	for _, e := range b.Entries {
		if e.CreatedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || e.CreatedAt.Before(oldest) {
			oldest = e.CreatedAt
		}
	}
	return oldest
}

// PayoutRepository persists payouts. Get* return ErrNotFound when absent.
type PayoutRepository interface {
	Create(ctx context.Context, p *Payout) error
	GetByID(ctx context.Context, id uuid.UUID) (*Payout, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*Payout, error)
	// CompareAndSwapStatus is one `UPDATE payouts SET status=$to, ... WHERE
	// id=$1 AND status=$from`. Returns ErrAlreadyExists (zero rows) when another
	// caller already moved the row — the same DB-level CAS the payments code
	// uses. providerRef/failure are applied only on the matching transition.
	CompareAndSwapStatus(ctx context.Context, id uuid.UUID, from, to PayoutStatus, patch PayoutStatusPatch, at time.Time) error
	// SetProviderRef records the acquirer-side payout id on a payout still in
	// `sent`, WITHOUT changing its status — the "accepted, still processing"
	// case, where the reconciler must later ask the acquirer for the outcome.
	// Idempotent: it only fills an empty provider_ref (COALESCE), so a retry
	// never overwrites a ref already stored.
	SetProviderRef(ctx context.Context, id uuid.UUID, providerRef string) error
	// ClaimStale selects up to limit payouts stuck in the given statuses whose
	// StatusChangedAt is older than before, oldest first, FOR UPDATE SKIP
	// LOCKED. Same contract as PaymentRepository.ClaimStale: the lock must NOT
	// be held across the acquirer call; correctness comes from the CAS writes
	// afterwards, never from the lock.
	ClaimStale(ctx context.Context, statuses []PayoutStatus, before time.Time, limit int) ([]Payout, error)
	// RecordReconcileAttempt bumps the reconcile counter for a payout still in
	// expectedStatus, flipping NeedsManualReview at maxAttempts. Returns
	// ErrAlreadyExists when the payout already moved on. Mirrors
	// PaymentRepository.RecordReconcileAttempt.
	RecordReconcileAttempt(ctx context.Context, id uuid.UUID, expectedStatus PayoutStatus, at time.Time, maxAttempts int) (attempts int, needsManualReview bool, err error)
	// List returns payouts for a restaurant, newest first.
	List(ctx context.Context, restaurantID uuid.UUID, limit int) ([]Payout, error)
}

// PayoutStatusPatch carries the extra columns a status transition sets. All
// fields are optional; only the non-nil ones are written.
type PayoutStatusPatch struct {
	ProviderRef   *string
	FailureCode   *string
	FailureReason *string
}

// PayoutItemRepository claims and releases the ledger entries a payout covers.
type PayoutItemRepository interface {
	// CreateBatch inserts the claim rows for a payout in ONE statement. A
	// UNIQUE(ledger_entry_id) violation surfaces as ErrAlreadyExists — the
	// arbiter that a ledger entry is never claimed by two live payouts. All rows
	// land in one INSERT, so a mid-batch conflict leaves nothing behind.
	CreateBatch(ctx context.Context, items []PayoutItem) error
	// DeleteByPayout releases a payout's claims (used when a payout fails), so
	// the underlying ledger entries are owed again.
	DeleteByPayout(ctx context.Context, payoutID uuid.UUID) error
	ListByPayout(ctx context.Context, payoutID uuid.UUID) ([]PayoutItem, error)
}

// OwedReader computes what BookEat owes restaurants from the ledger, excluding
// any entry already claimed by a live payout.
type OwedReader interface {
	// OwedForRestaurant returns the restaurant's unpaid balance in every
	// currency that has a positive net owed, with the concrete unclaimed
	// entries. Currencies whose net is <= 0 are omitted (a refund-heavy period
	// carries forward against future credits rather than creating a negative
	// payout).
	OwedForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]OwedBalance, error)
	// OwedForRestaurantUpTo is OwedForRestaurant bounded by an END OF PERIOD:
	// only ledger entries created STRICTLY BEFORE before are considered. It is
	// what makes an end-of-day payout mean "the venue's local day that has
	// finished" instead of "everything up to whenever the worker happened to
	// tick" — money captured after the venue's local midnight belongs to the
	// next day's payout, not to this one.
	//
	// There is no lower bound on purpose: money left unclaimed by an earlier
	// day (below the payout minimum, or released by a failed payout) is still
	// owed and rolls into this period automatically, without any carry-forward
	// bookkeeping that could lose or duplicate it.
	OwedForRestaurantUpTo(ctx context.Context, restaurantID uuid.UUID, before time.Time) ([]OwedBalance, error)
	// OwedRestaurantIDs lists every restaurant that currently has a positive
	// unpaid balance — the input to a "generate payouts for all venues" run.
	OwedRestaurantIDs(ctx context.Context) ([]uuid.UUID, error)
}

// PayoutVenueReader resolves the venue-local day boundary inputs for the daily
// payout pass. It is a read of restaurants.timezone kept as its own narrow port
// so the payouts usecase never depends on the restaurant repository as a whole.
//
// Batched by design: the daily pass resolves every owed venue's zone in ONE
// query rather than N, and a venue with no timezone set is simply absent from
// the map — the caller falls back to the configured platform default rather
// than guessing the server's zone.
type PayoutVenueReader interface {
	TimezonesFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]string, error)
}

// ---------------------------------------------------------------------------
// PayoutGateway — the acquirer payout port
// ---------------------------------------------------------------------------

// PayoutGateway is the only thing the payouts usecase knows about a payout
// provider. FreedomPay is an adapter behind it (infrastructure/payment/
// freedompay). Like PaymentGateway it is a strict anti-corruption layer:
// provider status words and pg_* fields never leak outward, and every call
// carries our idempotency key so a retry resolves to the same provider payout.
type PayoutGateway interface {
	// Payout dispatches a single payout. A definite success returns a
	// GatewayPayout with PayoutPaid or PayoutSent (accepted, still processing);
	// a definite decline returns ErrProviderDeclined; a timeout/malformed answer
	// returns ErrProviderOutcomeUnknown and the caller must leave the payout
	// `sent` for the reconciler, never mark it paid or failed.
	Payout(ctx context.Context, req PayoutRequest) (*GatewayPayout, error)
	// GetPayout reads the provider's own view of a payout by OUR order id (the
	// payout's own UUID, sent as pg_order_id on the original dispatch) — the
	// reconciliation path that resolves a `sent` payout to paid or failed. The
	// order id is used, not the provider ref, because a dispatch whose response
	// was lost never yielded a provider ref, yet must still be reconcilable.
	GetPayout(ctx context.Context, orderID string) (*GatewayPayout, error)
	// Name is the provider code this adapter serves.
	Name() PaymentProvider
}

// PayoutRequest is everything a provider needs to move money to a venue,
// expressed in domain terms only.
type PayoutRequest struct {
	PayoutID         uuid.UUID
	IdempotencyKey   string
	Amount           Money
	Method           PayoutMethod
	DestinationToken string
	// DestinationCustomerRef is the provider user id the token is registered
	// under (FreedomPay pg_user_id). Required by the tokenized payout.
	DestinationCustomerRef string
	Description            string // service wording only, never guest/card data
	CallbackURL            string // our webhook endpoint for payout status, may be empty
}

// GatewayPayout is the provider's view of a payout, already translated.
type GatewayPayout struct {
	ProviderRef    string
	Status         PayoutStatus // paid | sent (processing) | failed
	Amount         Money
	FailureCode    string
	FailureMessage string
}

package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Payout ledger
//
// The payment ledger (payment_ledger.go) is keyed by PAYMENT: every line
// belongs to one guest payment. A payout is the other direction — it settles
// MANY payments' worth of credits at once and it costs a fee that belongs to no
// single payment — so it gets its own append-only double-entry ledger keyed by
// PAYOUT, with the same accounts and the same debit==credit invariant.
//
// Why a ledger at all, when the fee is already a column on the payout row: the
// column answers "what did THIS payout cost"; the ledger answers "what did
// payouts cost us last month, and who paid" as a plain SUM over an account —
// without a calculator and without re-deriving policy from history.
// ---------------------------------------------------------------------------

// EntryPayoutFee labels the acquirer's charge for MOVING a payout (FreedomPay
// 1.9% / min 300 ₸). It is deliberately distinct from EntryAcquiring, which is
// the cut withheld on the money-IN side at refund time — mixing the two would
// make the cost of paying venues out unmeasurable.
const EntryPayoutFee LedgerEntryType = "payout_fee"

// PayoutLedgerEntry is one line of the payout ledger. Append-only, exactly like
// PaymentLedgerEntry: a mistake is corrected by a reversing entry, never by an
// UPDATE or a DELETE. AmountMinor is always positive; direction carries the
// meaning.
type PayoutLedgerEntry struct {
	ID          uuid.UUID
	PayoutID    uuid.UUID
	Account     LedgerAccount
	Direction   LedgerDirection
	AmountMinor int64
	Currency    Currency
	EntryType   LedgerEntryType
	CreatedAt   time.Time
}

// Amount returns the entry amount as Money.
func (e PayoutLedgerEntry) Amount() Money {
	return Money{AmountMinor: e.AmountMinor, Currency: e.Currency}
}

// PayoutLedgerRepository appends and reads payout ledger entries. There is no
// Update and no Delete on purpose.
type PayoutLedgerRepository interface {
	// CreateBatch appends a balanced set of entries for one payout. Call it
	// inside the same TxManager as the payout status change that earned them,
	// and validate the batch with ValidatePayoutLedgerBalance first. A repeat
	// of the same (payout, account, direction, type) surfaces as
	// ErrAlreadyExists — the DB-level guard that a replayed transition cannot
	// book the same cost twice.
	CreateBatch(ctx context.Context, entries []PayoutLedgerEntry) error
	ListByPayout(ctx context.Context, payoutID uuid.UUID) ([]PayoutLedgerEntry, error)
}

// PayoutLedgerEntries builds the double-entry batch for a payout that has been
// CONFIRMED PAID — the moment the money and the cost are both real:
//
//	restaurant debit  amount   settlement  (the venue's claim on us shrinks)
//	acquirer   credit amount   settlement  (the acquirer now holds it)
//	<bearer>   debit  fee      payout_fee  (whoever pays the cost of moving it)
//	acquirer   credit fee      payout_fee
//
// The fee's DEBIT side names the payer, so "how much did payouts cost the
// platform" is SUM(payout_fee entries on the platform account) and "how much
// did venues pay in payout fees" is the same sum on the restaurant account —
// no policy reconstruction needed.
//
// Balance check, both policies:
//   - platform bears: amount == gross, debits = gross + fee = credits.
//   - venue bears:    amount == gross − fee, debits = (gross − fee) + fee =
//     gross = credits.
func PayoutLedgerEntries(p Payout, at time.Time) []PayoutLedgerEntry {
	entries := make([]PayoutLedgerEntry, 0, 4)
	add := func(account LedgerAccount, dir LedgerDirection, amount int64, et LedgerEntryType) {
		if amount <= 0 {
			return
		}
		entries = append(entries, PayoutLedgerEntry{
			ID: uuid.New(), PayoutID: p.ID, Account: account, Direction: dir,
			AmountMinor: amount, Currency: p.Currency, EntryType: et, CreatedAt: at,
		})
	}
	add(AccountRestaurant, DirectionDebit, p.AmountMinor, EntrySettlement)
	add(AccountAcquirer, DirectionCredit, p.AmountMinor, EntrySettlement)
	if p.FeeMinor > 0 {
		payer := AccountPlatform
		if p.FeeBearer == PayoutFeeBearerVenue {
			payer = AccountRestaurant
		}
		add(payer, DirectionDebit, p.FeeMinor, EntryPayoutFee)
		add(AccountAcquirer, DirectionCredit, p.FeeMinor, EntryPayoutFee)
	}
	return entries
}

// ValidatePayoutLedgerBalance asserts the double-entry invariant for one
// payout: total debits equal total credits, one payout, one currency, known
// accounts and directions, positive amounts. An empty batch is invalid —
// writing "no movement" as a movement hides a bug.
func ValidatePayoutLedgerBalance(entries []PayoutLedgerEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("empty payout ledger batch: %w", ErrValidation)
	}

	var debit, credit int64
	payoutID := entries[0].PayoutID
	currency := entries[0].Currency

	for i, e := range entries {
		if e.PayoutID != payoutID {
			return fmt.Errorf("entry %d belongs to payout %s, batch is for %s: %w",
				i, e.PayoutID, payoutID, ErrValidation)
		}
		if e.Currency != currency {
			return fmt.Errorf("entry %d currency %q, batch is %q: %w", i, e.Currency, currency, ErrCurrencyMismatch)
		}
		if !e.Account.Valid() {
			return fmt.Errorf("entry %d unknown account %q: %w", i, e.Account, ErrValidation)
		}
		if !e.Direction.Valid() {
			return fmt.Errorf("entry %d unknown direction %q: %w", i, e.Direction, ErrValidation)
		}
		if e.AmountMinor <= 0 {
			return fmt.Errorf("entry %d amount %d must be positive: %w", i, e.AmountMinor, ErrValidation)
		}
		if e.Direction == DirectionDebit {
			if debit > 0 && e.AmountMinor > (1<<62)-debit {
				return ErrMoneyOverflow
			}
			debit += e.AmountMinor
		} else {
			if credit > 0 && e.AmountMinor > (1<<62)-credit {
				return ErrMoneyOverflow
			}
			credit += e.AmountMinor
		}
	}

	if debit != credit {
		return fmt.Errorf("payout ledger unbalanced: debit %d != credit %d: %w", debit, credit, ErrValidation)
	}
	return nil
}

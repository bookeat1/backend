package domain

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LedgerAccount names one side of a money movement, stored as VARCHAR.
type LedgerAccount string

const (
	// AccountGuest is the person who pays.
	AccountGuest LedgerAccount = "guest"
	// AccountRestaurant is the venue's payable balance. Under the subscription
	// monetisation model the whole base amount (deposit / pre-order) belongs
	// here — the platform takes no percentage of the bill (spec §9.2).
	AccountRestaurant LedgerAccount = "restaurant"
	// AccountPlatform is BookEat. It receives the service fee and nothing else.
	AccountPlatform LedgerAccount = "platform"
	// AccountAcquirer is the cost of moving money (withheld on refunds). It is
	// a cost, not revenue, which is why it is not part of AccountPlatform.
	AccountAcquirer LedgerAccount = "acquirer"
)

// Valid reports whether a is a known ledger account.
func (a LedgerAccount) Valid() bool {
	switch a {
	case AccountGuest, AccountRestaurant, AccountPlatform, AccountAcquirer:
		return true
	}
	return false
}

// LedgerDirection is the side of a double-entry line, stored as VARCHAR.
type LedgerDirection string

const (
	DirectionDebit  LedgerDirection = "debit"
	DirectionCredit LedgerDirection = "credit"
)

// Valid reports whether d is a known direction.
func (d LedgerDirection) Valid() bool {
	return d == DirectionDebit || d == DirectionCredit
}

// LedgerEntryType labels why a pair of entries was written. It is free-form for
// reporting, closed here so the strings stay consistent across the codebase.
type LedgerEntryType string

const (
	EntryCapture    LedgerEntryType = "capture"
	EntryServiceFee LedgerEntryType = "service_fee"
	EntryRefund     LedgerEntryType = "refund"
	EntryAcquiring  LedgerEntryType = "acquiring"
	EntrySettlement LedgerEntryType = "settlement"
	EntryCorrection LedgerEntryType = "correction"
)

// PaymentLedgerEntry is one line of the double-entry ledger. Entries are
// append-only: a mistake is fixed by a reversing entry (EntryCorrection), never
// by an UPDATE or a DELETE — otherwise the audit trail is worth nothing in a
// dispute.
//
// AmountMinor is always positive; direction, not sign, carries the meaning.
type PaymentLedgerEntry struct {
	ID          uuid.UUID
	PaymentID   uuid.UUID
	RefundID    *uuid.UUID // set when the line belongs to a refund
	Account     LedgerAccount
	Direction   LedgerDirection
	AmountMinor int64
	Currency    Currency
	EntryType   LedgerEntryType
	CreatedAt   time.Time
}

// Amount returns the entry amount as Money.
func (e PaymentLedgerEntry) Amount() Money {
	return Money{AmountMinor: e.AmountMinor, Currency: e.Currency}
}

// PaymentLedgerRepository appends and reads ledger entries. There is no Update
// and no Delete on purpose.
type PaymentLedgerRepository interface {
	// CreateBatch appends a balanced set of entries. Call it inside the same
	// TxManager as the payment mutation, and validate the batch with
	// ValidateLedgerBalance first.
	CreateBatch(ctx context.Context, entries []PaymentLedgerEntry) error
	ListByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]PaymentLedgerEntry, error)
	// BalanceByAccount sums debits minus credits per account for a payment.
	// Used by reconciliation reporting and by the venue payout register.
	BalanceByAccount(ctx context.Context, paymentID uuid.UUID) (map[LedgerAccount]int64, error)
}

// ValidateLedgerBalance asserts the double-entry invariant for one payment:
// total debits equal total credits. It also rejects the mistakes that silently
// break a ledger — unknown accounts or directions, non-positive amounts, mixed
// currencies, and entries belonging to different payments.
//
// An empty batch is invalid: writing "no movement" as a movement hides a bug.
func ValidateLedgerBalance(entries []PaymentLedgerEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("empty ledger batch: %w", ErrValidation)
	}

	var debit, credit int64
	paymentID := entries[0].PaymentID
	currency := entries[0].Currency

	for i, e := range entries {
		if e.PaymentID != paymentID {
			return fmt.Errorf("entry %d belongs to payment %s, batch is for %s: %w",
				i, e.PaymentID, paymentID, ErrValidation)
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
		return fmt.Errorf("ledger unbalanced: debit %d != credit %d: %w", debit, credit, ErrValidation)
	}
	return nil
}

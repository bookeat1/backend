package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// entry is a terse constructor for the invariant tests.
func entry(paymentID uuid.UUID, account LedgerAccount, dir LedgerDirection, amount int64) PaymentLedgerEntry {
	return PaymentLedgerEntry{
		ID:          uuid.New(),
		PaymentID:   paymentID,
		Account:     account,
		Direction:   dir,
		AmountMinor: amount,
		Currency:    CurrencyKZT,
		EntryType:   EntrySettlement,
	}
}

func TestValidateLedgerBalance(t *testing.T) {
	pid := uuid.New()
	other := uuid.New()

	cases := []struct {
		name    string
		entries []PaymentLedgerEntry
		wantErr error
	}{
		{
			// Capture of 10 000 ₸: the guest is debited, the venue is credited
			// the base and the platform the service fee (spec §9.2 — the
			// platform takes no percentage of the bill).
			name: "capture splits into base and fee",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 1_000_000),
				entry(pid, AccountRestaurant, DirectionCredit, 966_000),
				entry(pid, AccountPlatform, DirectionCredit, 34_000),
			},
		},
		{
			name: "refund with acquiring cost",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionCredit, 990_000),
				entry(pid, AccountAcquirer, DirectionCredit, 10_000),
				entry(pid, AccountRestaurant, DirectionDebit, 966_000),
				entry(pid, AccountPlatform, DirectionDebit, 34_000),
			},
		},
		{
			name:    "empty batch",
			entries: nil,
			wantErr: ErrValidation,
		},
		{
			name: "unbalanced by one tiyn",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 1_000_000),
				entry(pid, AccountRestaurant, DirectionCredit, 999_999),
			},
			wantErr: ErrValidation,
		},
		{
			name: "entries from two payments",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 100),
				entry(other, AccountRestaurant, DirectionCredit, 100),
			},
			wantErr: ErrValidation,
		},
		{
			name: "mixed currencies",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 100),
				{ID: uuid.New(), PaymentID: pid, Account: AccountRestaurant, Direction: DirectionCredit, AmountMinor: 100, Currency: "USD"},
			},
			wantErr: ErrCurrencyMismatch,
		},
		{
			name: "unknown account",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 100),
				entry(pid, LedgerAccount("bookeat"), DirectionCredit, 100),
			},
			wantErr: ErrValidation,
		},
		{
			name: "unknown direction",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, LedgerDirection("in"), 100),
				entry(pid, AccountRestaurant, DirectionCredit, 100),
			},
			wantErr: ErrValidation,
		},
		{
			name: "zero amount",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, 0),
				entry(pid, AccountRestaurant, DirectionCredit, 0),
			},
			wantErr: ErrValidation,
		},
		{
			// Direction carries the sign; a negative amount would let two bugs
			// cancel each other out and still "balance".
			name: "negative amount",
			entries: []PaymentLedgerEntry{
				entry(pid, AccountGuest, DirectionDebit, -100),
				entry(pid, AccountRestaurant, DirectionCredit, -100),
			},
			wantErr: ErrValidation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLedgerBalance(tc.entries)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateLedgerBalance() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLedgerAccountAndDirectionValid(t *testing.T) {
	for account, want := range map[LedgerAccount]bool{
		AccountGuest: true, AccountRestaurant: true, AccountPlatform: true, AccountAcquirer: true,
		"bank": false, "": false,
	} {
		if got := account.Valid(); got != want {
			t.Errorf("LedgerAccount(%q).Valid() = %v, want %v", account, got, want)
		}
	}
	for dir, want := range map[LedgerDirection]bool{
		DirectionDebit: true, DirectionCredit: true, "DEBIT": false, "": false,
	} {
		if got := dir.Valid(); got != want {
			t.Errorf("LedgerDirection(%q).Valid() = %v, want %v", dir, got, want)
		}
	}
}

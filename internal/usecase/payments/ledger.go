package payments

import (
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// captureLedgerEntries builds the double-entry batch for turning a hold into
// a charge (spec §9.2 — the platform takes no percentage of the bill, the
// whole base amount is the venue's):
//
//	guest      debit  total     (money leaves the guest)
//	restaurant credit base      (the venue's payable balance)
//	platform   credit fee      (BookEat's service fee, and only that)
//
// This is the literal example asserted by domain.TestValidateLedgerBalance
// ("capture splits into base and fee"); keep both in sync.
func captureLedgerEntries(p domain.Payment, at time.Time) []domain.PaymentLedgerEntry {
	entries := make([]domain.PaymentLedgerEntry, 0, 3)
	add := func(account domain.LedgerAccount, dir domain.LedgerDirection, amount int64, et domain.LedgerEntryType) {
		if amount <= 0 {
			return
		}
		entries = append(entries, domain.PaymentLedgerEntry{
			ID: uuid.New(), PaymentID: p.ID, Account: account, Direction: dir,
			AmountMinor: amount, Currency: p.Currency, EntryType: et, CreatedAt: at,
		})
	}
	add(domain.AccountGuest, domain.DirectionDebit, p.AmountMinor, domain.EntryCapture)
	add(domain.AccountRestaurant, domain.DirectionCredit, p.BaseAmountMinor, domain.EntryCapture)
	add(domain.AccountPlatform, domain.DirectionCredit, p.FeeMinor, domain.EntryServiceFee)
	return entries
}

// settlementLedgerEntries turns a domain.Settlement into the ledger delta
// against what captureLedgerEntries already booked at capture time.
//
// The subtlety (see domain.SettleRefund's doc comment and
// domain.TestValidateLedgerBalance's "refund with acquiring cost" case, which
// this function reproduces exactly): the restaurant and the platform were
// ALREADY credited base and fee at capture. A settlement only writes new
// lines for what actually CHANGES:
//   - guest gets s.GuestMinor back → credit guest;
//   - the acquirer's cut is booked as a cost → credit acquirer;
//   - the restaurant's claim shrinks from base to s.RestaurantMinor → debit
//     the difference (zero when a late cancellation / no-show confirms the
//     venue keeps exactly what it already has — no line is written, matching
//     the empty-batch-is-invalid rule in domain.ValidateLedgerBalance);
//   - same for the platform's fee claim.
//
// A late cancellation or a no-show settlement therefore returns an EMPTY
// batch: nothing changed from what capture already booked, and the caller
// (refund.go) must recognise that and skip the ledger write entirely rather
// than call CreateBatch with nothing in it.
func settlementLedgerEntries(p domain.Payment, s domain.Settlement, refundID *uuid.UUID, at time.Time) []domain.PaymentLedgerEntry {
	var entries []domain.PaymentLedgerEntry
	add := func(account domain.LedgerAccount, dir domain.LedgerDirection, amount int64, et domain.LedgerEntryType) {
		if amount <= 0 {
			return
		}
		entries = append(entries, domain.PaymentLedgerEntry{
			ID: uuid.New(), PaymentID: p.ID, RefundID: refundID, Account: account, Direction: dir,
			AmountMinor: amount, Currency: p.Currency, EntryType: et, CreatedAt: at,
		})
	}
	add(domain.AccountGuest, domain.DirectionCredit, s.GuestMinor, domain.EntryRefund)
	add(domain.AccountAcquirer, domain.DirectionCredit, s.AcquirerMinor, domain.EntryAcquiring)
	if delta := p.BaseAmountMinor - s.RestaurantMinor; delta > 0 {
		add(domain.AccountRestaurant, domain.DirectionDebit, delta, domain.EntryRefund)
	}
	if delta := p.FeeMinor - s.PlatformMinor; delta > 0 {
		add(domain.AccountPlatform, domain.DirectionDebit, delta, domain.EntryServiceFee)
	}
	return entries
}

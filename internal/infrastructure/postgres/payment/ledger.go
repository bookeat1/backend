package payment

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Ledger implements domain.PaymentLedgerRepository. Entries are append-only:
// there is deliberately no Update and no Delete method here at all, matching
// the domain interface — a mistake is fixed by a reversing entry
// (domain.EntryCorrection), never by touching a written row.
type Ledger struct{ pool sqltx.Querier }

// NewLedger builds the ledger repository.
func NewLedger(pool sqltx.Querier) *Ledger { return &Ledger{pool: pool} }

var _ domain.PaymentLedgerRepository = (*Ledger)(nil)

const ledgerCols = `id, payment_id, refund_id, account, direction, amount_minor, currency, entry_type, created_at`

// CreateBatch appends a balanced set of entries as ONE multi-row INSERT: all
// rows land in the same statement, so a mid-batch failure (a bad refund_id
// FK, a constraint violation) leaves nothing behind — there is no partial
// batch to clean up. domain.ValidateLedgerBalance is checked here too,
// defensively, even though every usecase caller already checks it before
// calling this — the same belt-and-braces the fake ledger repo applies, so a
// future caller that forgets cannot silently write an unbalanced batch.
func (r *Ledger) CreateBatch(ctx context.Context, entries []domain.PaymentLedgerEntry) error {
	if err := domain.ValidateLedgerBalance(entries); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO payment_ledger_entries (` + ledgerCols + `) VALUES `)
	args := make([]any, 0, len(entries)*9)
	now := time.Now()
	for i := range entries {
		e := &entries[i]
		if e.ID == uuid.Nil {
			e.ID = uuid.New()
		}
		if e.CreatedAt.IsZero() {
			e.CreatedAt = now
		}
		n := len(args)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9)
		args = append(args, e.ID, e.PaymentID, e.RefundID, string(e.Account),
			string(e.Direction), e.AmountMinor, string(e.Currency), string(e.EntryType), e.CreatedAt)
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, sb.String(), args...); err != nil {
		return mapWrite(err, "create payment ledger batch")
	}
	return nil
}

func (r *Ledger) ListByPaymentID(ctx context.Context, paymentID uuid.UUID) ([]domain.PaymentLedgerEntry, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+ledgerCols+` FROM payment_ledger_entries WHERE payment_id=$1 ORDER BY created_at, id`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("list payment ledger entries: %w", err)
	}
	defer rows.Close()
	var out []domain.PaymentLedgerEntry
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("list payment ledger entries: %w", err)
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// BalanceByAccount sums debits minus credits per account, computed in SQL so
// the result reflects the durable ledger, not whatever the caller happens to
// hold in memory.
func (r *Ledger) BalanceByAccount(ctx context.Context, paymentID uuid.UUID) (map[domain.LedgerAccount]int64, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT account,
			COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0)
			  - COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0) AS balance
		 FROM payment_ledger_entries
		 WHERE payment_id=$1
		 GROUP BY account`, paymentID)
	if err != nil {
		return nil, fmt.Errorf("balance payment ledger by account: %w", err)
	}
	defer rows.Close()
	out := map[domain.LedgerAccount]int64{}
	for rows.Next() {
		var account string
		var balance int64
		if err := rows.Scan(&account, &balance); err != nil {
			return nil, fmt.Errorf("balance payment ledger by account: %w", err)
		}
		out[domain.LedgerAccount(account)] = balance
	}
	return out, rows.Err()
}

func scanLedgerEntry(row scanner) (*domain.PaymentLedgerEntry, error) {
	var e domain.PaymentLedgerEntry
	var account, direction, currency, entryType string
	if err := row.Scan(&e.ID, &e.PaymentID, &e.RefundID, &account, &direction,
		&e.AmountMinor, &currency, &entryType, &e.CreatedAt); err != nil {
		return nil, err
	}
	e.Account = domain.LedgerAccount(account)
	e.Direction = domain.LedgerDirection(direction)
	e.Currency = domain.Currency(currency)
	e.EntryType = domain.LedgerEntryType(entryType)
	return &e, nil
}

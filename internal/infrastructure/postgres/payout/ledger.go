package payout

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Ledger implements domain.PayoutLedgerRepository — the append-only
// double-entry record of what a payout moved and what it cost. Same discipline
// as the payment ledger's repository: insert-only, no Update, no Delete.
type Ledger struct{ pool sqltx.Querier }

// NewLedger builds the payout ledger repository.
func NewLedger(pool sqltx.Querier) *Ledger { return &Ledger{pool: pool} }

var _ domain.PayoutLedgerRepository = (*Ledger)(nil)

const payoutLedgerCols = `id, payout_id, account, direction, amount_minor, currency, entry_type, created_at`

// CreateBatch appends a balanced batch in ONE statement, so a mid-batch
// conflict leaves nothing behind. A repeat of the same
// (payout, account, direction, type) hits uq_payout_ledger_line and surfaces as
// domain.ErrAlreadyExists — the DB-level guard that a replayed "mark paid"
// cannot book the acquirer's fee twice.
func (r *Ledger) CreateBatch(ctx context.Context, entries []domain.PayoutLedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO payout_ledger_entries (` + payoutLedgerCols + `) VALUES `)
	args := make([]any, 0, len(entries)*8)
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
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8)
		args = append(args, e.ID, e.PayoutID, string(e.Account), string(e.Direction),
			e.AmountMinor, string(e.Currency), string(e.EntryType), e.CreatedAt)
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, sb.String(), args...); err != nil {
		return mapWrite(err, "create payout ledger entries")
	}
	return nil
}

// ListByPayout returns a payout's ledger lines, oldest first.
func (r *Ledger) ListByPayout(ctx context.Context, payoutID uuid.UUID) ([]domain.PayoutLedgerEntry, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+payoutLedgerCols+` FROM payout_ledger_entries
		 WHERE payout_id = $1 ORDER BY created_at, id`, payoutID)
	if err != nil {
		return nil, fmt.Errorf("list payout ledger entries: %w", err)
	}
	defer rows.Close()

	var out []domain.PayoutLedgerEntry
	for rows.Next() {
		var e domain.PayoutLedgerEntry
		var account, direction, currency, entryType string
		if err := rows.Scan(&e.ID, &e.PayoutID, &account, &direction, &e.AmountMinor,
			&currency, &entryType, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan payout ledger entry: %w", err)
		}
		e.Account = domain.LedgerAccount(account)
		e.Direction = domain.LedgerDirection(direction)
		e.Currency = domain.Currency(currency)
		e.EntryType = domain.LedgerEntryType(entryType)
		out = append(out, e)
	}
	return out, rows.Err()
}

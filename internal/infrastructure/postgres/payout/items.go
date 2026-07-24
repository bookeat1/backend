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

// Items implements domain.PayoutItemRepository.
type Items struct{ pool sqltx.Querier }

// NewItems builds the payout-item repository.
func NewItems(pool sqltx.Querier) *Items { return &Items{pool: pool} }

var _ domain.PayoutItemRepository = (*Items)(nil)

const itemCols = `id, payout_id, ledger_entry_id, restaurant_id, amount_signed_minor, currency, created_at`

// CreateBatch claims a payout's ledger entries in ONE multi-row INSERT. A
// UNIQUE(ledger_entry_id) violation surfaces as domain.ErrAlreadyExists — the
// single arbiter that a captured restaurant credit is settled through at most
// one live payout. Because all rows land in one statement, a mid-batch conflict
// rolls the whole INSERT back; nothing is half-claimed.
func (r *Items) CreateBatch(ctx context.Context, items []domain.PayoutItem) error {
	if len(items) == 0 {
		return fmt.Errorf("empty payout item batch: %w", domain.ErrValidation)
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO payout_items (` + itemCols + `) VALUES `)
	args := make([]any, 0, len(items)*7)
	now := time.Now()
	for i := range items {
		it := &items[i]
		if it.ID == uuid.Nil {
			it.ID = uuid.New()
		}
		if it.CreatedAt.IsZero() {
			it.CreatedAt = now
		}
		n := len(args)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7)
		args = append(args, it.ID, it.PayoutID, it.LedgerEntryID, it.RestaurantID,
			it.AmountSignedMinor, string(it.Currency), it.CreatedAt)
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, sb.String(), args...); err != nil {
		return mapWrite(err, "create payout item batch")
	}
	return nil
}

// DeleteByPayout releases a payout's claims (a failed payout), so its ledger
// entries are owed again and a later payout can re-claim them.
func (r *Items) DeleteByPayout(ctx context.Context, payoutID uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM payout_items WHERE payout_id=$1`, payoutID); err != nil {
		return fmt.Errorf("delete payout items: %w", err)
	}
	return nil
}

func (r *Items) ListByPayout(ctx context.Context, payoutID uuid.UUID) ([]domain.PayoutItem, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+itemCols+` FROM payout_items WHERE payout_id=$1 ORDER BY created_at, id`, payoutID)
	if err != nil {
		return nil, fmt.Errorf("list payout items: %w", err)
	}
	defer rows.Close()
	var out []domain.PayoutItem
	for rows.Next() {
		var it domain.PayoutItem
		var currency string
		if err := rows.Scan(&it.ID, &it.PayoutID, &it.LedgerEntryID, &it.RestaurantID,
			&it.AmountSignedMinor, &currency, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan payout item: %w", err)
		}
		it.Currency = domain.Currency(currency)
		out = append(out, it)
	}
	return out, rows.Err()
}

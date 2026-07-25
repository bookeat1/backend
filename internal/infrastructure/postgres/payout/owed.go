package payout

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Owed implements domain.OwedReader: it computes what BookEat owes restaurants
// straight from the payment ledger, excluding any entry already claimed by a
// live payout (payout_items). "Owed" is the net of the restaurant account's
// CREDITS (capture) minus its DEBITS (refunds/corrections) — the ledger is the
// single source of truth for the venue's payable balance (payment_ledger.go,
// AccountRestaurant).
type Owed struct{ pool sqltx.Querier }

// NewOwed builds the owed-balance reader.
func NewOwed(pool sqltx.Querier) *Owed { return &Owed{pool: pool} }

var _ domain.OwedReader = (*Owed)(nil)

// signedExpr maps a ledger direction to its contribution to the owed balance:
// a restaurant credit adds, a restaurant debit subtracts.
const signedExpr = `CASE WHEN le.direction = 'credit' THEN le.amount_minor ELSE -le.amount_minor END`

// OwedForRestaurant returns one OwedBalance per currency with a positive net
// owed, each carrying the exact unclaimed ledger entries it is built from. A
// currency whose net is <= 0 is omitted: a refund-heavy period must not create
// a negative or zero payout — those entries stay unclaimed and net against
// future credits. The concrete entries are returned so the caller can claim
// exactly them into a payout in the same logical operation, closing the gap
// between "what I summed" and "what I claim".
func (r *Owed) OwedForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.OwedBalance, error) {
	// A zero cutoff means "no upper bound" — see OwedForRestaurantUpTo.
	return r.OwedForRestaurantUpTo(ctx, restaurantID, time.Time{})
}

// OwedForRestaurantUpTo is OwedForRestaurant bounded by the end of a period:
// only ledger entries created STRICTLY BEFORE before are considered, which is
// what makes a scheduled payout mean "the venue's local day that has finished".
// A zero `before` disables the bound (the manual, unbounded path).
//
// There is deliberately no lower bound: an entry left unclaimed by an earlier
// day (below the payout minimum, or released by a failed payout) is still owed
// and is picked up here automatically — the roll-over needs no bookkeeping of
// its own and therefore cannot drift from the ledger.
func (r *Owed) OwedForRestaurantUpTo(ctx context.Context, restaurantID uuid.UUID, before time.Time) ([]domain.OwedBalance, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		// le.created_at is selected because it is the AGE of the money: the
		// daily pass's max-hold rule ("this venue never reaches its threshold,
		// pay it anyway") is measured against the oldest unclaimed entry, and
		// that age can only come from the same read that produced the balance.
		`SELECT le.id, le.currency, `+signedExpr+` AS signed, le.created_at
		 FROM payment_ledger_entries le
		 JOIN payments p ON p.id = le.payment_id
		 WHERE le.account = 'restaurant'
		   AND p.restaurant_id = $1
		   AND ($2::timestamptz IS NULL OR le.created_at < $2::timestamptz)
		   AND NOT EXISTS (SELECT 1 FROM payout_items pi WHERE pi.ledger_entry_id = le.id)
		 ORDER BY le.currency, le.created_at, le.id`,
		restaurantID, nullableTime(before))
	if err != nil {
		return nil, fmt.Errorf("read owed for restaurant: %w", err)
	}
	defer rows.Close()

	byCurrency := map[domain.Currency]*domain.OwedBalance{}
	var order []domain.Currency
	for rows.Next() {
		var entryID uuid.UUID
		var currency string
		var signed int64
		var createdAt time.Time
		if err := rows.Scan(&entryID, &currency, &signed, &createdAt); err != nil {
			return nil, fmt.Errorf("scan owed entry: %w", err)
		}
		cur := domain.Currency(currency)
		bal, ok := byCurrency[cur]
		if !ok {
			bal = &domain.OwedBalance{RestaurantID: restaurantID, Currency: cur}
			byCurrency[cur] = bal
			order = append(order, cur)
		}
		bal.AmountMinor += signed
		bal.Entries = append(bal.Entries, domain.OwedEntry{
			LedgerEntryID:     entryID,
			AmountSignedMinor: signed,
			Currency:          cur,
			CreatedAt:         createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owed for restaurant: %w", err)
	}

	out := make([]domain.OwedBalance, 0, len(order))
	for _, cur := range order {
		if bal := byCurrency[cur]; bal.AmountMinor > 0 {
			out = append(out, *bal)
		}
	}
	return out, nil
}

// OwedRestaurantIDs lists every restaurant with a positive unpaid balance in at
// least one currency — the input to a "generate payouts for all venues" run.
func (r *Owed) OwedRestaurantIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT p.restaurant_id
		 FROM payment_ledger_entries le
		 JOIN payments p ON p.id = le.payment_id
		 WHERE le.account = 'restaurant'
		   AND NOT EXISTS (SELECT 1 FROM payout_items pi WHERE pi.ledger_entry_id = le.id)
		 GROUP BY p.restaurant_id, le.currency
		 HAVING SUM(`+signedExpr+`) > 0`)
	if err != nil {
		return nil, fmt.Errorf("read owed restaurant ids: %w", err)
	}
	defer rows.Close()

	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan owed restaurant id: %w", err)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, rows.Err()
}

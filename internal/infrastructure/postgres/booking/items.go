package booking

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Items implements domain.BookingItemRepository.
type Items struct{ pool sqltx.Querier }

// NewItems builds the pre-order repository.
func NewItems(pool sqltx.Querier) *Items { return &Items{pool: pool} }

var _ domain.BookingItemRepository = (*Items)(nil)

const itemCols = `id, booking_id, menu_item_id, item_name, item_price_minor,
	currency, quantity, status, comment, created_at, updated_at`

func (r *Items) ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingItem, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+itemCols+` FROM booking_items WHERE booking_id=$1 ORDER BY created_at, id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list booking items: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingItem
	for rows.Next() {
		var i domain.BookingItem
		var status string
		if err := rows.Scan(&i.ID, &i.BookingID, &i.MenuItemID, &i.ItemName, &i.PriceMinor,
			&i.Currency, &i.Quantity, &status, &i.Comment, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list booking items: %w", err)
		}
		i.Status = domain.BookingItemStatus(status)
		out = append(out, i)
	}
	return out, rows.Err()
}

func (r *Items) ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, items []domain.BookingItem) error {
	// Two statements — call inside a TxManager.
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM booking_items WHERE booking_id=$1`, bookingID); err != nil {
		return fmt.Errorf("replace booking items: %w", err)
	}
	for i := range items {
		items[i].BookingID = bookingID
	}
	return r.Create(ctx, items)
}

func (r *Items) Create(ctx context.Context, items []domain.BookingItem) error {
	if len(items) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO booking_items (` + itemCols + `) VALUES `)
	args := make([]any, 0, len(items)*11)
	now := time.Now()
	for i := range items {
		if items[i].ID == uuid.Nil {
			items[i].ID = uuid.New()
		}
		if items[i].Currency == "" {
			items[i].Currency = "KZT"
		}
		if items[i].Status == "" {
			items[i].Status = domain.BookingItemPending
		}
		if items[i].CreatedAt.IsZero() {
			items[i].CreatedAt = now
		}
		items[i].UpdatedAt = now
		n := len(args)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8, n+9, n+10, n+11)
		args = append(args, items[i].ID, items[i].BookingID, items[i].MenuItemID,
			items[i].ItemName, items[i].PriceMinor, items[i].Currency, items[i].Quantity,
			string(items[i].Status), items[i].Comment, items[i].CreatedAt, items[i].UpdatedAt)
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, sb.String(), args...); err != nil {
		return mapWrite(err, "create booking items")
	}
	return nil
}

func (r *Items) SetStatus(ctx context.Context, id uuid.UUID, status domain.BookingItemStatus) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_items SET status=$2, updated_at=now() WHERE id=$1`, id, string(status))
	if err != nil {
		return fmt.Errorf("set booking item status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

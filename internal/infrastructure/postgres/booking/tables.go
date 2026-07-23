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

// Tables implements domain.BookingTableRepository.
//
// slot is stored as a half-open tstzrange built in SQL from two timestamptz
// parameters, and read back as lower(slot)/upper(slot) — no range type crosses
// the driver boundary. active is never written here: the trigger on
// bookings.status is its only writer.
type Tables struct{ pool sqltx.Querier }

// NewTables builds the booking ↔ table link repository.
func NewTables(pool sqltx.Querier) *Tables { return &Tables{pool: pool} }

var _ domain.BookingTableRepository = (*Tables)(nil)

const tableCols = `id, booking_id, table_id, lower(slot), upper(slot), active, created_at`

func (r *Tables) Create(ctx context.Context, links []domain.BookingTable) error {
	if len(links) == 0 {
		return nil
	}
	q, args := insertLinks(links)
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...); err != nil {
		return mapWrite(err, "create booking tables")
	}
	return nil
}

func (r *Tables) ReplaceForBooking(ctx context.Context, bookingID uuid.UUID, links []domain.BookingTable) error {
	// DELETE + INSERT are two statements: call inside a TxManager, otherwise a
	// failed insert leaves the booking without tables.
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM booking_tables WHERE booking_id=$1`, bookingID); err != nil {
		return fmt.Errorf("replace booking tables: %w", err)
	}
	for i := range links {
		links[i].BookingID = bookingID
	}
	return r.Create(ctx, links)
}

func (r *Tables) ListByBooking(ctx context.Context, bookingID uuid.UUID) ([]domain.BookingTable, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+tableCols+` FROM booking_tables WHERE booking_id=$1 ORDER BY created_at, id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("list booking tables: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingTable
	for rows.Next() {
		var l domain.BookingTable
		if err := rows.Scan(&l.ID, &l.BookingID, &l.TableID, &l.SlotStart, &l.SlotEnd, &l.Active, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("list booking tables: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *Tables) ListBusy(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.TableBusyInterval, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT bt.table_id, lower(bt.slot), upper(bt.slot)
		 FROM booking_tables bt
		 JOIN restaurant_tables t ON t.id = bt.table_id
		 WHERE t.restaurant_id = $1
		   AND bt.active
		   AND bt.slot && tstzrange($2, $3, '[)')
		 ORDER BY bt.table_id, lower(bt.slot)`, restaurantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list busy intervals: %w", err)
	}
	defer rows.Close()
	var out []domain.TableBusyInterval
	for rows.Next() {
		var b domain.TableBusyInterval
		if err := rows.Scan(&b.TableID, &b.From, &b.To); err != nil {
			return nil, fmt.Errorf("list busy intervals: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// insertLinks builds one multi-row INSERT. Postgres evaluates the exclusion
// constraint per row, so links conflicting with each other inside a single
// statement are rejected too.
func insertLinks(links []domain.BookingTable) (string, []any) {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO booking_tables (id, booking_id, table_id, slot, created_at) VALUES `)
	args := make([]any, 0, len(links)*5)
	now := time.Now()
	for i := range links {
		if links[i].ID == uuid.Nil {
			links[i].ID = uuid.New()
		}
		if links[i].CreatedAt.IsZero() {
			links[i].CreatedAt = now
		}
		n := len(args)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,tstzrange($%d,$%d,'[)'),$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, links[i].ID, links[i].BookingID, links[i].TableID,
			links[i].SlotStart, links[i].SlotEnd, links[i].CreatedAt)
	}
	return sb.String(), args
}

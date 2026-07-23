package booking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

func link(bookingID, tableID uuid.UUID, from, to time.Time) domain.BookingTable {
	return domain.BookingTable{BookingID: bookingID, TableID: tableID, SlotStart: from, SlotEnd: to}
}

func TestBookingTablesCreateListBusy(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	tid := seedTable(t, pool, rid)
	bookings := New(pool)
	tables := NewTables(pool)

	start := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)
	b := newBooking(rid, start)
	if err := bookings.Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}
	// slot carries a 15-minute buffer on both sides, as the usecase resolves it.
	slotFrom, slotTo := start.Add(-15*time.Minute), start.Add(2*time.Hour+15*time.Minute)
	links := []domain.BookingTable{link(b.ID, tid, slotFrom, slotTo)}
	if err := tables.Create(ctx, links); err != nil {
		t.Fatalf("create link: %v", err)
	}
	if links[0].ID == uuid.Nil {
		t.Error("Create did not backfill the generated id")
	}

	got, err := tables.ListByBooking(ctx, b.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("list by booking = %d rows err=%v", len(got), err)
	}
	if !got[0].SlotStart.Equal(slotFrom) || !got[0].SlotEnd.Equal(slotTo) || !got[0].Active {
		t.Errorf("slot roundtrip mismatch: %+v", got[0])
	}

	busy, err := tables.ListBusy(ctx, rid, start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil || len(busy) != 1 || busy[0].TableID != tid {
		t.Fatalf("list busy = %+v err=%v", busy, err)
	}
	// A window that only touches the slot's exclusive upper bound is not busy.
	busy, err = tables.ListBusy(ctx, rid, slotTo, slotTo.Add(time.Hour))
	if err != nil || len(busy) != 0 {
		t.Errorf("list busy after slot = %d rows err=%v, want 0", len(busy), err)
	}

	// The neighbouring booking overlaps only through the buffer: still rejected.
	next := newBooking(rid, start.Add(2*time.Hour))
	if err := bookings.Create(ctx, next); err != nil {
		t.Fatalf("create next booking: %v", err)
	}
	err = tables.Create(ctx, []domain.BookingTable{
		link(next.ID, tid, start.Add(2*time.Hour-15*time.Minute), start.Add(4*time.Hour+15*time.Minute)),
	})
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("buffered overlap = %v, want ErrAlreadyExists", err)
	}

	// ReplaceForBooking swaps the set inside one transaction.
	txm := sqltx.NewManager(pool)
	moved := start.Add(6 * time.Hour)
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return tables.ReplaceForBooking(ctx, b.ID, []domain.BookingTable{
			link(b.ID, tid, moved, moved.Add(2*time.Hour)),
		})
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = tables.ListByBooking(ctx, b.ID)
	if len(got) != 1 || !got[0].SlotStart.Equal(moved) {
		t.Errorf("replace mismatch: %+v", got)
	}
}

// TestBookingTablesRace is the core guarantee of the wave: two concurrent
// transactions grabbing the same table for overlapping intervals — exactly one
// wins, the loser gets ErrAlreadyExists (HTTP 409), never a raw 23P01 → 500.
func TestBookingTablesRace(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	tid := seedTable(t, pool, rid)
	bookings := New(pool)
	tables := NewTables(pool)
	txm := sqltx.NewManager(pool)

	start := time.Date(2026, 8, 2, 19, 0, 0, 0, time.UTC)
	ids := make([]uuid.UUID, 2)
	for i := range ids {
		b := newBooking(rid, start)
		if err := bookings.Create(ctx, b); err != nil {
			t.Fatalf("create booking %d: %v", i, err)
		}
		ids[i] = b.ID
	}

	// Overlapping, not identical: 19:00–21:00 vs 20:00–22:00.
	slots := [][2]time.Time{
		{start, start.Add(2 * time.Hour)},
		{start.Add(time.Hour), start.Add(3 * time.Hour)},
	}

	var (
		wg    sync.WaitGroup
		gate  = make(chan struct{})
		errCh = make(chan error, len(ids))
	)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			errCh <- txm.WithinTx(ctx, func(ctx context.Context) error {
				return tables.Create(ctx, []domain.BookingTable{
					link(ids[i], tid, slots[i][0], slots[i][1]),
				})
			})
		}(i)
	}
	close(gate)
	wg.Wait()
	close(errCh)

	var wins, conflicts int
	for err := range errCh {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, domain.ErrAlreadyExists):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("race outcome: %d wins, %d conflicts; want exactly 1 and 1", wins, conflicts)
	}
}

// TestBookingTablesFreedOnCancel pins the DB trigger: cancelling a booking drops
// active on its links, which releases the table for the next guest.
func TestBookingTablesFreedOnCancel(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	tid := seedTable(t, pool, rid)
	bookings := New(pool)
	tables := NewTables(pool)

	start := time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC)
	first := newBooking(rid, start)
	if err := bookings.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := tables.Create(ctx, []domain.BookingTable{link(first.ID, tid, start, start.Add(2*time.Hour))}); err != nil {
		t.Fatalf("occupy: %v", err)
	}

	second := newBooking(rid, start)
	if err := bookings.Create(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}
	replacement := []domain.BookingTable{link(second.ID, tid, start, start.Add(2*time.Hour))}
	if err := tables.Create(ctx, replacement); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("occupied table = %v, want ErrAlreadyExists", err)
	}

	if err := bookings.UpdateStatus(ctx, first.ID, domain.BookingCancelled, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	links, _ := tables.ListByBooking(ctx, first.ID)
	if len(links) != 1 || links[0].Active {
		t.Fatalf("trigger left active=true after cancel: %+v", links)
	}

	if err := tables.Create(ctx, []domain.BookingTable{link(second.ID, tid, start, start.Add(2*time.Hour))}); err != nil {
		t.Fatalf("re-occupy after cancel: %v", err)
	}
	busy, _ := tables.ListBusy(ctx, rid, start, start.Add(2*time.Hour))
	if len(busy) != 1 {
		t.Errorf("list busy = %d rows, want only the live booking", len(busy))
	}
}

func TestBookingTablesCreateEmpty(t *testing.T) {
	pool, ctx := setup(t)
	if err := NewTables(pool).Create(ctx, nil); err != nil {
		t.Fatalf("create(nil) = %v, want nil", err)
	}
}

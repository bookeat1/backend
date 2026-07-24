package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- fakes -----------------------------------------------------------------

type fakeReader struct {
	rows map[SourceName][]SourceRow
}

func (f *fakeReader) ListSince(_ context.Context, source SourceName, after Cursor, limit int) ([]SourceRow, error) {
	var out []SourceRow
	for _, r := range f.rows[source] {
		if after.CreatedAt.IsZero() || r.CreatedAt.After(after.CreatedAt) ||
			(r.CreatedAt.Equal(after.CreatedAt) && bytesGreater(r.ID, after.ID)) {
			out = append(out, r)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func bytesGreater(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

type fakeCursor struct {
	cur map[SourceName]Cursor
}

func (f *fakeCursor) Get(_ context.Context, source SourceName) (Cursor, error) {
	return f.cur[source], nil
}
func (f *fakeCursor) Save(_ context.Context, source SourceName, c Cursor) error {
	f.cur[source] = c
	return nil
}

type fakeSender struct {
	batches [][]Event
	failN   int // fail the first failN Send calls
	calls   int
}

func (f *fakeSender) Send(_ context.Context, batch []Event) error {
	f.calls++
	if f.calls <= f.failN {
		return errors.New("simulated transient amplitude failure")
	}
	cp := make([]Event, len(batch))
	copy(cp, batch)
	f.batches = append(f.batches, cp)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func bookingRow(t *testing.T, eventType string, at time.Time) SourceRow {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id": uuid.New(), "restaurant_id": uuid.New(), "user_id": uuid.New(),
		"name": "PII Name", "phone": "+77010000000", "guests": 2,
		"starts_at": at, "status": "confirmed", "source": "guest_app",
	})
	if err != nil {
		t.Fatal(err)
	}
	return SourceRow{ID: uuid.New(), EventType: eventType, Payload: payload, CreatedAt: at}
}

func newDispatcherWith(reader SourceReader, cursor CursorStore, sender Sender) *Dispatcher {
	return NewDispatcher(reader, cursor, sender, Config{BatchSize: 100}, testLogger())
}

// --- tests -----------------------------------------------------------------

// A tracked action is shipped to Amplitude and the cursor advances past it.
func TestTick_ShipsTrackedEventAndAdvances(t *testing.T) {
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	row := bookingRow(t, "booking.created", at)
	reader := &fakeReader{rows: map[SourceName][]SourceRow{SourceBookingOutbox: {row}}}
	cursor := &fakeCursor{cur: map[SourceName]Cursor{}}
	sender := &fakeSender{}
	d := newDispatcherWith(reader, cursor, sender)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Shipped != 1 {
		t.Fatalf("shipped = %d, want 1", res.Shipped)
	}
	if len(sender.batches) != 1 || len(sender.batches[0]) != 1 {
		t.Fatalf("want one batch of one event, got %v", sender.batches)
	}
	if sender.batches[0][0].Type != EventBookingCreated {
		t.Fatalf("shipped type = %q", sender.batches[0][0].Type)
	}
	if got := cursor.cur[SourceBookingOutbox]; got.ID != row.ID || !got.CreatedAt.Equal(at) {
		t.Fatalf("cursor not advanced to last row: %+v", got)
	}
	// A second tick has nothing new to ship.
	res2, _ := d.Tick(context.Background())
	if res2.Shipped != 0 {
		t.Fatalf("second tick shipped = %d, want 0 (cursor past all rows)", res2.Shipped)
	}
}

// Multiple rows in one source are shipped as a SINGLE batch.
func TestTick_BatchesMultipleRows(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	var rows []SourceRow
	for i := 0; i < 5; i++ {
		rows = append(rows, bookingRow(t, "booking.created", base.Add(time.Duration(i)*time.Second)))
	}
	reader := &fakeReader{rows: map[SourceName][]SourceRow{SourceBookingOutbox: rows}}
	sender := &fakeSender{}
	d := newDispatcherWith(reader, &fakeCursor{cur: map[SourceName]Cursor{}}, sender)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Shipped != 5 {
		t.Fatalf("shipped = %d, want 5", res.Shipped)
	}
	if len(sender.batches) != 1 {
		t.Fatalf("want exactly ONE batch (batched), got %d Send calls", len(sender.batches))
	}
	if len(sender.batches[0]) != 5 {
		t.Fatalf("batch size = %d, want 5", len(sender.batches[0]))
	}
}

// A transient send failure leaves the cursor in place so the batch is reshipped.
func TestTick_SendFailure_RetriesLeavesCursor(t *testing.T) {
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	row := bookingRow(t, "booking.created", at)
	reader := &fakeReader{rows: map[SourceName][]SourceRow{SourceBookingOutbox: {row}}}
	cursor := &fakeCursor{cur: map[SourceName]Cursor{}}
	sender := &fakeSender{failN: 1}
	d := newDispatcherWith(reader, cursor, sender)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a send failure must NOT surface as a tick error (business flow unaffected): %v", err)
	}
	if res.Retry != 1 || res.Shipped != 0 {
		t.Fatalf("res = %+v, want retry=1 shipped=0", res)
	}
	if got := cursor.cur[SourceBookingOutbox]; got.ID != uuid.Nil {
		t.Fatalf("cursor advanced despite send failure: %+v", got)
	}
	// Next tick: the same row is reshipped, now succeeds, cursor advances.
	res2, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if res2.Shipped != 1 {
		t.Fatalf("reship shipped = %d, want 1", res2.Shipped)
	}
	if cursor.cur[SourceBookingOutbox].ID != row.ID {
		t.Fatal("cursor must advance after the successful reship")
	}
}

// No AMPLITUDE_API_KEY -> the no-op sender: no crash, cursor still advances so
// the worker does not spin on the same rows.
func TestTick_NoopSender_AdvancesNoCrash(t *testing.T) {
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	row := bookingRow(t, "booking.created", at)
	reader := &fakeReader{rows: map[SourceName][]SourceRow{SourceBookingOutbox: {row}}}
	cursor := &fakeCursor{cur: map[SourceName]Cursor{}}
	d := newDispatcherWith(reader, cursor, NewNoopSender())

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick with no-op sender must not error: %v", err)
	}
	if res.Shipped != 1 {
		t.Fatalf("shipped counter = %d, want 1 (counted even though the sender no-ops)", res.Shipped)
	}
	if cursor.cur[SourceBookingOutbox].ID != row.ID {
		t.Fatal("no-op sender must still let the cursor advance")
	}
}

// Untracked and poison rows are skipped but the cursor still passes them, so
// they never block the tracked rows behind them.
func TestTick_SkipsUntrackedAndPoisonButAdvances(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	untracked := bookingRow(t, "booking.waitlisted", base)
	poison := SourceRow{ID: uuid.New(), EventType: "booking.created", Payload: []byte("{bad"), CreatedAt: base.Add(time.Second)}
	tracked := bookingRow(t, "booking.cancelled", base.Add(2*time.Second))
	reader := &fakeReader{rows: map[SourceName][]SourceRow{SourceBookingOutbox: {untracked, poison, tracked}}}
	cursor := &fakeCursor{cur: map[SourceName]Cursor{}}
	sender := &fakeSender{}
	d := newDispatcherWith(reader, cursor, sender)

	res, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Shipped != 1 || res.Skipped != 1 || res.Poison != 1 {
		t.Fatalf("res = %+v, want shipped=1 skipped=1 poison=1", res)
	}
	if cursor.cur[SourceBookingOutbox].ID != tracked.ID {
		t.Fatal("cursor must advance to the last row of the batch, past skipped/poison rows")
	}
}

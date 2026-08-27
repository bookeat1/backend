package legacysync

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/logger"
)

// recordingSource notes which entities were READ. A disabled entity must not
// even be queried: the old database is a live production system and the sync
// holds a read-only connection to it, so "we fetched it and threw it away"
// would still be load we have no reason to put on the web site's database.
type recordingSource struct{ read []string }

func (s *recordingSource) Restaurants(context.Context, Cursor, int) ([]Restaurant, error) {
	s.read = append(s.read, EntityRestaurants)
	return nil, nil
}
func (s *recordingSource) Tables(context.Context, Cursor, int) ([]Table, error) {
	s.read = append(s.read, EntityTables)
	return nil, nil
}
func (s *recordingSource) MenuCategories(context.Context, Cursor, int) ([]MenuCategory, error) {
	s.read = append(s.read, EntityMenuCategories)
	return nil, nil
}
func (s *recordingSource) MenuItems(context.Context, Cursor, int) ([]MenuItem, error) {
	s.read = append(s.read, EntityMenuItems)
	return nil, nil
}
func (s *recordingSource) Bookings(context.Context, Cursor, int) ([]LegacyBooking, error) {
	s.read = append(s.read, EntityBookings)
	return nil, nil
}
func (s *recordingSource) BookingTables(context.Context, Cursor, int) ([]LegacyBookingTable, error) {
	s.read = append(s.read, EntityBookingTables)
	return nil, nil
}

// entitySink answers the cursor/duration reads a pass needs and records any
// working-hours work. Every Upsert* is left on the embedded nil interface: this
// test's sources return no rows, so calling one would be a bug worth a panic.
type entitySink struct {
	Sink
	hoursScanned int
}

func (s *entitySink) GetCursor(context.Context, string) (Cursor, error) { return Cursor{}, nil }
func (s *entitySink) SetCursor(context.Context, string, Cursor) error   { return nil }
func (s *entitySink) RestaurantDurations(context.Context) (map[uuid.UUID]int, error) {
	return map[uuid.UUID]int{}, nil
}
func (s *entitySink) WorkingHoursCandidates(context.Context, int) ([]WorkingHoursCandidate, error) {
	s.hoursScanned++
	return nil, nil
}

func tickWith(t *testing.T, entities []string) (*recordingSource, *entitySink) {
	t.Helper()
	src := &recordingSource{}
	sink := &entitySink{}
	w := NewWorker(src, sink, passthroughTx{}, Config{Entities: entities}, logger.New("error", "text"))
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	return src, sink
}

// TestDefaultAllowlistReadsBookingsOnly is the guard on the narrowed sync: with
// no LEGACY_SYNC_ENTITIES set, a pass touches bookings and nothing else. The
// catalog, the tables, the menu and the schedules belong to this system now, and
// importing them again overwrote live cabinet edits in silence.
func TestDefaultAllowlistReadsBookingsOnly(t *testing.T) {
	src, sink := tickWith(t, nil)

	if got := strings.Join(src.read, ","); got != EntityBookings {
		t.Errorf("entities read = %q, want %q only", got, EntityBookings)
	}
	if sink.hoursScanned != 0 {
		t.Errorf("working-hours pass ran %d times, want 0 — it is derived from the legacy venue text",
			sink.hoursScanned)
	}
}

// TestAllowlistIsHonouredAndOrdered: a one-off import can still ask for more,
// and the FK-safe parents-first order survives the filtering.
func TestAllowlistIsHonouredAndOrdered(t *testing.T) {
	// Deliberately listed out of order — the worker's own order must win, or a
	// child would be attempted before its parent and park for no reason.
	src, sink := tickWith(t, []string{
		EntityBookings, EntityRestaurants, EntityWorkingHours, EntityTables,
	})

	want := strings.Join([]string{EntityRestaurants, EntityTables, EntityBookings}, ",")
	if got := strings.Join(src.read, ","); got != want {
		t.Errorf("entities read = %q, want %q", got, want)
	}
	if sink.hoursScanned != 1 {
		t.Errorf("working-hours pass ran %d times, want 1 when it is on the list", sink.hoursScanned)
	}
}

// TestValidateEntitiesRejectsUnknown: a typo must fail the worker's startup. If
// it were tolerated it would read as "that entity is off", which is the exact
// failure this allowlist exists to make impossible.
func TestValidateEntitiesRejectsUnknown(t *testing.T) {
	if err := ValidateEntities(KnownEntities()); err != nil {
		t.Fatalf("every known entity must validate: %v", err)
	}
	if err := ValidateEntities(nil); err != nil {
		t.Fatalf("an empty list means the default, not an error: %v", err)
	}
	err := ValidateEntities([]string{EntityBookings, "bokings"})
	if err == nil {
		t.Fatal("a misspelled entity must be rejected")
	}
	if !strings.Contains(err.Error(), "bokings") {
		t.Errorf("error %q should name the offending value", err)
	}
}

// TestDefaultEntitiesAreKnown keeps the two lists from drifting apart.
func TestDefaultEntitiesAreKnown(t *testing.T) {
	if err := ValidateEntities(DefaultEntities); err != nil {
		t.Fatalf("DefaultEntities must be valid: %v", err)
	}
}

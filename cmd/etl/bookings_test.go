package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

// spec §8: item_price numeric → item_price_minor (× 100, banker's rounding).
func TestPriceToMinor(t *testing.T) {
	tests := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "", want: 0},
		{raw: "0", want: 0},
		{raw: "1200", want: 120000},
		{raw: "1200.00", want: 120000},
		{raw: "1200.5", want: 120050},
		{raw: "0.01", want: 1},
		{raw: "12.344", want: 1234},
		{raw: "12.346", want: 1235},
		// Exact halves round to even, not away from zero.
		{raw: "12.345", want: 1234},
		{raw: "12.355", want: 1236},
		{raw: "0.005", want: 0},
		{raw: "0.015", want: 2},
		// More than a half always rounds up.
		{raw: "12.3451", want: 1235},
		{raw: "-1.005", want: -100},
		{raw: "  990.99  ", want: 99099},
		{raw: "abc", wantErr: true},
		{raw: "1.2.3", wantErr: true},
	}
	for _, tt := range tests {
		got, err := priceToMinor(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("priceToMinor(%q): want error, got %d", tt.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("priceToMinor(%q): %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("priceToMinor(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

// spec §8: ends_at = starts_at + the venue's duration, env default when the
// venue has no override.
func TestResolveDurationAndEndsAt(t *testing.T) {
	def := 90 * time.Minute
	starts := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		override sql.NullInt64
		want     time.Duration
	}{
		{name: "no override → env default", override: sql.NullInt64{}, want: 90 * time.Minute},
		{name: "venue override wins", override: sql.NullInt64{Int64: 120, Valid: true}, want: 120 * time.Minute},
		{name: "nonsense override ignored", override: sql.NullInt64{Int64: 0, Valid: true}, want: 90 * time.Minute},
		{name: "negative override ignored", override: sql.NullInt64{Int64: -30, Valid: true}, want: 90 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := resolveDurationMinutes(tt.override, def)
			if d != tt.want {
				t.Fatalf("duration = %s, want %s", d, tt.want)
			}
			if got := starts.Add(d); !got.Equal(starts.Add(tt.want)) {
				t.Fatalf("ends_at = %s, want %s", got, starts.Add(tt.want))
			}
		})
	}
}

// The buffer is part of the stored slot on both sides; 0 is a valid override.
func TestResolveBufferAndSlotBounds(t *testing.T) {
	starts := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	ends := starts.Add(2 * time.Hour)

	if got := resolveBufferMinutes(sql.NullInt64{}, 15*time.Minute); got != 15*time.Minute {
		t.Fatalf("buffer = %s, want the env default 15m", got)
	}
	if got := resolveBufferMinutes(sql.NullInt64{Int64: 0, Valid: true}, 15*time.Minute); got != 0 {
		t.Fatalf("buffer = %s, want 0 (an explicit zero override is meaningful)", got)
	}

	from, to := slotBounds(starts, ends, 15*time.Minute)
	if !from.Equal(starts.Add(-15*time.Minute)) || !to.Equal(ends.Add(15*time.Minute)) {
		t.Fatalf("slot = [%s, %s), want the visit window widened by 15m on both sides", from, to)
	}
}

// The legacy single table_id becomes a booking_tables row with a stable id, so
// re-running the ETL upserts instead of duplicating.
func TestLegacyLinkIDIsDeterministic(t *testing.T) {
	bookingID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tableID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	first := legacyLinkID(bookingID, tableID)
	if second := legacyLinkID(bookingID, tableID); first != second {
		t.Fatalf("legacyLinkID is not stable: %s vs %s", first, second)
	}
	if other := legacyLinkID(bookingID, uuid.New()); other == first {
		t.Fatal("different tables must produce different link ids")
	}
	if other := legacyLinkID(uuid.New(), tableID); other == first {
		t.Fatal("different bookings must produce different link ids")
	}
	if first == uuid.Nil {
		t.Fatal("legacyLinkID returned the nil UUID")
	}
}

// phone → phone_normalized goes through internal/auth/phone; an unnormalizable
// value falls back to the raw string (the column is NOT NULL).
func TestPhoneNormalizationForBookings(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "+7 (777) 123-45-67", want: "+77771234567"},
		{raw: "87771234567", want: "+77771234567"},
		{raw: "+77771234567", want: "+77771234567"},
	}
	for _, tt := range tests {
		if got := phone.Normalize(tt.raw); got != tt.want {
			t.Errorf("phone.Normalize(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// created_by_admin is the only origin signal Supabase had.
func TestBookingSource(t *testing.T) {
	if got := bookingSource(true); got != string(domain.SourceAdmin) {
		t.Fatalf("source = %s, want admin", got)
	}
	if got := bookingSource(false); got != string(domain.SourceApp) {
		t.Fatalf("source = %s, want app", got)
	}
}

// Every Supabase booking_status value must survive the transfer as-is, and
// no_show must not appear (it is never back-filled — spec §8).
func TestSupabaseStatusesAreValid(t *testing.T) {
	for _, s := range []string{"pending", "confirmed", "waitlist", "cancelled", "completed", "arrived"} {
		if !domain.BookingStatus(s).Valid() {
			t.Errorf("supabase status %q is not a valid domain status", s)
		}
	}
	// Statuses that hold a table decide booking_tables.active at insert time.
	for s, want := range map[domain.BookingStatus]bool{
		domain.BookingPending:   true,
		domain.BookingConfirmed: true,
		domain.BookingArrived:   true,
		domain.BookingCompleted: false,
		domain.BookingCancelled: false,
	} {
		if got := s.HoldsTable(); got != want {
			t.Errorf("%s.HoldsTable() = %v, want %v", s, got, want)
		}
	}
}

// sortedBookings must be deterministic so a re-run resolves legacy slot
// overlaps the same way.
func TestSortedBookingsIsDeterministic(t *testing.T) {
	base := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	m := map[uuid.UUID]migratedBooking{}
	for i := 0; i < 20; i++ {
		id := uuid.New()
		m[id] = migratedBooking{id: id, startsAt: base.Add(time.Duration(i%3) * time.Hour)}
	}
	first := sortedBookings(m)
	for i := 0; i < 5; i++ {
		next := sortedBookings(m)
		for j := range first {
			if first[j].id != next[j].id {
				t.Fatal("sortedBookings is not stable across calls")
			}
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i].startsAt.Before(first[i-1].startsAt) {
			t.Fatal("sortedBookings is not ordered by starts_at")
		}
	}
}

// The migration deliberately leaves confirmed_at, arrived_at and cancelled_by
// NULL: Supabase never recorded them. cancelled_by in particular feeds
// venue-reliability statistics, so defaulting it to 'restaurant' would not be a
// placeholder but a fabricated accusation against every venue with cancelled
// history. cancelled_at / cancellation_reason DO come across, so a human can
// still see that and why a booking was cancelled.
//
// This test guards the decision: it fails the moment someone starts sourcing
// those three columns from the dump without revisiting the reasoning (see
// cmd/etl/README.md).
func TestMigratedBookingsHaveNoInventedActorOrTimestamps(t *testing.T) {
	// The INSERT column list and the VALUES list, in order.
	insertCols := []string{
		"id", "restaurant_id", "user_id", "name", "phone", "email", "phone_normalized",
		"guests", "starts_at", "ends_at", "status", "source", "notes", "promotion_id", "event_id",
		"created_by_admin", "forced_placement", "confirmed_at", "arrived_at", "cancelled_at", "cancelled_by",
		"cancellation_reason_code", "cancellation_reason", "late_notification_sent",
		"user_notified_late_at", "user_late_message", "reminder_60_sent_at", "reminder_30_sent_at",
		"original_booking_time_text", "created_at", "updated_at",
	}
	values := []string{
		"$1", "$2", "$3", "$4", "$5", "$6", "$7", "$8", "$9", "$10", "$11", "$12", "$13", "$14", "$15",
		"$16", "false", "NULL", "NULL", "$17", "NULL",
		"$18", "$19", "$20", "$21", "$22", "$23", "$24", "$25", "$26", "$27",
	}
	if len(insertCols) != len(values) {
		t.Fatalf("test fixture is inconsistent: %d columns vs %d values", len(insertCols), len(values))
	}

	normalized := strings.Join(strings.Fields(upsertBooking), " ")
	mustBeNull := map[string]bool{"confirmed_at": true, "arrived_at": true, "cancelled_by": true}
	for i, col := range insertCols {
		if !mustBeNull[col] {
			continue
		}
		if values[i] != "NULL" {
			t.Errorf("%s is bound to %s; it must stay NULL — the dump has no data for it", col, values[i])
		}
		// …and it must not be revived on a re-run either.
		if strings.Contains(normalized, col+"=EXCLUDED."+col) {
			t.Errorf("%s is written by the ON CONFLICT branch; it must stay NULL", col)
		}
	}
	// The fixture above must actually describe the live statement. Compared
	// without whitespace, so re-wrapping the SQL does not fail the test.
	dense := strings.ReplaceAll(normalized, " ", "")
	wantInsert := "INSERTINTObookings(" + strings.Join(insertCols, ",") + ")"
	wantValues := "VALUES(" + strings.Join(values, ",") + ")"
	if !strings.Contains(dense, wantInsert) {
		t.Errorf("upsertBooking column list changed; update this test:\n%s", normalized)
	}
	if !strings.Contains(dense, wantValues) {
		t.Errorf("upsertBooking VALUES list changed; update this test:\n%s", normalized)
	}

	// What the dump DOES have about a cancellation is carried over.
	for _, col := range []string{"cancelled_at", "cancellation_reason", "cancellation_reason_code"} {
		if !strings.Contains(normalized, col+"=EXCLUDED."+col) {
			t.Errorf("%s must be migrated: the source has it", col)
		}
	}
}

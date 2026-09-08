package legacysync

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMapBookingStatusPhoneSource(t *testing.T) {
	start := time.Date(2026, 1, 2, 19, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		in         LegacyBooking
		wantStatus string
		wantPhone  string
		wantSource string
		wantOK     bool
	}{
		{
			name: "guest confirmed",
			in: LegacyBooking{ID: uuid.New(), Status: "confirmed", Guests: 2,
				Phone: "8 701 123 45 67", Email: "A@B.KZ", BookingDate: start},
			wantStatus: "confirmed", wantPhone: "+77011234567", wantSource: "app", wantOK: true,
		},
		{
			name: "admin-created maps to admin source",
			in: LegacyBooking{ID: uuid.New(), Status: "arrived", Guests: 4,
				Phone: "+7 (701) 000-11-22", CreatedByAdmin: true, BookingDate: start},
			wantStatus: "arrived", wantPhone: "+77010001122", wantSource: "admin", wantOK: true,
		},
		{
			name:   "unknown status is not coerced",
			in:     LegacyBooking{ID: uuid.New(), Status: "expired", Guests: 1, Phone: "8700", BookingDate: start},
			wantOK: false, // not a known new status -> skip+flag rather than guess
		},
		{
			name:   "non-positive guests is rejected",
			in:     LegacyBooking{ID: uuid.New(), Status: "pending", Guests: 0, Phone: "8700", BookingDate: start},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mapBooking(tc.in, 90*time.Minute)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status=%q want %q", got.Status, tc.wantStatus)
			}
			if got.PhoneNormalized != tc.wantPhone {
				t.Errorf("phone=%q want %q", got.PhoneNormalized, tc.wantPhone)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source=%q want %q", got.Source, tc.wantSource)
			}
			if got.EndsAt.Sub(got.StartsAt) != 90*time.Minute {
				t.Errorf("duration=%v want 90m", got.EndsAt.Sub(got.StartsAt))
			}
		})
	}
}

func TestMapBookingTableSynthesizedIDIsDeterministic(t *testing.T) {
	bookingID := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	tableID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	base := LegacyBookingTable{ID: uuid.Nil, BookingID: bookingID, TableID: tableID,
		BookingDate: time.Now(), Status: "confirmed"}

	a := mapBookingTable(base, time.Hour)
	b := mapBookingTable(base, time.Hour)
	if a.ID != b.ID {
		t.Fatalf("synthesized id not deterministic: %v vs %v", a.ID, b.ID)
	}
	if a.ID == uuid.Nil {
		t.Fatalf("synthesized id must not be nil")
	}
	if !a.Active {
		t.Errorf("confirmed booking should map to an active hold")
	}
	// a terminal status must not hold a table
	base.Status = "cancelled"
	if mapBookingTable(base, time.Hour).Active {
		t.Errorf("cancelled booking must not be an active hold")
	}
}

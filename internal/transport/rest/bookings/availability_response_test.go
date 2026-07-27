package bookings

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/bookings"
)

// availabilityBody is the decoded payload of GET .../availability, read the way
// a client reads it: through the envelope, from raw JSON, so a field the DTO
// never writes shows up as its zero value here exactly as it does on the wire.
type availabilityBody struct {
	Data struct {
		RestaurantID  string `json:"restaurant_id"`
		Timezone      string `json:"timezone"`
		CapacityMode  string `json:"capacity_mode"`
		CapacitySeats int    `json:"capacity_seats"`
		Slots         []struct {
			Available      bool `json:"available"`
			FreeTables     int  `json:"free_tables"`
			RemainingSeats *int `json:"remaining_seats"`
		} `json:"slots"`
	} `json:"data"`
}

func getAvailability(t *testing.T, d *deps, rid uuid.UUID) availabilityBody {
	t.Helper()
	r := newRouter(d)
	w := do(r, http.MethodGet,
		"/api/v1/restaurants/"+rid.String()+"/availability?date=2026-07-30&guests=4", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	var body availabilityBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode availability body: %v (raw %s)", err, w.Body)
	}
	return body
}

func intp(v int) *int { return &v }

// The response must describe ITSELF truthfully: the summary fields and the
// slots are two readings of the same venue, and a client that trusts the
// summary must reach the same conclusion as one that reads the slots.
//
// Found live on the test server: a seats-mode venue with 30 seats answered
// `"capacity_mode":"", "capacity_seats":0` while every slot carried
// `remaining_seats: 30`. The engine had both values; the transport DTO simply
// did not copy them.
func TestAvailabilityResponseReportsItsCapacityMode(t *testing.T) {
	rid := uuid.New()

	t.Run("seats mode", func(t *testing.T) {
		d := newDeps()
		d.avail.day = &uc.DayAvailability{
			RestaurantID: rid, Date: "2026-07-30", Timezone: "Asia/Almaty",
			Guests: 4, DurationMinutes: 90,
			CapacityMode: domain.CapacityModeSeats, CapacitySeats: 30,
			Slots: []uc.Slot{{Available: true, FreeTables: 7, RemainingSeats: intp(30)}},
		}
		body := getAvailability(t, d, rid)

		if body.Data.CapacityMode != string(domain.CapacityModeSeats) {
			t.Errorf("capacity_mode = %q, want %q", body.Data.CapacityMode, domain.CapacityModeSeats)
		}
		if body.Data.CapacitySeats != 30 {
			t.Errorf("capacity_seats = %d, want 30", body.Data.CapacitySeats)
		}
	})

	t.Run("tables mode", func(t *testing.T) {
		d := newDeps()
		d.avail.day = &uc.DayAvailability{
			RestaurantID: rid, Date: "2026-07-30", Timezone: "Asia/Almaty",
			Guests: 4, DurationMinutes: 90,
			CapacityMode: domain.CapacityModeTables, CapacitySeats: 0,
			Slots: []uc.Slot{{Available: true, FreeTables: 3}},
		}
		body := getAvailability(t, d, rid)

		if body.Data.CapacityMode != string(domain.CapacityModeTables) {
			t.Errorf("capacity_mode = %q, want %q", body.Data.CapacityMode, domain.CapacityModeTables)
		}
		if body.Data.CapacitySeats != 0 {
			t.Errorf("capacity_seats = %d, want 0 in table mode", body.Data.CapacitySeats)
		}
	})
}

// The summary and the slots must never contradict each other, whichever half a
// client happens to read. remaining_seats is present exactly in seats mode, and
// seats mode always names a declared capacity — so "slots say seats, summary
// says nothing" is a state the payload cannot be in.
func TestAvailabilitySummaryAgreesWithSlots(t *testing.T) {
	rid := uuid.New()

	cases := []struct {
		name string
		day  *uc.DayAvailability
	}{
		{
			name: "seats mode day",
			day: &uc.DayAvailability{
				RestaurantID: rid, Date: "2026-07-30", Timezone: "Asia/Almaty",
				Guests: 4, DurationMinutes: 90,
				CapacityMode: domain.CapacityModeSeats, CapacitySeats: 30,
				Slots: []uc.Slot{
					{Available: true, FreeTables: 7, RemainingSeats: intp(30)},
					{Available: false, FreeTables: 0, RemainingSeats: intp(2), Reason: uc.ReasonOccupied},
				},
			},
		},
		{
			name: "tables mode day",
			day: &uc.DayAvailability{
				RestaurantID: rid, Date: "2026-07-30", Timezone: "Asia/Almaty",
				Guests: 4, DurationMinutes: 90,
				CapacityMode: domain.CapacityModeTables,
				Slots: []uc.Slot{
					{Available: true, FreeTables: 3},
					{Available: false, FreeTables: 0, Reason: uc.ReasonOccupied},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeps()
			d.avail.day = tc.day
			body := getAvailability(t, d, rid)

			seatsMode := body.Data.CapacityMode == string(domain.CapacityModeSeats)
			if body.Data.CapacityMode == "" {
				t.Fatalf("capacity_mode is empty: the payload does not say how to read its own slots")
			}
			if seatsMode && body.Data.CapacitySeats <= 0 {
				t.Errorf("capacity_mode=seats but capacity_seats=%d: the summary denies the capacity its slots sell",
					body.Data.CapacitySeats)
			}
			for i, s := range body.Data.Slots {
				if s.RemainingSeats != nil && !seatsMode {
					t.Errorf("slot %d carries remaining_seats=%d but capacity_mode=%q",
						i, *s.RemainingSeats, body.Data.CapacityMode)
				}
				if s.RemainingSeats == nil && seatsMode {
					t.Errorf("slot %d has no remaining_seats but capacity_mode=%q", i, body.Data.CapacityMode)
				}
			}
		})
	}
}

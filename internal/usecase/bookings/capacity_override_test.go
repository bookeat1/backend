package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Deliberate overbooking in table-less mode (owner decision, 25.07.2026):
// staff may seat a party the declared capacity does not fit — "we'll bring a
// chair" — but the seats they take stay counted, and who did it stays on record.
//
// The fakes here reproduce the two DB behaviours the mechanism rests on: a
// bucket refuses a claim above the limit it is stamped with, and every accepted
// claim RE-STAMPS that limit. Everything below is meaningless without both, so
// capacity_override_integration_test.go re-proves each case against real
// Postgres.

// overbookInput is a staff request to seat `guests` beyond the capacity.
func overbookInput(h *createHarness, guests int) CreateInput {
	in := h.input()
	in.UserID = nil // a walk-in the venue takes at the door
	in.Guests = guests
	in.Source = domain.SourceAdmin
	in.Overbook = true
	return in
}

// A guest must not be able to overbook, whatever they put in the body. This is
// the usecase-level check; the transport layer blanks the field as well.
func TestOverbookIsRefusedForGuests(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)
	in := h.input()
	in.Guests = 12
	in.Overbook = true

	_, err := h.uc.Create(context.Background(), h.guest, in)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("guest overbooking = %v, want ErrForbidden", err)
	}
	if len(h.capacity.overrides) != 0 || len(h.bookings.byID) != 0 {
		t.Fatal("a refused override must leave no booking and no audit row")
	}
}

// Staff of ANOTHER venue are not staff here: the same permission that gates a
// manual booking gates this one, and nothing else was invented for it.
func TestOverbookIsRefusedForStaffOfAnotherVenue(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)
	stranger := Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}

	_, err := h.uc.Create(context.Background(), stranger, overbookInput(h, 12))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("outside manager overbooking = %v, want ErrForbidden", err)
	}
	if len(h.capacity.overrides) != 0 {
		t.Fatal("a refused override must write no audit row")
	}
}

// In table mode the equivalent power is force + table_ids. Accepting `overbook`
// there too would create a second, unaudited way of saying the same thing.
func TestOverbookIsRefusedInTableMode(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	in := h.input()
	in.Overbook = true

	_, err := h.uc.Create(context.Background(), h.manager, in)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("overbook in table mode = %v, want ErrValidation", err)
	}
}

// The core case. A 10-seat venue with 8 seats sold takes a party of 4 anyway:
// the booking exists, it HOLDS its seats, the affected buckets end up at 12 with
// no room to spare, and exactly one audit row names the manager who did it.
func TestOverbookSeatsThePartyAndRecordsIt(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)
	ctx := context.Background()

	sold := h.input()
	sold.Guests = 8
	if _, err := h.uc.Create(ctx, h.guest, sold); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	// Without the flag the same party of 4 does not fit — that is the state
	// being overridden.
	plain := overbookInput(h, 4)
	plain.Overbook = false
	if _, err := h.uc.Create(ctx, h.manager, plain); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("party of 4 into 2 free seats without overbook = %v, want ErrAlreadyExists", err)
	}

	details, err := h.uc.Create(ctx, h.manager, overbookInput(h, 4))
	if err != nil {
		t.Fatalf("staff overbooking must be accepted: %v", err)
	}
	if !details.Booking.ForcedPlacement {
		t.Fatal("an overbooked party must be flagged as a forced placement")
	}

	// It holds capacity: the booking is visible to the ledger, not beside it.
	holds := h.capacity.holds[details.Booking.ID]
	if len(holds) == 0 {
		t.Fatal("an overridden booking must still take capacity holds")
	}
	for _, hold := range holds {
		if hold.Seats != 4 {
			t.Fatalf("hold seats = %d, want 4", hold.Seats)
		}
		if hold.SeatsLimit != 12 {
			t.Fatalf("hold limit = %d, want exactly 12 (8 sold + 4 seated, no headroom)", hold.SeatsLimit)
		}
	}

	// Exactly one audit row, and it answers "who put 12 people in a 10-seat room".
	if len(h.capacity.overrides) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(h.capacity.overrides))
	}
	o := h.capacity.overrides[0]
	if o.BookingID != details.Booking.ID || o.RestaurantID != h.restaurantID {
		t.Fatalf("audit row points at the wrong booking: %+v", o)
	}
	if o.ActorUserID != h.manager.UserID || o.ActorType != domain.ActorManager {
		t.Fatalf("audit actor = %s/%s, want the manager", o.ActorUserID, o.ActorType)
	}
	if o.Guests != 4 || o.DeclaredSeats != 10 || o.SeatsOver != 2 || o.PeakSeatsTaken != 12 {
		t.Fatalf("audit numbers = %+v, want 4 guests over a 10-seat room by 2 (12 total)", o)
	}
	if o.PeakBucketStart.IsZero() {
		t.Fatal("the audit row must name the bucket that was overloaded")
	}
}

// The guarantee must not rot: after an override the room is FULL, and the next
// ordinary booking — even for a single guest — is refused exactly as before.
func TestNormalBookingAfterOverrideIsStillRefused(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)
	ctx := context.Background()

	sold := h.input()
	sold.Guests = 8
	if _, err := h.uc.Create(ctx, h.guest, sold); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	if _, err := h.uc.Create(ctx, h.manager, overbookInput(h, 4)); err != nil {
		t.Fatalf("overbook: %v", err)
	}

	one := h.input()
	one.Guests = 1
	if _, err := h.uc.Create(ctx, h.guest, one); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("booking after an override = %v, want ErrAlreadyExists", err)
	}
	// Staff without the intent flag are refused too: the power is the flag, not
	// the person.
	if _, err := h.uc.Create(ctx, h.manager, one); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("staff booking without the flag after an override = %v, want ErrAlreadyExists", err)
	}
	if len(h.capacity.overrides) != 1 {
		t.Fatalf("audit rows = %d, want still exactly 1", len(h.capacity.overrides))
	}
}

// A party larger than the WHOLE room is the case this exists for: normally a
// validation error ("we are never that big"), and seatable with the flag.
func TestOverbookSeatsAPartyLargerThanTheVenue(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)
	ctx := context.Background()

	plain := h.input()
	plain.Guests = 12
	if _, err := h.uc.Create(ctx, h.manager, plain); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("party of 12 in a 10-seat room without the flag = %v, want ErrValidation", err)
	}

	details, err := h.uc.Create(ctx, h.manager, overbookInput(h, 12))
	if err != nil {
		t.Fatalf("overbooking a party of 12: %v", err)
	}
	for _, hold := range h.capacity.holds[details.Booking.ID] {
		if hold.SeatsLimit != 12 {
			t.Fatalf("hold limit = %d, want 12", hold.SeatsLimit)
		}
	}
	if len(h.capacity.overrides) != 1 || h.capacity.overrides[0].SeatsOver != 2 {
		t.Fatalf("audit = %+v, want one row 2 seats over", h.capacity.overrides)
	}
}

// An "override" that turns out not to be needed is not an overbooking: the room
// fit the party, so nothing is raised and nothing is written to the audit. The
// trail must answer "what happened to the room", not "who ticked a checkbox".
func TestOverbookThatFitsChangesNothing(t *testing.T) {
	h := newSeatsCreateHarness(t, 10)

	details, err := h.uc.Create(context.Background(), h.manager, overbookInput(h, 4))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, hold := range h.capacity.holds[details.Booking.ID] {
		if hold.SeatsLimit != 10 {
			t.Fatalf("hold limit = %d, want the declared 10 — nothing was overridden", hold.SeatsLimit)
		}
	}
	if len(h.capacity.overrides) != 0 {
		t.Fatalf("audit rows = %d, want 0 — the party fit", len(h.capacity.overrides))
	}
}

// Availability must not offer the overbooked window to the next guest. It
// reports zero, not a negative number: remaining_seats is what can still be
// sold, and an oversold room sells nothing. The size of the overshoot is the
// audit trail's job.
func TestAvailabilityReportsNoSeatsAfterAnOverride(t *testing.T) {
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()
	day := time.Now().In(loc).AddDate(0, 0, 7)
	capacity := newFakeCapacity()
	u := newSeatsAvailability(t, rid, 10, capacity)

	// 8 sold, then 4 seated anyway over the same visit: 12 in a 10-seat room.
	noon := time.Date(day.Year(), day.Month(), day.Day(), 13, 0, 0, 0, loc)
	policy := domain.BookingPolicy{Duration: 2 * time.Hour, Buffer: 15 * time.Minute, CapacitySeats: 10}
	first := &domain.Booking{ID: uuid.New(), RestaurantID: rid, Guests: 8, StartsAt: noon}
	if err := capacity.Create(context.Background(), buildCapacityHolds(first, policy, time.Now())); err != nil {
		t.Fatalf("seed holds: %v", err)
	}
	over := &domain.Booking{ID: uuid.New(), RestaurantID: rid, Guests: 4, StartsAt: noon}
	overHolds := buildCapacityHolds(over, policy, time.Now())
	for i := range overHolds {
		overHolds[i].SeatsLimit = 12 // what planCapacityOverride stamps
	}
	if err := capacity.Create(context.Background(), overHolds); err != nil {
		t.Fatalf("seed override holds: %v", err)
	}

	got, err := u.Day(context.Background(), rid, day.Format(DateLayout), 1)
	if err != nil {
		t.Fatalf("Day: %v", err)
	}
	for _, s := range got.Slots {
		if s.StartsAt.In(loc).Format("15:04") != "13:00" {
			continue
		}
		if s.Available {
			t.Fatalf("13:00 is still offered after an override: %+v", s)
		}
		if s.RemainingSeats == nil || *s.RemainingSeats != 0 {
			t.Fatalf("13:00 remaining = %v, want 0", s.RemainingSeats)
		}
		if s.FreeTables != 0 {
			t.Fatalf("13:00 free_tables = %d, want 0", s.FreeTables)
		}
	}
}

// A raised bucket limit that outlives its booking (the release path never lowers
// a limit — see migration 0054) must not be read as free seats. freeSeats caps
// every bucket by the venue's CURRENT declared capacity.
func TestFreeSeatsIgnoresALimitRaisedAboveTheVenue(t *testing.T) {
	base := time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC)
	// The override booking is gone: 4 seats taken, but the row still carries the
	// limit 16 that the override stamped on it.
	usage := usageIndex([]domain.CapacityUsage{
		{BucketStart: base, SeatsTaken: 4, SeatsLimit: 16},
	})
	if got := freeSeats(usage, base, base.Add(15*time.Minute), 10); got != 6 {
		t.Fatalf("freeSeats = %d, want 6 — the venue seats 10, not 16", got)
	}
}

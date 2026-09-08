package restaurants

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	schedulerepo "backend-core/internal/infrastructure/postgres/schedule"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	bookinguc "backend-core/internal/usecase/bookings"
	uc "backend-core/internal/usecase/restaurants"
)

// This is the test the whole change is for: a date the venue closed with a
// schedule override must, over the REAL stack (Postgres rows → repositories →
// usecases → JSON),
//
//   - yield ZERO bookable slots from the availability engine, and
//   - be visible to a guest as closed in the public catalog payload,
//
// and the two must agree, because they now resolve the date through the same
// domain helper. Fixing only one of them would be worse than the bug: the
// storefront would show one thing and the booking engine would sell another.
func TestScheduleOverrideClosesBothSlotsAndPayload(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants")
	ctx := context.Background()

	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// Real (not frozen) dates: the availability engine measures lead time and
	// horizon against the wall clock, so the dates under test have to be
	// genuinely in the future for the slots to be bookable at all.
	today := domain.StartOfDay(time.Now().In(almaty), almaty)
	closedDate := today.AddDate(0, 0, 3)
	specialDate := today.AddDate(0, 0, 4)
	normalDate := today.AddDate(0, 0, 5)
	pastDate := today.AddDate(0, 0, -2)

	venue := seededVenue{
		id: uuid.New(), name: "Holiday", timezone: "Asia/Almaty",
		hours:  fullWeekHours("11:00", "22:00"),
		tables: []domain.RestaurantTable{{Name: "T1", Capacity: 4, IsActive: true}},
	}
	seedVenues(t, pool, []seededVenue{venue})

	overrides := schedulerepo.New(pool)
	note := "Санитарный день"
	for _, o := range []*domain.ScheduleOverride{
		{RestaurantID: venue.id, Date: closedDate, IsClosed: true, Note: &note},
		{RestaurantID: venue.id, Date: specialDate, OpenTime: strp("16:00"), CloseTime: strp("20:00")},
		// A closure that has already passed: it must not touch anything ahead.
		{RestaurantID: venue.id, Date: pastDate, IsClosed: true},
	} {
		if err := overrides.Upsert(ctx, o); err != nil {
			t.Fatalf("upsert override %s: %v", o.Date.Format("2006-01-02"), err)
		}
	}

	// ---- the booking engine ------------------------------------------------
	cfg := bookinguc.Config{TimezoneFallback: "Asia/Almaty", SlotStep: 30 * time.Minute}
	avail := bookinguc.NewAvailabilityUseCase(bookingrepo.NewTables(pool), bookingrepo.NewCapacity(pool),
		restrepo.New(pool), restrepo.NewRelated(pool), cfg)

	dayOf := func(d time.Time) *bookinguc.DayAvailability {
		t.Helper()
		got, err := avail.Day(ctx, venue.id, d.Format(bookinguc.DateLayout), 2)
		if err != nil {
			t.Fatalf("availability for %s: %v", d.Format(bookinguc.DateLayout), err)
		}
		return got
	}

	if got := dayOf(closedDate); len(got.Slots) != 0 {
		t.Errorf("a date closed by an override offered %d slots (first %v) — the engine is still selling a holiday",
			len(got.Slots), got.Slots[0].StartsAt.In(almaty))
	}
	special := dayOf(specialDate)
	if len(special.Slots) == 0 {
		t.Fatal("the special day is OPEN with different hours and must still offer slots")
	}
	firstSpecial := special.Slots[0].StartsAt.In(almaty)
	lastSpecial := special.Slots[len(special.Slots)-1].StartsAt.In(almaty)
	if firstSpecial.Format("15:04") != "16:00" {
		t.Errorf("first special-day slot = %s, want 16:00 (the override's own opening)", firstSpecial.Format("15:04"))
	}
	if lastSpecial.Format("15:04") != "18:00" {
		t.Errorf("last special-day slot = %s, want 18:00 (a 2h visit must end by 20:00)", lastSpecial.Format("15:04"))
	}
	normal := dayOf(normalDate)
	if len(normal.Slots) == 0 || normal.Slots[0].StartsAt.In(almaty).Format("15:04") != "11:00" {
		t.Fatalf("an ordinary day must keep its weekly 11:00 grid, got %d slots", len(normal.Slots))
	}
	bookable := false
	for _, s := range normal.Slots {
		if s.Available {
			bookable = true
			break
		}
	}
	if !bookable {
		t.Error("the control day offers no bookable slot at all — the test proves nothing about the closed one")
	}

	// ---- the public payload ------------------------------------------------
	repo := restrepo.New(pool)
	rel := restrepo.NewRelated(pool)
	facade := uc.NewFacade(repo, rel, restrepo.NewCategories(pool), restrepo.NewPartnership(pool),
		sqltx.NewManager(pool),
		uc.WithVenueState(uc.NewVenueState(rel, cfg,
			uc.WithVenueStateClock(func() time.Time { return today.Add(12 * time.Hour) }))))
	router := newTestRouter(facade)
	payload := detailPayload(t, router, "/api/v1/restaurants/"+venue.id.String())

	if payload.Schedule == nil {
		t.Fatal("schedule missing from the public payload")
	}
	// The weekly grid is untouched: exceptions are an addition, not a rewrite.
	if len(payload.Schedule.Days) != 7 {
		t.Errorf("weekly days = %d, want 7 (the exceptions must not replace the week)", len(payload.Schedule.Days))
	}
	wantFrom := today.Format("2006-01-02")
	if payload.Schedule.ExceptionsFrom != wantFrom {
		t.Errorf("exceptions_from = %q, want %q", payload.Schedule.ExceptionsFrom, wantFrom)
	}
	if payload.Schedule.ExceptionsUntil < closedDate.Format("2006-01-02") {
		t.Errorf("exceptions_until = %q does not even cover the closed date %q",
			payload.Schedule.ExceptionsUntil, closedDate.Format("2006-01-02"))
	}

	byDate := map[string]struct {
		open          bool
		opensAt       string
		closesAt      string
		closesNextDay bool
		note          string
	}{}
	for _, e := range payload.Schedule.Exceptions {
		byDate[e.Date] = struct {
			open          bool
			opensAt       string
			closesAt      string
			closesNextDay bool
			note          string
		}{e.IsOpen, e.OpensAt, e.ClosesAt, e.ClosesNextDay, e.Note}
	}
	if len(byDate) != 2 {
		t.Fatalf("exceptions = %+v, want exactly the two upcoming ones (the past closure must be dropped)",
			payload.Schedule.Exceptions)
	}
	closed, ok := byDate[closedDate.Format("2006-01-02")]
	if !ok {
		t.Fatalf("the closed date is NOT in the public payload: a guest still reads the venue as open. got %+v",
			payload.Schedule.Exceptions)
	}
	if closed.open {
		t.Error("the closed date is published as open")
	}
	if closed.note != note {
		t.Errorf("note = %q, want %q", closed.note, note)
	}
	moved, ok := byDate[specialDate.Format("2006-01-02")]
	if !ok {
		t.Fatalf("the changed-hours date is missing from the payload: %+v", payload.Schedule.Exceptions)
	}
	if !moved.open || moved.opensAt != "16:00" || moved.closesAt != "20:00" {
		t.Errorf("special day published as %+v, want open 16:00–20:00", moved)
	}
	if _, ok := byDate[pastDate.Format("2006-01-02")]; ok {
		t.Error("a past closure is published to guests and would shut a venue that is open")
	}

	// ---- the two sides must agree, date by date ---------------------------
	// This is the guarantee that broke: whatever the payload says about a date,
	// the engine must sell accordingly.
	for _, e := range payload.Schedule.Exceptions {
		d, err := time.ParseInLocation("2006-01-02", e.Date, almaty)
		if err != nil {
			t.Fatalf("bad date in payload: %q", e.Date)
		}
		slots := dayOf(d).Slots
		if !e.IsOpen && len(slots) != 0 {
			t.Errorf("%s is published as closed but the engine offers %d slots", e.Date, len(slots))
		}
		if e.IsOpen && len(slots) > 0 {
			if got := slots[0].StartsAt.In(almaty).Format("15:04"); got != e.OpensAt {
				t.Errorf("%s: first slot %s, but the guest was promised an opening at %s", e.Date, got, e.OpensAt)
			}
		}
	}
}

func strp(s string) *string { return &s }

// fullWeekHours is the "open the same hours every day" shape, so the weekday a
// test date happens to fall on never changes the outcome.
func fullWeekHours(open, close_ string) []domain.WorkingHours {
	out := make([]domain.WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		out = append(out, openDay(dow, open, close_))
	}
	return out
}

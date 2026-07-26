package restaurants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeVenueState is a hand-written venueStateReader over two plain maps, close
// enough to the batch SQL: a restaurant with no rows is simply absent.
type fakeVenueState struct {
	hours     map[uuid.UUID][]domain.WorkingHours
	tables    map[uuid.UUID]int
	hoursErr  error
	tablesErr error
	hoursIDs  [][]uuid.UUID // every batch of ids the facade asked for
}

func (f *fakeVenueState) WorkingHoursFor(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.WorkingHours, error) {
	f.hoursIDs = append(f.hoursIDs, ids)
	if f.hoursErr != nil {
		return nil, f.hoursErr
	}
	return f.hours, nil
}

func (f *fakeVenueState) BookableTableCountsFor(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]int, error) {
	if f.tablesErr != nil {
		return nil, f.tablesErr
	}
	return f.tables, nil
}

// fixedTZ resolves every venue to one zone, standing in for the booking
// engine's resolvePolicy (which deps.go binds for real).
type fixedTZ struct{ tz string }

func (f fixedTZ) VenueTimezone(domain.Restaurant) string { return f.tz }

// perVenueTZ resolves each venue to its own stored override, exactly like the
// real resolver does, so a test can prove the flags follow the VENUE's clock.
type perVenueTZ struct{ fallback string }

func (p perVenueTZ) VenueTimezone(r domain.Restaurant) string {
	if tz := r.BookingPolicy.Timezone; tz != nil && *tz != "" {
		return *tz
	}
	return p.fallback
}

func wh(dow int, isOpen bool, open, close_ string) domain.WorkingHours {
	w := domain.WorkingHours{DayOfWeek: dow, IsOpen: isOpen}
	if open != "" {
		w.OpenTime = &open
	}
	if close_ != "" {
		w.CloseTime = &close_
	}
	return w
}

// openEveryDay is the common "11:00 — 01:00, every day" shape of the live data.
func openEveryDay(open, close_ string) []domain.WorkingHours {
	out := make([]domain.WorkingHours, 0, 7)
	for dow := 0; dow < 7; dow++ {
		out = append(out, wh(dow, true, open, close_))
	}
	return out
}

func TestBuildVenueState(t *testing.T) {
	almaty := "Asia/Almaty"
	// 2026-07-25 00:30 Almaty is a Saturday, inside Friday's 11:00–01:00 tail.
	loc, err := time.LoadLocation(almaty)
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	afterMidnight := time.Date(2026, 7, 25, 0, 30, 0, 0, loc)
	middayFriday := time.Date(2026, 7, 24, 13, 0, 0, 0, loc)

	tests := []struct {
		name        string
		hours       []domain.WorkingHours
		tz          string
		tables      int
		now         time.Time
		wantSched   bool
		wantOpenNow *bool
		wantBooking bool
	}{
		{
			name:        "no working-hours rows: schedule unknown and nothing bookable",
			hours:       nil,
			tz:          almaty,
			tables:      4,
			now:         middayFriday,
			wantSched:   false,
			wantBooking: false,
		},
		{
			name:        "open across midnight, asked at 00:30, reads as open",
			hours:       openEveryDay("11:00", "01:00"),
			tz:          almaty,
			tables:      4,
			now:         afterMidnight,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: true,
		},
		{
			name:        "closed today reads as shut but stays bookable on other days",
			hours:       []domain.WorkingHours{wh(6, false, "", ""), wh(5, true, "11:00", "22:00")},
			tz:          almaty,
			tables:      4,
			now:         afterMidnight, // Saturday 00:30, Friday closed at 22:00
			wantSched:   true,
			wantOpenNow: boolp(false),
			wantBooking: true,
		},
		{
			name:        "hours but no tables: schedule known, bookings refused",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          almaty,
			tables:      0,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(true),
			wantBooking: false,
		},
		{
			name:        "tables but every day closed: nothing to book either",
			hours:       []domain.WorkingHours{wh(5, false, "", ""), wh(6, false, "", "")},
			tz:          almaty,
			tables:      9,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: boolp(false),
			wantBooking: false,
		},
		{
			name:        "unknown timezone: days still served, open_now stays unknown",
			hours:       openEveryDay("11:00", "22:00"),
			tz:          "Mars/Olympus",
			tables:      4,
			now:         middayFriday,
			wantSched:   true,
			wantOpenNow: nil,
			wantBooking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := buildVenueState(tc.hours, tc.tz, tc.tables, tc.now)
			if st.AcceptsOnlineBookings != tc.wantBooking {
				t.Errorf("AcceptsOnlineBookings = %v, want %v", st.AcceptsOnlineBookings, tc.wantBooking)
			}
			if tc.wantSched != (st.Schedule != nil) {
				t.Fatalf("Schedule present = %v, want %v", st.Schedule != nil, tc.wantSched)
			}
			if st.Schedule == nil {
				return
			}
			if st.Schedule.Timezone != tc.tz {
				t.Errorf("timezone = %q, want %q", st.Schedule.Timezone, tc.tz)
			}
			switch {
			case tc.wantOpenNow == nil && st.Schedule.OpenNow != nil:
				t.Errorf("OpenNow = %v, want unknown (nil)", *st.Schedule.OpenNow)
			case tc.wantOpenNow != nil && st.Schedule.OpenNow == nil:
				t.Errorf("OpenNow = nil, want %v", *tc.wantOpenNow)
			case tc.wantOpenNow != nil && *st.Schedule.OpenNow != *tc.wantOpenNow:
				t.Errorf("OpenNow = %v, want %v", *st.Schedule.OpenNow, *tc.wantOpenNow)
			}
		})
	}
}

// The same instant must produce different answers for venues in different
// zones — proof that "open now" follows the venue's clock, not the server's.
func TestListComputesOpenNowPerVenueTimezone(t *testing.T) {
	if _, err := time.LoadLocation("Europe/Istanbul"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	almatyVenue := uuid.New()
	istanbulVenue := uuid.New()
	istanbul := "Europe/Istanbul"

	repo := &fakeRestaurantRepo{list: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{ID: almatyVenue}},
		{Restaurant: domain.Restaurant{
			ID:            istanbulVenue,
			BookingPolicy: domain.BookingPolicyOverride{Timezone: &istanbul},
		}},
	}}
	state := &fakeVenueState{
		hours: map[uuid.UUID][]domain.WorkingHours{
			almatyVenue:   openEveryDay("11:00", "22:00"),
			istanbulVenue: openEveryDay("11:00", "22:00"),
		},
		tables: map[uuid.UUID]int{almatyVenue: 4, istanbulVenue: 4},
	}
	// 06:30 UTC = 11:30 in Almaty (open), 09:30 in Istanbul (still shut).
	instant := time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, perVenueTZ{fallback: "Asia/Almaty"},
			WithVenueStateClock(func() time.Time { return instant }))))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.VenueState == nil || it.VenueState.Schedule == nil || it.VenueState.Schedule.OpenNow == nil {
			t.Fatalf("venue %s: expected a computed open_now, got %+v", it.Restaurant.ID, it.VenueState)
		}
	}
	if !*items[0].VenueState.Schedule.OpenNow {
		t.Error("Almaty venue must be open at 11:30 local")
	}
	if *items[1].VenueState.Schedule.OpenNow {
		t.Error("Istanbul venue must be closed at 09:30 local")
	}
	if items[1].VenueState.Schedule.Timezone != istanbul {
		t.Errorf("timezone = %q, want %q", items[1].VenueState.Schedule.Timezone, istanbul)
	}
}

// Both the listing and the detail read must carry the fields; a venue with no
// tables ("Adept") must be flagged before the guest tries date after date.
func TestListAndGetAttachVenueState(t *testing.T) {
	id := uuid.New()
	rest := domain.Restaurant{ID: id}
	repo := &fakeRestaurantRepo{
		list: []domain.RestaurantListItem{{Restaurant: rest}},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	state := &fakeVenueState{
		hours:  map[uuid.UUID][]domain.WorkingHours{id: openEveryDay("11:00", "22:00")},
		tables: map[uuid.UUID]int{}, // no bookable tables at all
	}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, fixedTZ{tz: "Asia/Almaty"})))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if items[0].VenueState == nil || items[0].VenueState.Schedule == nil {
		t.Fatal("list item must carry the schedule")
	}
	if items[0].VenueState.AcceptsOnlineBookings {
		t.Error("a venue with no bookable tables must not claim to accept online bookings")
	}

	agg, err := f.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if agg.VenueState == nil || agg.VenueState.Schedule == nil {
		t.Fatal("detail must carry the schedule")
	}
	if agg.VenueState.AcceptsOnlineBookings {
		t.Error("detail must report the same bookability as the listing")
	}

	// One batch per read, not one query per restaurant.
	if len(state.hoursIDs) != 2 || len(state.hoursIDs[0]) != 1 || len(state.hoursIDs[1]) != 1 {
		t.Errorf("expected one batched lookup per read, got %v", state.hoursIDs)
	}
}

// The catalog is the app's home screen: a failed hours lookup must degrade to
// "unknown", never break the listing.
func TestVenueStateLookupFailureDegradesToUnknown(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{list: []domain.RestaurantListItem{{Restaurant: domain.Restaurant{ID: id}}}}
	state := &fakeVenueState{hoursErr: errors.New("db down")}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, fixedTZ{tz: "Asia/Almaty"})))

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{})
	if err != nil {
		t.Fatalf("list must still succeed: %v", err)
	}
	if items[0].VenueState != nil {
		t.Error("a failed lookup must leave the venue state absent, not defaulted")
	}
}

// Without the option wired, nothing changes for existing callers.
func TestVenueStateAbsentWhenNotWired(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{
		list: []domain.RestaurantListItem{{Restaurant: domain.Restaurant{ID: id}}},
		agg:  &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: id}},
	}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	items, _, err := f.List(context.Background(), domain.RestaurantFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if items[0].VenueState != nil {
		t.Error("venue state must be absent when the enrichment is not wired")
	}
	agg, err := f.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if agg.VenueState != nil {
		t.Error("venue state must be absent on detail when not wired")
	}
}

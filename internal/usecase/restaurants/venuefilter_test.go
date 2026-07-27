package restaurants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// This file pins the SERVER-SIDE catalog filters: ?open_now= and
// ?accepts_online_bookings=. Until they existed the app filtered on the client
// over one page, so both the filter and the "забронировать можно в N из M"
// counter were correct only while the whole catalog fitted in that page.
//
// The invariants under test:
//   - the two flags filter the same way they are published (same schedule code,
//     venue's own timezone) — never a second implementation;
//   - a venue with no usable working hours is NOT open;
//   - the reported total counts the FILTERED set, not the SQL-matched one;
//   - paging happens after the filter, over the whole matching set.

func boolPtr(v bool) *bool { return &v }

// venueFixture is one seeded catalog row plus the state the enrichment should
// derive for it.
type venueFixture struct {
	name   string
	id     uuid.UUID
	hours  []domain.WorkingHours
	tables int
	tz     string // venue override; empty = platform fallback
}

// newFilterFacade wires the real facade + real VenueState over the fakes, with
// a frozen clock. Returns the facade and the repo so a test can inspect the
// filter the repository was handed.
func newFilterFacade(t *testing.T, now time.Time, venues []venueFixture) (Facade, *fakeRestaurantRepo) {
	t.Helper()
	items := make([]domain.RestaurantListItem, 0, len(venues))
	hours := map[uuid.UUID][]domain.WorkingHours{}
	tables := map[uuid.UUID]int{}
	for _, v := range venues {
		r := domain.Restaurant{ID: v.id, Name: v.name}
		if v.tz != "" {
			tz := v.tz
			r.BookingPolicy.Timezone = &tz
		}
		items = append(items, domain.RestaurantListItem{Restaurant: r})
		if len(v.hours) > 0 {
			hours[v.id] = v.hours
		}
		if v.tables > 0 {
			tables[v.id] = v.tables
		}
	}
	repo := &fakeRestaurantRepo{list: items, total: len(items)}
	state := &fakeVenueState{hours: hours, tables: tables}
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{},
		WithVenueState(NewVenueState(state, perVenueTZ{fallback: "Asia/Almaty"},
			WithVenueStateClock(func() time.Time { return now }))))
	return f, repo
}

// catalogFixture is the shared four-venue catalog: every combination of
// open/shut × bookable/not, plus the venue nobody entered hours for.
func catalogFixture(t *testing.T) (time.Time, []venueFixture) {
	t.Helper()
	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// Friday 2026-07-24, 13:00 Almaty.
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, almaty)
	return now, []venueFixture{
		{name: "OpenBookable", id: uuid.New(), hours: openEveryDay("11:00", "22:00"), tables: 4},
		{name: "OpenNoTables", id: uuid.New(), hours: openEveryDay("11:00", "22:00")},
		{name: "ShutBookable", id: uuid.New(), hours: openEveryDay("19:00", "23:00"), tables: 4},
		// No hours at all: the live catalog has exactly one such venue
		// (THE ME'ET — its free-text opening_hours does not parse). It is
		// neither open nor bookable, and it must never be counted as open.
		{name: "NoSchedule", id: uuid.New(), tables: 4},
	}
}

func namesOf(items []domain.RestaurantListItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Restaurant.Name)
	}
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestVenueStateFilterCombinations(t *testing.T) {
	now, venues := catalogFixture(t)

	tests := []struct {
		name  string
		vs    domain.VenueStateFilter
		want  []string
		total int
	}{
		{
			name:  "no filter: the whole catalog, as before",
			vs:    domain.VenueStateFilter{},
			want:  []string{"OpenBookable", "OpenNoTables", "ShutBookable", "NoSchedule"},
			total: 4,
		},
		{
			name:  "open_now=true excludes the shut one AND the one with no hours",
			vs:    domain.VenueStateFilter{OpenNow: boolPtr(true)},
			want:  []string{"OpenBookable", "OpenNoTables"},
			total: 2,
		},
		{
			name:  "open_now=false is the exact complement, so unknown hours land here",
			vs:    domain.VenueStateFilter{OpenNow: boolPtr(false)},
			want:  []string{"ShutBookable", "NoSchedule"},
			total: 2,
		},
		{
			name:  "accepts_online_bookings=true needs both capacity and a readable schedule",
			vs:    domain.VenueStateFilter{AcceptsOnlineBookings: boolPtr(true)},
			want:  []string{"OpenBookable", "ShutBookable"},
			total: 2,
		},
		{
			name:  "accepts_online_bookings=false: no tables, or no schedule to generate slots in",
			vs:    domain.VenueStateFilter{AcceptsOnlineBookings: boolPtr(false)},
			want:  []string{"OpenNoTables", "NoSchedule"},
			total: 2,
		},
		{
			name: "both flags are AND-combined",
			vs: domain.VenueStateFilter{
				OpenNow: boolPtr(true), AcceptsOnlineBookings: boolPtr(true),
			},
			want:  []string{"OpenBookable"},
			total: 1,
		},
		{
			name: "open but unbookable — the combination the app shows as 'только по телефону'",
			vs: domain.VenueStateFilter{
				OpenNow: boolPtr(true), AcceptsOnlineBookings: boolPtr(false),
			},
			want:  []string{"OpenNoTables"},
			total: 1,
		},
		{
			name: "a combination nothing satisfies is an empty page, not an unfiltered one",
			vs: domain.VenueStateFilter{
				OpenNow: boolPtr(false), AcceptsOnlineBookings: boolPtr(false),
			},
			want:  []string{"NoSchedule"},
			total: 1,
		},
	}

	for _, tc := range tests {
		t.Run("list/"+tc.name, func(t *testing.T) {
			f, _ := newFilterFacade(t, now, venues)
			items, total, err := f.List(context.Background(), domain.RestaurantFilter{}, tc.vs)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if got := namesOf(items); !equalNames(got, tc.want) {
				t.Errorf("items = %v, want %v", got, tc.want)
			}
			if total != tc.total {
				t.Errorf("total = %d, want %d", total, tc.total)
			}
		})
		t.Run("search/"+tc.name, func(t *testing.T) {
			f, _ := newFilterFacade(t, now, venues)
			items, total, err := f.Search(context.Background(), domain.RestaurantSearchFilter{}, tc.vs)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if got := namesOf(items); !equalNames(got, tc.want) {
				t.Errorf("items = %v, want %v", got, tc.want)
			}
			if total != tc.total {
				t.Errorf("total = %d, want %d", total, tc.total)
			}
		})
	}
}

// The two values of one flag must partition the catalog: whatever the filter
// does, no venue may fall out of both halves or appear in both.
func TestVenueStateFilterPartitionsTheCatalog(t *testing.T) {
	now, venues := catalogFixture(t)
	for _, flag := range []string{"open_now", "accepts_online_bookings"} {
		t.Run(flag, func(t *testing.T) {
			var yes, no domain.VenueStateFilter
			if flag == "open_now" {
				yes.OpenNow, no.OpenNow = boolPtr(true), boolPtr(false)
			} else {
				yes.AcceptsOnlineBookings, no.AcceptsOnlineBookings = boolPtr(true), boolPtr(false)
			}
			f, _ := newFilterFacade(t, now, venues)
			_, totalYes, err := f.List(context.Background(), domain.RestaurantFilter{}, yes)
			if err != nil {
				t.Fatalf("list true: %v", err)
			}
			f, _ = newFilterFacade(t, now, venues)
			_, totalNo, err := f.List(context.Background(), domain.RestaurantFilter{}, no)
			if err != nil {
				t.Fatalf("list false: %v", err)
			}
			if totalYes+totalNo != len(venues) {
				t.Errorf("%d + %d != %d: a venue is counted twice or lost", totalYes, totalNo, len(venues))
			}
		})
	}
}

// "Open now" must follow the VENUE's clock, not the server's — the same rule
// the published schedule.open_now obeys, because it is literally the same call.
func TestVenueStateFilterUsesTheVenueTimezone(t *testing.T) {
	// 06:30 UTC = 11:30 in Almaty (open), 09:30 in Istanbul (still shut).
	instant := time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)
	venues := []venueFixture{
		{name: "Almaty", id: uuid.New(), hours: openEveryDay("11:00", "22:00"), tables: 4},
		{name: "Istanbul", id: uuid.New(), hours: openEveryDay("11:00", "22:00"), tables: 4,
			tz: "Europe/Istanbul"},
	}
	f, _ := newFilterFacade(t, instant, venues)
	items, total, err := f.List(context.Background(), domain.RestaurantFilter{},
		domain.VenueStateFilter{OpenNow: boolPtr(true)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || !equalNames(namesOf(items), []string{"Almaty"}) {
		t.Fatalf("open_now=true returned %v (total %d), want only Almaty",
			namesOf(items), total)
	}
}

// The filtered read must ask the repository for the WHOLE matching set. Paging
// first and filtering the page afterwards is the client-side bug, moved server
// side: page 1 would hold fewer than per_page results and the total would be
// page-local.
func TestVenueStateFilterReadsTheWholeMatchingSet(t *testing.T) {
	now, venues := catalogFixture(t)

	f, repo := newFilterFacade(t, now, venues)
	if _, _, err := f.List(context.Background(),
		domain.RestaurantFilter{PerPage: 2}, domain.VenueStateFilter{OpenNow: boolPtr(true)}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !repo.lastList.Unpaginated {
		t.Error("a filtered listing must scan the whole matching set, not one page")
	}

	f, repo = newFilterFacade(t, now, venues)
	if _, _, err := f.Search(context.Background(),
		domain.RestaurantSearchFilter{PerPage: 2}, domain.VenueStateFilter{OpenNow: boolPtr(true)}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if !repo.lastSearch.Unpaginated {
		t.Error("a filtered search must scan the whole matching set, not one page")
	}

	// Without a venue-state filter nothing changes: the repository still pages
	// in SQL, which is what every existing client relies on.
	f, repo = newFilterFacade(t, now, venues)
	if _, _, err := f.List(context.Background(),
		domain.RestaurantFilter{PerPage: 2}, domain.VenueStateFilter{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if repo.lastList.Unpaginated {
		t.Error("an unfiltered listing must keep paging in SQL")
	}
}

// Paging is applied AFTER the filter, over the filtered set, and the total
// stays the size of that set on every page.
func TestVenueStateFilterPagination(t *testing.T) {
	almaty, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, almaty)

	// Five open+bookable venues interleaved with five that are shut, so a
	// page-local filter would give a visibly different answer on every page.
	var venues []venueFixture
	var wantOpen []string
	for i := 0; i < 5; i++ {
		open := venueFixture{
			name: "Open" + string(rune('A'+i)), id: uuid.New(),
			hours: openEveryDay("11:00", "22:00"), tables: 4,
		}
		shut := venueFixture{
			name: "Shut" + string(rune('A'+i)), id: uuid.New(),
			hours: openEveryDay("19:00", "23:00"), tables: 4,
		}
		venues = append(venues, open, shut)
		wantOpen = append(wantOpen, open.name)
	}

	tests := []struct {
		page, perPage int
		want          []string
	}{
		{page: 1, perPage: 2, want: wantOpen[0:2]},
		{page: 2, perPage: 2, want: wantOpen[2:4]},
		{page: 3, perPage: 2, want: wantOpen[4:5]},
		{page: 4, perPage: 2, want: nil}, // past the end: empty page, honest total
		{page: 0, perPage: 0, want: wantOpen},
	}
	for _, tc := range tests {
		f, _ := newFilterFacade(t, now, venues)
		items, total, err := f.List(context.Background(),
			domain.RestaurantFilter{Page: tc.page, PerPage: tc.perPage},
			domain.VenueStateFilter{OpenNow: boolPtr(true)})
		if err != nil {
			t.Fatalf("page %d: %v", tc.page, err)
		}
		if got := namesOf(items); !equalNames(got, tc.want) {
			t.Errorf("page %d (per_page %d) = %v, want %v", tc.page, tc.perPage, got, tc.want)
		}
		if total != len(wantOpen) {
			t.Errorf("page %d: total = %d, want %d on EVERY page", tc.page, total, len(wantOpen))
		}
	}
}

// The enrichment is best-effort for a plain catalog read (a failed hours lookup
// degrades to "hours unknown"). It must NOT degrade a request to FILTER by it:
// answering a filtered query with the unfiltered catalog is the very lie this
// feature removes.
func TestVenueStateFilterRefusesWhenStateIsUnavailable(t *testing.T) {
	id := uuid.New()
	repo := &fakeRestaurantRepo{
		list:  []domain.RestaurantListItem{{Restaurant: domain.Restaurant{ID: id}}},
		total: 1,
	}
	// No WithVenueState at all: the same shape as a hours/tables read that
	// failed, i.e. every row comes back with VenueState nil.
	f := NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{})

	items, total, err := f.List(context.Background(), domain.RestaurantFilter{},
		domain.VenueStateFilter{OpenNow: boolPtr(true)})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable (503); got items=%v total=%d", err, items, total)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeCatalogVenueStateUnavailable {
		t.Errorf("code = %q (present %v), want %q", code, ok, domain.CodeCatalogVenueStateUnavailable)
	}

	// Unfiltered reads are unaffected: they still serve the catalog without the
	// optional fields.
	if _, _, err := f.List(context.Background(), domain.RestaurantFilter{},
		domain.VenueStateFilter{}); err != nil {
		t.Errorf("an unfiltered listing must still work without the enrichment: %v", err)
	}
}

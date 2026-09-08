package restaurants

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The catalog's "гости + дата" filter has one property that is easy to lose and
// expensive to lose: when it cannot be computed, the request must FAIL. The
// tempting fallback — serve the catalog unfiltered — produces a screen that
// says "нашли 24 заведения на двоих в пятницу" about a list nobody checked, and
// the guest discovers it only at the booking screen.

type fakeAvailability struct {
	free map[uuid.UUID]bool
	err  error
	// seen records the venues the filter was handed, so a test can prove it ran
	// over the whole matching set rather than over one page.
	seen int
}

func (f *fakeAvailability) Filter(_ context.Context, venues []domain.Restaurant, _ domain.AvailabilitySearch) (map[uuid.UUID]bool, error) {
	f.seen = len(venues)
	return f.free, f.err
}

func availabilityFacade(t *testing.T, now time.Time, venues []venueFixture, av availabilityFilter) Facade {
	t.Helper()
	items := make([]domain.RestaurantListItem, 0, len(venues))
	hours := map[uuid.UUID][]domain.WorkingHours{}
	tables := map[uuid.UUID]int{}
	for _, v := range venues {
		items = append(items, domain.RestaurantListItem{Restaurant: domain.Restaurant{ID: v.id, Name: v.name}})
		if len(v.hours) > 0 {
			hours[v.id] = v.hours
		}
		if v.tables > 0 {
			tables[v.id] = v.tables
		}
	}
	repo := &fakeRestaurantRepo{list: items, total: len(items)}
	state := &fakeVenueState{hours: hours, tables: tables, overrides: map[uuid.UUID][]domain.ScheduleOverride{}}
	opts := []FacadeOption{WithVenueState(NewVenueState(state, perVenueTZ{fallback: "Asia/Almaty"},
		WithVenueStateClock(func() time.Time { return now })))}
	if av != nil {
		opts = append(opts, WithAvailabilityFilter(av))
	}
	return NewFacade(repo, &fakeRelated{}, &fakeCategories{}, &fakePartners{}, &inlineTx{}, opts...)
}

func availabilityQuery() domain.VenueStateFilter {
	return domain.VenueStateFilter{
		Availability: &domain.AvailabilitySearch{Date: "2026-07-24", Guests: 2},
	}
}

func TestAvailabilityFilterKeepsOnlyVenuesWithRoom(t *testing.T) {
	now, venues := catalogFixture(t)
	av := &fakeAvailability{free: map[uuid.UUID]bool{venues[0].id: true, venues[2].id: true}}
	f := availabilityFacade(t, now, venues, av)

	items, total, err := f.List(context.Background(), domain.RestaurantFilter{}, availabilityQuery())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2 — the count must be of what survived the filter", total)
	}
	if got := namesOf(items); !equalNames(got, []string{"OpenBookable", "ShutBookable"}) {
		t.Fatalf("kept %v", got)
	}
	if av.seen != len(venues) {
		t.Fatalf("filter saw %d venues, want the whole matching set (%d)", av.seen, len(venues))
	}
}

func TestAvailabilityFilterRefusesWhenTheEngineIsNotWired(t *testing.T) {
	now, venues := catalogFixture(t)
	f := availabilityFacade(t, now, venues, nil)

	_, _, err := f.List(context.Background(), domain.RestaurantFilter{}, availabilityQuery())
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("got %v, want an unavailable error rather than an unfiltered catalog", err)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeCatalogAvailabilityUnavailable {
		t.Fatalf("code = %q (ok=%v), want %q", code, ok, domain.CodeCatalogAvailabilityUnavailable)
	}
}

func TestAvailabilityFilterRefusesWhenTheEngineFails(t *testing.T) {
	now, venues := catalogFixture(t)
	av := &fakeAvailability{err: errors.New("database is down")}
	f := availabilityFacade(t, now, venues, av)

	_, _, err := f.List(context.Background(), domain.RestaurantFilter{}, availabilityQuery())
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("got %v, want a refusal — a failed availability read must not degrade to an unfiltered list", err)
	}
}

func TestAvailabilityFilterPassesValidationErrorsThrough(t *testing.T) {
	// A broken date or a zero party size is the CLIENT's mistake, and it has to
	// arrive as a 400 with a message, not as our 503.
	now, venues := catalogFixture(t)
	av := &fakeAvailability{err: domain.ErrValidation}
	f := availabilityFacade(t, now, venues, av)

	_, _, err := f.List(context.Background(), domain.RestaurantFilter{}, availabilityQuery())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("got %v, want the validation error to travel through", err)
	}
}

func TestAvailabilityFilterCombinesWithOpenNow(t *testing.T) {
	// Both filters must apply, and the total must count what survived BOTH.
	now, venues := catalogFixture(t)
	av := &fakeAvailability{free: map[uuid.UUID]bool{venues[0].id: true, venues[2].id: true}}
	f := availabilityFacade(t, now, venues, av)

	vs := availabilityQuery()
	open := true
	vs.OpenNow = &open

	items, total, err := f.List(context.Background(), domain.RestaurantFilter{}, vs)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// ShutBookable has room but is closed at 13:00 (it opens at 19:00).
	if total != 1 || !equalNames(namesOf(items), []string{"OpenBookable"}) {
		t.Fatalf("got %v (total %d), want only OpenBookable", namesOf(items), total)
	}
}

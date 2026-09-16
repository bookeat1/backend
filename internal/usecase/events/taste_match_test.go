package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes for GET /events?sort=for_you's three taste-match dependencies ---

type fakeTasteLoader struct {
	profile domain.TasteProfile
	err     error
	calls   int
	gotUser uuid.UUID
}

func (f *fakeTasteLoader) LoadTasteProfile(_ context.Context, userID uuid.UUID) (domain.TasteProfile, error) {
	f.calls++
	f.gotUser = userID
	if f.err != nil {
		return domain.TasteProfile{}, f.err
	}
	return f.profile, nil
}

type fakeVenueSignals struct {
	byID      map[uuid.UUID]domain.RestaurantListItem
	err       error
	calls     int
	gotFilter domain.RestaurantFilter
}

func (f *fakeVenueSignals) ListActive(_ context.Context, flt domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	f.calls++
	f.gotFilter = flt
	if f.err != nil {
		return nil, 0, f.err
	}
	out := make([]domain.RestaurantListItem, 0, len(flt.IDs))
	for _, id := range flt.IDs {
		if v, ok := f.byID[id]; ok {
			out = append(out, v)
		}
	}
	return out, len(out), nil
}

type fakeHomePicksReader struct {
	byCity map[string][]uuid.UUID
	calls  []string
}

func (f *fakeHomePicksReader) ListIDs(_ context.Context, city string) ([]uuid.UUID, error) {
	f.calls = append(f.calls, city)
	return f.byCity[city], nil
}

// venueItem builds a minimal domain.RestaurantListItem carrying exactly the
// signals ScoreTasteMatch reads: cuisines (position order), price, features,
// popularity.
func venueItem(id uuid.UUID, popular bool, price domain.PriceCategory, cuisineCodes ...string) domain.RestaurantListItem {
	cuisines := make([]domain.Cuisine, 0, len(cuisineCodes))
	for _, c := range cuisineCodes {
		cuisines = append(cuisines, domain.Cuisine{Code: c})
	}
	return domain.RestaurantListItem{
		Restaurant: domain.Restaurant{ID: id, PriceCategory: price, IsPopular: &popular},
		Cuisines:   cuisines,
	}
}

func eventItem(id, restaurantID uuid.UUID, startsAt time.Time, city domain.City) domain.EventListItem {
	rid := restaurantID
	return domain.EventListItem{
		Event: domain.Event{
			ID: id, RestaurantID: &rid, Title: "Event " + id.String()[:8],
			StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour), Status: domain.EventPublished,
		},
		Restaurant: &domain.EventRestaurant{ID: restaurantID, City: city},
	}
}

func platformEventItem(id uuid.UUID, startsAt time.Time) domain.EventListItem {
	return domain.EventListItem{
		Event: domain.Event{
			ID: id, RestaurantID: nil, Title: "Platform " + id.String()[:8],
			StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour), Status: domain.EventPublished,
		},
	}
}

// The core of criterion 19: taste score desc, then starts_at asc, then id
// asc — proven with a venue that scores HIGH but starts LATER beating one
// that scores nothing but starts sooner, and a genuine tie broken by id.
func TestListPublicUpcomingForYou_OrdersByScoreThenStartsAtThenID(t *testing.T) {
	italianID, kazakhID, otherItalianID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	soonNoMatch := eventItem(uuid.New(), kazakhID, now.Add(2*time.Hour), domain.CityAlmaty)
	laterMatch := eventItem(uuid.New(), italianID, now.Add(48*time.Hour), domain.CityAlmaty)
	// Two events at DIFFERENT venues that will score IDENTICALLY (same
	// cuisine, same price, both non-popular, no editorial) and start at the
	// exact same instant — only their id can break the tie.
	tieA := eventItem(uuid.New(), otherItalianID, now.Add(72*time.Hour), domain.CityAlmaty)
	tieBID := uuid.New()
	tieRestaurantB := uuid.New()
	tieB := eventItem(tieBID, tieRestaurantB, tieA.StartsAt, domain.CityAlmaty)
	// Force a deterministic id order regardless of uuid.New()'s own
	// randomness: whichever of tieA/tieB sorts first BY STRING becomes
	// "first", and the assertion below follows that, not a hardcoded literal.
	first, second := tieA, tieB
	if second.ID.String() < first.ID.String() {
		first, second = second, first
	}

	repo.publicItems = []domain.EventListItem{soonNoMatch, laterMatch, first, second}

	venues := &fakeVenueSignals{byID: map[uuid.UUID]domain.RestaurantListItem{
		italianID:      venueItem(italianID, false, domain.PriceMid, "italian"),
		kazakhID:       venueItem(kazakhID, false, domain.PriceMid, "kazakh"),
		otherItalianID: venueItem(otherItalianID, false, domain.PriceMid, "italian"),
		tieRestaurantB: venueItem(tieRestaurantB, false, domain.PriceMid, "italian"),
	}}
	taste := &fakeTasteLoader{profile: domain.TasteProfile{CuisineCodes: []string{"italian"}}}
	userID := uuid.New()

	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(taste, venues, nil))

	ranked, total, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{Page: 1, PerPage: 20}, userID)
	if err != nil {
		t.Fatalf("ListPublicUpcomingForYou: %v", err)
	}
	if total != 4 || len(ranked) != 4 {
		t.Fatalf("total=%d len=%d, want 4/4", total, len(ranked))
	}
	if taste.calls != 1 || taste.gotUser != userID {
		t.Fatalf("taste loader called %d times with user %s, want 1 call with %s", taste.calls, taste.gotUser, userID)
	}

	gotIDs := []uuid.UUID{ranked[0].ID, ranked[1].ID, ranked[2].ID, ranked[3].ID}
	wantIDs := []uuid.UUID{laterMatch.ID, first.ID, second.ID, soonNoMatch.ID}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("order[%d] = %s, want %s (full order: %v, want %v)", i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
		}
	}
	if ranked[0].Match.Score != 400 { // cuisine_match only — the guest set no budget/diet
		t.Fatalf("laterMatch score = %d, want 400", ranked[0].Match.Score)
	}
	if ranked[3].Match.Score != 0 { // kazakh: no shared cuisine, nothing else to score
		t.Fatalf("soonNoMatch score = %d, want 0", ranked[3].Match.Score)
	}
}

// A PLATFORM event (no host venue) is never excluded — it is simply scored
// from the zero domain.VenueTasteSignals and ranked on equal footing.
func TestListPublicUpcomingForYou_PlatformEventScoresZeroSignalsAndIsNotExcluded(t *testing.T) {
	repo := newFakeRepo()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	plat := platformEventItem(uuid.New(), now.Add(time.Hour))
	repo.publicItems = []domain.EventListItem{plat}

	f := NewFacade(repo, &fakePerms{}, &fakeFeed{},
		WithTasteMatch(&fakeTasteLoader{profile: domain.TasteProfile{CuisineCodes: []string{"italian"}}}, &fakeVenueSignals{}, nil))

	ranked, total, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{}, uuid.New())
	if err != nil {
		t.Fatalf("ListPublicUpcomingForYou: %v", err)
	}
	if total != 1 || len(ranked) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(ranked))
	}
	if ranked[0].ID != plat.ID {
		t.Fatalf("platform event excluded: %+v", ranked)
	}
	if ranked[0].Match.Score != 0 {
		t.Fatalf("platform event score = %d, want 0", ranked[0].Match.Score)
	}
}

// editorial_pick (§5.3 row 5) is resolved per the event's OWN effective city
// (its override, else its venue's), against domain.HomePicksAllCities too.
func TestListPublicUpcomingForYou_EditorialPickResolvedPerEffectiveCity(t *testing.T) {
	rid := uuid.New()
	repo := newFakeRepo()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repo.publicItems = []domain.EventListItem{eventItem(uuid.New(), rid, now.Add(time.Hour), domain.CityAlmaty)}

	venues := &fakeVenueSignals{byID: map[uuid.UUID]domain.RestaurantListItem{
		rid: venueItem(rid, false, domain.PriceMid),
	}}
	homePicks := &fakeHomePicksReader{byCity: map[string][]uuid.UUID{string(domain.CityAlmaty): {rid}}}
	taste := &fakeTasteLoader{}

	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(taste, venues, homePicks))

	ranked, _, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{}, uuid.New())
	if err != nil {
		t.Fatalf("ListPublicUpcomingForYou: %v", err)
	}
	if ranked[0].Match.Score != 150 {
		t.Fatalf("score = %d, want 150 (editorial_pick only)", ranked[0].Match.Score)
	}
	foundReason := false
	for _, r := range ranked[0].Match.Reasons {
		if r.Code == domain.TasteSignalEditorialPick && r.Points == 150 {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("editorial_pick reason missing: %+v", ranked[0].Match.Reasons)
	}
}

// Missing WithTasteMatch wiring (every pre-existing NewFacade call in this
// package) must degrade to the plain listing's own order rather than panic —
// same documented posture as WithSeriesContent's own missing dependency.
func TestListPublicUpcomingForYou_WithoutWiringFallsBackToPlainOrder(t *testing.T) {
	repo := newFakeRepo()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	a := eventItem(uuid.New(), uuid.New(), now, domain.CityAlmaty)
	b := eventItem(uuid.New(), uuid.New(), now.Add(time.Hour), domain.CityAlmaty)
	repo.publicItems = []domain.EventListItem{a, b}

	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}) // no WithTasteMatch

	ranked, total, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{}, uuid.New())
	if err != nil {
		t.Fatalf("ListPublicUpcomingForYou: %v", err)
	}
	if total != 2 || len(ranked) != 2 || ranked[0].ID != a.ID || ranked[1].ID != b.ID {
		t.Fatalf("ranked = %+v, want the repository's own order [a,b]", ranked)
	}
	if ranked[0].Match.Score != 0 || ranked[0].Match.Reasons != nil {
		t.Fatalf("unwired match must be the zero value, got %+v", ranked[0].Match)
	}
}

// A profile load failure must not turn a listing into a 5xx — it degrades to
// the plain date order, exactly the posture spec criterion 10 documents for
// /restaurants/picks.
func TestListPublicUpcomingForYou_ProfileLoadErrorFallsBackWithoutFailing(t *testing.T) {
	repo := newFakeRepo()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	a := eventItem(uuid.New(), uuid.New(), now, domain.CityAlmaty)
	repo.publicItems = []domain.EventListItem{a}

	taste := &fakeTasteLoader{err: errors.New("profile store unavailable")}
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(taste, &fakeVenueSignals{}, nil))

	ranked, total, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{}, uuid.New())
	if err != nil {
		t.Fatalf("a profile load failure must not become a facade error, got %v", err)
	}
	if total != 1 || len(ranked) != 1 || ranked[0].ID != a.ID {
		t.Fatalf("ranked = %+v, want the single event still returned", ranked)
	}
}

// An inverted date range is refused before anything is scored — same
// validation ListPublicUpcoming already enforces.
func TestListPublicUpcomingForYou_InvertedRangeRejected(t *testing.T) {
	from := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(-24 * time.Hour)
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(&fakeTasteLoader{}, &fakeVenueSignals{}, nil))

	_, _, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{From: &from, To: &to}, uuid.New())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("to before from must be ErrValidation, got %v", err)
	}
	if repo.publicCalls != 0 {
		t.Fatal("an invalid range must not reach the repository")
	}
}

// A restaurant-signals read failure propagates as a real error — a broken
// ranking read must not silently become an empty or unranked page.
func TestListPublicUpcomingForYou_VenueSignalsErrorPropagates(t *testing.T) {
	repo := newFakeRepo()
	rid := uuid.New()
	repo.publicItems = []domain.EventListItem{eventItem(uuid.New(), rid, time.Now(), domain.CityAlmaty)}
	venues := &fakeVenueSignals{err: errors.New("db down")}
	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(&fakeTasteLoader{}, venues, nil))

	if _, _, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{}, uuid.New()); err == nil {
		t.Fatal("a venue-signals failure must not be swallowed")
	}
}

// Pagination slices the ALREADY-ranked order — a page boundary must not
// re-run the ranking or disagree with the unpaginated order.
func TestListPublicUpcomingForYou_Pagination(t *testing.T) {
	repo := newFakeRepo()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	items := make([]domain.EventListItem, 0, 5)
	for i := 0; i < 5; i++ {
		items = append(items, eventItem(uuid.New(), uuid.New(), now.Add(time.Duration(i)*time.Hour), domain.CityAlmaty))
	}
	repo.publicItems = items

	f := NewFacade(repo, &fakePerms{}, &fakeFeed{}, WithTasteMatch(&fakeTasteLoader{}, &fakeVenueSignals{}, nil))

	page1, total, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{Page: 1, PerPage: 2}, uuid.New())
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if total != 5 || len(page1) != 2 {
		t.Fatalf("page1 total=%d len=%d, want 5/2", total, len(page1))
	}
	page2, _, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{Page: 2, PerPage: 2}, uuid.New())
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != items[2].ID {
		t.Fatalf("page2 = %+v, want to continue right after page1", page2)
	}
	page3, _, err := f.ListPublicUpcomingForYou(context.Background(), domain.PublicEventFilter{Page: 3, PerPage: 2}, uuid.New())
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != items[4].ID {
		t.Fatalf("page3 = %+v, want the last remaining item", page3)
	}
}

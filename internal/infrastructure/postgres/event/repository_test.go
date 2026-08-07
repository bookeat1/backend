package event

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func mkEvent(rid uuid.UUID, status domain.EventStatus, startsIn, dur time.Duration) *domain.Event {
	start := time.Now().Add(startsIn).UTC().Truncate(time.Second)
	return &domain.Event{
		RestaurantID: rid,
		Title:        "E",
		StartsAt:     start,
		EndsAt:       start.Add(dur),
		Status:       status,
	}
}

func TestListPublishedUpcoming_OnlyPublishedAndNotEnded(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	// published + upcoming: SHOWN
	up := mkEvent(rid, domain.EventPublished, 24*time.Hour, 2*time.Hour)
	// published but already ended: HIDDEN
	past := mkEvent(rid, domain.EventPublished, -48*time.Hour, 2*time.Hour)
	// draft upcoming: HIDDEN
	draft := mkEvent(rid, domain.EventDraft, 24*time.Hour, 2*time.Hour)
	// hidden upcoming: HIDDEN
	hidden := mkEvent(rid, domain.EventHidden, 24*time.Hour, 2*time.Hour)
	// published, in progress (started, not yet ended): SHOWN
	ongoing := mkEvent(rid, domain.EventPublished, -1*time.Hour, 2*time.Hour)
	for _, e := range []*domain.Event{up, past, draft, hidden, ongoing} {
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("create event: %v", err)
		}
	}

	items, total, err := repo.ListPublishedUpcoming(ctx, rid, time.Now(), 1, 20)
	if err != nil {
		t.Fatalf("ListPublishedUpcoming: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("expected exactly 2 visible events (upcoming + ongoing), got total=%d len=%d", total, len(items))
	}
	// Stable order: soonest start first → ongoing (started 1h ago) before up (in 24h).
	if items[0].ID != ongoing.ID || items[1].ID != up.ID {
		t.Fatalf("unexpected order: %s, %s", items[0].ID, items[1].ID)
	}
}

func TestCreate_UnknownRestaurantIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	repo := New(pool)

	e := mkEvent(uuid.New(), domain.EventDraft, time.Hour, time.Hour)
	if err := repo.Create(ctx, e); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("create against unknown restaurant must be ErrNotFound, got %v", err)
	}
}

func TestUpdateDelete_RoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	price := int64(500000)
	cap := 40
	e := mkEvent(rid, domain.EventDraft, 24*time.Hour, 2*time.Hour)
	e.TitleI18n = domain.I18n{"en": "Wine Night"}
	e.Ticketed = true
	e.TicketPriceMinor = &price
	e.Capacity = &cap
	e.TicketsRefundable = true
	e.TicketRefundCutoffMinutes = 180
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TitleI18n["en"] != "Wine Night" || !got.Ticketed || got.TicketPriceMinor == nil || *got.TicketPriceMinor != price || got.Capacity == nil || *got.Capacity != cap {
		t.Fatalf("carried fields not persisted: %+v", got)
	}

	if got.TicketRefundPolicy() != (domain.TicketRefundPolicy{Refundable: true, CutoffMinutes: 180}) {
		t.Fatalf("refund policy not persisted: %+v", got.TicketRefundPolicy())
	}

	got.Status = domain.EventPublished
	got.Title = "Renamed"
	// The venue tightens its rules: a full-replace Update must carry them.
	got.TicketsRefundable = false
	got.TicketRefundCutoffMinutes = 0
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := repo.GetByID(ctx, e.ID)
	if after.TicketRefundPolicy() != (domain.TicketRefundPolicy{Refundable: false, CutoffMinutes: 0}) {
		t.Fatalf("refund policy not updated: %+v", after.TicketRefundPolicy())
	}
	if after.Status != domain.EventPublished || after.Title != "Renamed" {
		t.Fatalf("update not persisted: %+v", after)
	}

	if err := repo.Delete(ctx, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, e.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted event must be NotFound, got %v", err)
	}
	if err := repo.Delete(ctx, e.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete must be NotFound, got %v", err)
	}
}

// --- cross-venue public listing (Explore screen) ---

func seedRestaurantIn(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, city domain.City, active bool) uuid.UUID {
	t.Helper()
	repo := restaurant.New(pool)
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: city, PriceCategory: domain.PriceMid, IsActive: active}
	if err := repo.Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant %s: %v", name, err)
	}
	return r.ID
}

// publicFixture builds the world the listing is read against: two active venues
// in different cities plus one deactivated venue, each with a mix of visible and
// invisible events.
type publicFixture struct {
	almaty, astana, closed uuid.UUID
	// visible, soonest first
	soonAlmaty, laterAstana, ongoingAlmaty *domain.Event
	// invisible, one per reason
	draft, hidden, finished, atClosedVenue *domain.Event
}

func seedPublicFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) publicFixture {
	t.Helper()
	repo := New(pool)
	f := publicFixture{
		almaty: seedRestaurantIn(ctx, t, pool, "Almaty Bistro", domain.CityAlmaty, true),
		astana: seedRestaurantIn(ctx, t, pool, "Astana Grill", domain.CityAstana, true),
		closed: seedRestaurantIn(ctx, t, pool, "Closed Place", domain.CityAlmaty, false),
	}
	f.ongoingAlmaty = mkEvent(f.almaty, domain.EventPublished, -1*time.Hour, 3*time.Hour)
	f.soonAlmaty = mkEvent(f.almaty, domain.EventPublished, 24*time.Hour, 2*time.Hour)
	f.laterAstana = mkEvent(f.astana, domain.EventPublished, 72*time.Hour, 2*time.Hour)
	f.draft = mkEvent(f.almaty, domain.EventDraft, 24*time.Hour, 2*time.Hour)
	f.hidden = mkEvent(f.almaty, domain.EventHidden, 24*time.Hour, 2*time.Hour)
	f.finished = mkEvent(f.almaty, domain.EventPublished, -48*time.Hour, 2*time.Hour)
	f.atClosedVenue = mkEvent(f.closed, domain.EventPublished, 24*time.Hour, 2*time.Hour)
	for _, e := range []*domain.Event{
		f.ongoingAlmaty, f.soonAlmaty, f.laterAstana, f.draft, f.hidden, f.finished, f.atClosedVenue,
	} {
		if err := repo.Create(ctx, e); err != nil {
			t.Fatalf("create event: %v", err)
		}
	}
	return f
}

func ids(items []domain.EventListItem) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func sameIDs(got []domain.EventListItem, want []uuid.UUID) bool {
	g := ids(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// The heart of the endpoint: what a guest may see, and in what order. Each row
// is a filter; the expectation is the exact id sequence, so ordering is asserted
// everywhere, not only in the "sorting" row.
func TestListPublicUpcoming_VisibilityFiltersAndOrder(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	fx := seedPublicFixture(ctx, t, pool)
	repo := New(pool)
	now := time.Now()

	almaty := domain.CityAlmaty
	astana := domain.CityAstana
	unknownCity := domain.City("Атлантида")
	otherVenue := fx.astana
	// Windows relative to the fixture's start offsets (-1h, +24h, +72h).
	inTwoDays := now.Add(48 * time.Hour)
	inFourDays := now.Add(96 * time.Hour)

	cases := []struct {
		name   string
		filter domain.PublicEventFilter
		want   []uuid.UUID
	}{
		{
			// Only published + not finished + at an active venue, soonest first.
			name:   "no filters: only visible events, soonest first",
			filter: domain.PublicEventFilter{},
			want:   []uuid.UUID{fx.ongoingAlmaty.ID, fx.soonAlmaty.ID, fx.laterAstana.ID},
		},
		{
			name:   "city narrows by the HOST venue's city",
			filter: domain.PublicEventFilter{City: &almaty},
			want:   []uuid.UUID{fx.ongoingAlmaty.ID, fx.soonAlmaty.ID},
		},
		{
			name:   "city: the other city",
			filter: domain.PublicEventFilter{City: &astana},
			want:   []uuid.UUID{fx.laterAstana.ID},
		},
		{
			name:   "unknown city matches nothing",
			filter: domain.PublicEventFilter{City: &unknownCity},
			want:   nil,
		},
		{
			name:   "restaurant id narrows to one venue",
			filter: domain.PublicEventFilter{RestaurantID: &otherVenue},
			want:   []uuid.UUID{fx.laterAstana.ID},
		},
		{
			name:   "from: only events starting at or after the bound",
			filter: domain.PublicEventFilter{From: &inTwoDays},
			want:   []uuid.UUID{fx.laterAstana.ID},
		},
		{
			name:   "to: only events starting at or before the bound",
			filter: domain.PublicEventFilter{To: &inTwoDays},
			want:   []uuid.UUID{fx.ongoingAlmaty.ID, fx.soonAlmaty.ID},
		},
		{
			name:   "from+to: a window",
			filter: domain.PublicEventFilter{From: &inTwoDays, To: &inFourDays},
			want:   []uuid.UUID{fx.laterAstana.ID},
		},
		{
			name:   "filters combine (AND), not widen",
			filter: domain.PublicEventFilter{City: &almaty, From: &inTwoDays},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total, err := repo.ListPublicUpcoming(ctx, tc.filter, now)
			if err != nil {
				t.Fatalf("ListPublicUpcoming: %v", err)
			}
			if total != len(tc.want) {
				t.Fatalf("total = %d, want %d", total, len(tc.want))
			}
			if !sameIDs(items, tc.want) {
				t.Fatalf("ids = %v, want %v", ids(items), tc.want)
			}
		})
	}
}

// The invisible events must be invisible for the RIGHT reason — spelled out one
// by one so a future change that, say, starts serving hidden events fails here
// with a readable message.
func TestListPublicUpcoming_ExcludesEachKindOfInvisibleEvent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	fx := seedPublicFixture(ctx, t, pool)
	repo := New(pool)

	items, _, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{PerPage: 100}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, it := range items {
		got[it.ID] = true
	}
	for reason, id := range map[string]uuid.UUID{
		"a draft event":                   fx.draft.ID,
		"an event withdrawn from view":    fx.hidden.ID,
		"an event that already finished":  fx.finished.ID,
		"an event at a deactivated venue": fx.atClosedVenue.ID,
	} {
		if got[id] {
			t.Errorf("%s must not be listed publicly (%s)", reason, id)
		}
	}
}

// Each item carries the venue the Explore card renders, so the app needs no
// second query per row.
func TestListPublicUpcoming_CarriesTheHostVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	fx := seedPublicFixture(ctx, t, pool)
	repo := New(pool)

	astana := domain.CityAstana
	items, _, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{City: &astana}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	v := items[0].Restaurant
	if v.ID != fx.astana || v.Name != "Astana Grill" || v.City != domain.CityAstana {
		t.Fatalf("venue = %+v, want the Astana venue", v)
	}
	if v.ID != items[0].RestaurantID {
		t.Fatalf("venue id %s does not match the event's restaurant_id %s", v.ID, items[0].RestaurantID)
	}
}

// Pagination walks the same total in the same order, with no row served twice
// and none skipped.
func TestListPublicUpcoming_Pagination(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	fx := seedPublicFixture(ctx, t, pool)
	repo := New(pool)
	now := time.Now()

	want := []uuid.UUID{fx.ongoingAlmaty.ID, fx.soonAlmaty.ID, fx.laterAstana.ID}
	var seen []uuid.UUID
	for page := 1; page <= 3; page++ {
		items, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{Page: page, PerPage: 2}, now)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if total != 3 {
			t.Fatalf("page %d: total = %d, want 3 (the full count, not the page size)", page, total)
		}
		seen = append(seen, ids(items)...)
	}
	if len(seen) != 3 {
		t.Fatalf("walked %d rows across 3 pages, want 3: %v", len(seen), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paginated order = %v, want %v", seen, want)
		}
	}
}

// Tags round-trip through every read path: a set list survives create→get and a
// full-replace update, an event created without tags reads back as a non-nil
// empty slice (never nil), and the cross-venue listing carries the chips so the
// «Афиша» card needs no follow-up query.
func TestTags_RoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "events", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Bistro")
	repo := New(pool)

	// Created WITH tags: they survive create→get.
	tagged := mkEvent(rid, domain.EventPublished, 24*time.Hour, 2*time.Hour)
	tagged.Tags = []string{"Бранч", "Живая музыка"}
	if err := repo.Create(ctx, tagged); err != nil {
		t.Fatalf("create tagged: %v", err)
	}
	got, err := repo.GetByID(ctx, tagged.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Бранч" || got.Tags[1] != "Живая музыка" {
		t.Fatalf("tags not persisted in order: %#v", got.Tags)
	}

	// Created WITHOUT tags: reads back as a non-nil empty slice, never nil.
	untagged := mkEvent(rid, domain.EventPublished, 48*time.Hour, 2*time.Hour)
	if err := repo.Create(ctx, untagged); err != nil {
		t.Fatalf("create untagged: %v", err)
	}
	gotEmpty, err := repo.GetByID(ctx, untagged.ID)
	if err != nil {
		t.Fatalf("get untagged: %v", err)
	}
	if gotEmpty.Tags == nil {
		t.Fatalf("empty tags read back as nil, want an empty slice")
	}
	if len(gotEmpty.Tags) != 0 {
		t.Fatalf("tags = %#v, want empty", gotEmpty.Tags)
	}

	// Full-replace update rewrites the list.
	got.Tags = []string{"Коктейли"}
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := repo.GetByID(ctx, tagged.ID)
	if len(after.Tags) != 1 || after.Tags[0] != "Коктейли" {
		t.Fatalf("tags not updated: %#v", after.Tags)
	}

	// The cross-venue public listing carries the chips inline.
	items, _, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{RestaurantID: &rid}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming: %v", err)
	}
	byID := map[uuid.UUID][]string{}
	for _, it := range items {
		if it.Tags == nil {
			t.Fatalf("listing item %s carries nil tags, want an empty slice at least", it.ID)
		}
		byID[it.ID] = it.Tags
	}
	if tags := byID[tagged.ID]; len(tags) != 1 || tags[0] != "Коктейли" {
		t.Fatalf("listing tags for tagged event = %#v, want [Коктейли]", tags)
	}
	if tags, ok := byID[untagged.ID]; !ok || len(tags) != 0 {
		t.Fatalf("listing tags for untagged event = %#v (present=%v), want empty", tags, ok)
	}
}

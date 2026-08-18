package eventrecurrence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

var tables = []string{
	"event_recurrence_skips", "event_images", "events", "event_recurrences", "restaurants",
}

func seedRestaurant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name, tz string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var tzArg any
	if tz != "" {
		tzArg = tz
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active, timezone)
		 VALUES ($1, $2, 'almaty', 'mid', true, $3)`, id, name, tzArg); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func fullRule(rid uuid.UUID) *domain.EventRecurrence {
	cover := "https://cdn.example/cover.jpg"
	price := int64(500000)
	capacity := 40
	until := domain.CalendarDate{Year: 2026, Month: time.December, Day: 31}
	return &domain.EventRecurrence{
		RestaurantID:              rid,
		Title:                     "Cocktail Wednesday",
		TitleI18n:                 domain.I18n{"en": "Cocktail Wednesday", "kk": "Коктейль сәрсенбі"},
		Description:               "Каждую среду",
		DescriptionI18n:           domain.I18n{"en": "Every Wednesday"},
		Venue:                     "rooftop terrace",
		CoverImageURL:             &cover,
		Tags:                      []string{"Коктейли", "Живая музыка"},
		OccurrenceStatus:          domain.EventPublished,
		Ticketed:                  true,
		TicketPriceMinor:          &price,
		Capacity:                  &capacity,
		TicketsRefundable:         true,
		TicketRefundCutoffMinutes: 240,
		Frequency:                 domain.RecurrenceWeekly,
		Weekdays:                  []domain.ISOWeekday{3, 5},
		StartMinutes:              19*60 + 30,
		DurationMinutes:           180,
		Timezone:                  "Asia/Almaty",
		StartsOn:                  domain.CalendarDate{Year: 2026, Month: time.August, Day: 17},
		UntilDate:                 &until,
		IsActive:                  true,
	}
}

// Every field must survive the round trip. A rule is a template that will be
// copied onto dozens of real events, so a column silently lost here becomes
// dozens of wrong events, not one.
func TestCreateAndGetRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Rooftop", "Asia/Almaty")
	repo := New(pool)

	want := fullRule(rid)
	if err := repo.Create(ctx, want); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Title != want.Title || got.Venue != want.Venue || got.Description != want.Description {
		t.Fatalf("text fields lost: %+v", got)
	}
	if got.TitleI18n["kk"] != "Коктейль сәрсенбі" || got.DescriptionI18n["en"] != "Every Wednesday" {
		t.Fatalf("i18n lost: %+v / %+v", got.TitleI18n, got.DescriptionI18n)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Коктейли" {
		t.Fatalf("tags lost: %v", got.Tags)
	}
	if got.CoverImageURL == nil || *got.CoverImageURL != *want.CoverImageURL {
		t.Fatalf("cover lost: %v", got.CoverImageURL)
	}
	if !got.Ticketed || got.TicketPriceMinor == nil || *got.TicketPriceMinor != 500000 {
		t.Fatalf("ticket settings lost: %+v", got)
	}
	if got.Capacity == nil || *got.Capacity != 40 {
		t.Fatalf("capacity lost: %v", got.Capacity)
	}
	if !got.TicketsRefundable || got.TicketRefundCutoffMinutes != 240 {
		t.Fatalf("refund policy lost: %+v", got)
	}
	if len(got.Weekdays) != 2 || got.Weekdays[0] != 3 || got.Weekdays[1] != 5 {
		t.Fatalf("weekdays lost: %v", got.Weekdays)
	}
	if got.StartMinutes != 19*60+30 || got.DurationMinutes != 180 {
		t.Fatalf("schedule lost: %+v", got)
	}
	if got.Timezone != "Asia/Almaty" {
		t.Fatalf("timezone lost: %q", got.Timezone)
	}
	if got.StartsOn != (domain.CalendarDate{Year: 2026, Month: time.August, Day: 17}) {
		t.Fatalf("starts_on lost: %v", got.StartsOn)
	}
	if got.UntilDate == nil || *got.UntilDate != (domain.CalendarDate{Year: 2026, Month: time.December, Day: 31}) {
		t.Fatalf("until_date lost: %v", got.UntilDate)
	}
	if !got.IsActive || got.OccurrenceStatus != domain.EventPublished {
		t.Fatalf("flags lost: %+v", got)
	}
}

// "No zone override" is stored as SQL NULL and read back as the empty string —
// never as "", which time.LoadLocation would happily read as UTC.
func TestTimezoneOverrideIsNullWhenAbsent(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "No zone", "Asia/Almaty")
	repo := New(pool)

	rec := fullRule(rid)
	rec.Timezone = ""
	rec.UntilDate = nil
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	var stored *string
	if err := pool.QueryRow(ctx, `SELECT timezone FROM event_recurrences WHERE id=$1`, rec.ID).Scan(&stored); err != nil {
		t.Fatalf("read timezone: %v", err)
	}
	if stored != nil {
		t.Fatalf("an absent override must be NULL, got %q", *stored)
	}
	got, err := repo.GetByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Timezone != "" || got.UntilDate != nil {
		t.Fatalf("want empty zone and no until-date, got %q / %v", got.Timezone, got.UntilDate)
	}
}

func TestCreateUnknownRestaurantIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()

	err := New(pool).Create(ctx, fullRule(uuid.New()))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unknown venue, got %v", err)
	}
}

func TestGetUpdateSetActiveMissingIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	repo := New(pool)
	missing := uuid.New()

	if _, err := repo.GetByID(ctx, missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get: want ErrNotFound, got %v", err)
	}
	rec := fullRule(uuid.New())
	rec.ID = missing
	if err := repo.Update(ctx, rec); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update: want ErrNotFound, got %v", err)
	}
	if err := repo.SetActive(ctx, missing, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("set active: want ErrNotFound, got %v", err)
	}
}

func TestListByRestaurantIsScopedAndPaginated(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	mine := seedRestaurant(ctx, t, pool, "Mine", "Asia/Almaty")
	theirs := seedRestaurant(ctx, t, pool, "Theirs", "Asia/Almaty")
	repo := New(pool)

	for i := 0; i < 3; i++ {
		if err := repo.Create(ctx, fullRule(mine)); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := repo.Create(ctx, fullRule(theirs)); err != nil {
		t.Fatalf("create: %v", err)
	}

	items, total, err := repo.ListByRestaurant(ctx, mine, 1, 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 || len(items) != 2 {
		t.Fatalf("want total 3 and a page of 2, got %d / %d", total, len(items))
	}
	for _, it := range items {
		if it.RestaurantID != mine {
			t.Fatal("another venue's rule leaked into the listing")
		}
	}
	page2, _, err := repo.ListByRestaurant(ctx, mine, 2, 2)
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("want 1 rule on page 2, got %d", len(page2))
	}
}

// ListActive answers the generator's question: active rules at active venues,
// with the VENUE's zone alongside so the worker never needs a second query.
func TestListActiveCarriesVenueTimezoneAndSkipsInactive(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	withZone := seedRestaurant(ctx, t, pool, "Zoned", "Asia/Almaty")
	noZone := seedRestaurant(ctx, t, pool, "Zoneless", "")
	repo := New(pool)

	a := fullRule(withZone)
	a.Timezone = ""
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b := fullRule(noZone)
	b.Timezone = ""
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("create b: %v", err)
	}
	inactive := fullRule(withZone)
	inactive.IsActive = false
	if err := repo.Create(ctx, inactive); err != nil {
		t.Fatalf("create inactive: %v", err)
	}

	got, err := repo.ListActive(ctx, uuid.Nil, 100)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 active rules, got %d", len(got))
	}
	byID := map[uuid.UUID]domain.ActiveEventRecurrence{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if byID[a.ID].VenueTimezone != "Asia/Almaty" {
		t.Fatalf("venue zone not joined in: %q", byID[a.ID].VenueTimezone)
	}
	if byID[b.ID].VenueTimezone != "" {
		t.Fatalf("a zoneless venue must report an empty zone, got %q", byID[b.ID].VenueTimezone)
	}
	if _, ok := byID[inactive.ID]; ok {
		t.Fatal("an inactive rule must not be listed")
	}
}

// Keyset pagination must not skip or repeat a rule.
func TestListActiveKeysetPagination(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Many", "Asia/Almaty")
	repo := New(pool)
	want := map[uuid.UUID]bool{}
	for i := 0; i < 5; i++ {
		rec := fullRule(rid)
		if err := repo.Create(ctx, rec); err != nil {
			t.Fatalf("create: %v", err)
		}
		want[rec.ID] = true
	}

	seen := map[uuid.UUID]bool{}
	after := uuid.Nil
	for {
		page, err := repo.ListActive(ctx, after, 2)
		if err != nil {
			t.Fatalf("list active: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			if seen[r.ID] {
				t.Fatalf("rule %s returned twice", r.ID)
			}
			seen[r.ID] = true
			after = r.ID
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("keyset walk saw %d rules, want %d", len(seen), len(want))
	}
}

// A skip for a rule that no longer exists is a no-op, not an FK error: by then
// nothing generates that slot anyway.
func TestRecordSkipForMissingRuleIsNoop(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()

	if err := New(pool).RecordSkip(ctx, uuid.New(), time.Now()); err != nil {
		t.Fatalf("recording a skip for a vanished rule must be a no-op, got %v", err)
	}
}

// An empty slot list must not produce a statement at all — the generator calls
// this on every tick for rules whose window is already full.
func TestInsertOccurrencesWithNoSlots(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "Empty", "Asia/Almaty")
	repo := New(pool)
	rec := fullRule(rid)
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := repo.InsertOccurrences(ctx, rec, nil)
	if err != nil || n != 0 {
		t.Fatalf("want 0 rows and no error, got %d / %v", n, err)
	}
}

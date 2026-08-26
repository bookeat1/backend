package event

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/migrations"
)

// The city of an event is decided in SQL — COALESCE(e.city, r.city) across the
// join the listing already does, plus a trigger that canonicalizes the override
// — so it is tested against real Postgres. A fake would only re-run the Go code
// that has no say in it.

var cityTables = []string{"event_recurrence_skips", "event_images", "events", "event_recurrences", "restaurants"}

func seedVenueIn(ctx context.Context, t *testing.T, pool sqltx.Querier, name string, city domain.City) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: city, PriceCategory: domain.PriceMid, IsActive: true}
	if err := restaurant.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant %s: %v", name, err)
	}
	return r.ID
}

func seedEventIn(ctx context.Context, t *testing.T, repo *Repository, rid uuid.UUID, title string, city *string) uuid.UUID {
	t.Helper()
	e := mkEvent(rid, domain.EventPublished, 24*time.Hour, 2*time.Hour)
	e.Title = title
	if city != nil {
		c := domain.City(*city)
		e.City = &c
	}
	if err := repo.Create(ctx, e); err != nil {
		t.Fatalf("create event %s: %v", title, err)
	}
	return e.ID
}

// titlesFor runs the listing with a city filter and returns the titles it
// answered with, so a test reads as the guest's screen does.
func titlesFor(ctx context.Context, t *testing.T, repo *Repository, city string) []string {
	t.Helper()
	c := domain.City(city)
	items, total, err := repo.ListPublicUpcoming(ctx, domain.PublicEventFilter{City: &c}, time.Now())
	if err != nil {
		t.Fatalf("ListPublicUpcoming(city=%s): %v", city, err)
	}
	if total != len(items) {
		t.Fatalf("total %d disagrees with the %d rows returned", total, len(items))
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func assertTitles(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestListPublicUpcoming_EventLivesInItsVenueCity is the characterization test
// for what the Афиша did before migration 0084 and must keep doing after it: an
// event with no city of its own belongs to the city of the venue hosting it,
// and is invisible from any other city.
func TestListPublicUpcoming_EventLivesInItsVenueCity(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	almatyVenue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astanaVenue := seedVenueIn(ctx, t, pool, "Bistro Astana", domain.CityAstana)
	seedEventIn(ctx, t, repo, almatyVenue, "Ужин в Алматы", nil)
	seedEventIn(ctx, t, repo, astanaVenue, "Ужин в Астане", nil)

	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Ужин в Алматы")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)), "Ужин в Астане")
}

// TestListPublicUpcoming_CityOverrideBeatsTheVenue: an event pinned to a city
// is shown there and NOWHERE else, whatever its host venue says. This is the
// half that did not exist before — until now the venue's city was the only
// answer possible, and an Almaty venue could not run a date in Astana.
func TestListPublicUpcoming_CityOverrideBeatsTheVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astana := string(domain.CityAstana)
	seedEventIn(ctx, t, repo, venue, "Гастроли в Астане", &astana)
	seedEventIn(ctx, t, repo, venue, "Дома в Алматы", nil)

	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)), "Гастроли в Астане")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Дома в Алматы")
}

// TestEventCityOverrideIsCanonicalizedOnWrite: the listing compares cities as
// exact strings, so an override written as a code or a historical name has to
// be stored as the dictionary's spelling — otherwise the event would be linked
// to the right city and found by no filter at all. The trigger
// trg_events_sync_city does it for every writer, not just ours.
func TestEventCityOverrideIsCanonicalizedOnWrite(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)
	venue := seedVenueIn(ctx, t, pool, "Bistro Astana", domain.CityAstana)

	for _, written := range []string{"almaty", "alma-ata", "Алма-Ата", "  алматы  "} {
		t.Run(written, func(t *testing.T) {
			id := seedEventIn(ctx, t, repo, venue, "Гастроли "+written, &written)
			t.Cleanup(func() {
				if _, err := pool.Exec(ctx, `DELETE FROM events WHERE id=$1`, id); err != nil {
					t.Errorf("cleanup event: %v", err)
				}
			})

			var stored string
			var linked *uuid.UUID
			if err := pool.QueryRow(ctx, `SELECT city, city_id FROM events WHERE id=$1`, id).
				Scan(&stored, &linked); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored != string(domain.CityAlmaty) {
				t.Fatalf("stored city = %q, want %q", stored, domain.CityAlmaty)
			}
			if linked == nil {
				t.Fatal("the override was not linked to a dictionary entry")
			}
			assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Гастроли "+written)
		})
	}
}

// TestEventCityOverrideCanBeCleared: clearing the override must return the
// event to its venue's city, and must leave ONE representation of "no city" —
// a blank string would otherwise become a city nothing matches.
func TestEventCityOverrideCanBeCleared(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astana := string(domain.CityAstana)
	id := seedEventIn(ctx, t, repo, venue, "Гастроли", &astana)

	e, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	blank := domain.City("   ")
	e.City = &blank
	if err := repo.Update(ctx, e); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var stored *string
	var linked *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT city, city_id FROM events WHERE id=$1`, id).
		Scan(&stored, &linked); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != nil || linked != nil {
		got := "NULL"
		if stored != nil {
			got = strconv.Quote(*stored)
		}
		t.Fatalf("cleared override reads back as city=%s city_id=%v, want NULL/NULL", got, linked)
	}
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Гастроли")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)))
}

// TestRenameCityStringFollowsTheOverride: renaming a city rewrites the string
// stored on every event pinned to it, in the same transaction the venues are
// rewritten in (see usecase/cities). Left behind, an override would advertise a
// spelling the filter no longer produces and the event would simply vanish.
func TestRenameCityStringFollowsTheOverride(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	repo := New(pool)

	venue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astana := string(domain.CityAstana)
	id := seedEventIn(ctx, t, repo, venue, "Гастроли", &astana)

	var cityID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM cities WHERE code='astana'`).Scan(&cityID); err != nil {
		t.Fatalf("read city id: %v", err)
	}
	// The dictionary registers the new spelling as an alias before it rewrites
	// the rows (usecase/cities.Update); reproduce that order here, because the
	// trigger re-resolves the string it is handed.
	if _, err := pool.Exec(ctx,
		`INSERT INTO city_aliases (alias, city_id) VALUES (city_key($2), $1) ON CONFLICT DO NOTHING`,
		cityID, "Целиноград"); err != nil {
		t.Fatalf("register alias: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE cities SET name='Астана' WHERE id=$1`, cityID); err != nil {
			t.Errorf("restore city name: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM city_aliases WHERE alias=city_key('Целиноград')`); err != nil {
			t.Errorf("cleanup alias: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `UPDATE cities SET name='Целиноград' WHERE id=$1`, cityID); err != nil {
		t.Fatalf("rename city: %v", err)
	}

	n, err := repo.RenameCityString(ctx, cityID, "Целиноград")
	if err != nil {
		t.Fatalf("RenameCityString: %v", err)
	}
	if n != 1 {
		t.Fatalf("rewrote %d events, want 1", n)
	}

	var stored string
	var linked uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT city, city_id FROM events WHERE id=$1`, id).
		Scan(&stored, &linked); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "Целиноград" || linked != cityID {
		t.Fatalf("after rename: city=%q city_id=%s, want «Целиноград» still linked to %s", stored, linked, cityID)
	}
	assertTitles(t, titlesFor(ctx, t, repo, "Целиноград"), "Гастроли")
	// The old spelling stays an alias, so a stale client still finds the event —
	// once the usecase resolves it. Here, unresolved, it correctly matches
	// nothing: that resolution is usecase/events' job and is tested there.
	assertTitles(t, titlesFor(ctx, t, repo, "Астана"))
}

// gooseDB opens goose's database/sql handle onto the same TEST_DATABASE_URL the
// pgx pool uses, so the assertions read what the migration really produced.
func gooseDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open goose db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	return db
}

// TestEventCityMigrationIsSafeOnExistingRows is the production question: what
// happens to the events that are already there when 0084 lands?
//
// It rolls 0084 back, writes events the way the pre-0084 code did (there is no
// city column then — raw SQL, because the repository already knows about one),
// and applies the migration again. Those rows must come back with NO city of
// their own and must still be found by exactly their venue's city — that is
// what "existing events keep behaving exactly as today" means, and it is why
// the migration deliberately backfills nothing.
func TestEventCityMigrationIsSafeOnExistingRows(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, cityTables...)
	ctx := context.Background()
	db := gooseDB(t)

	// Whatever happens below, the shared database must be left at the latest
	// version or every package after this one fails.
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	})
	if err := goose.DownContext(ctx, db, "."); err != nil {
		t.Fatalf("goose down: %v", err)
	}

	almatyVenue := seedVenueIn(ctx, t, pool, "Bistro Almaty", domain.CityAlmaty)
	astanaVenue := seedVenueIn(ctx, t, pool, "Bistro Astana", domain.CityAstana)
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	legacy := []struct {
		id    uuid.UUID
		venue uuid.UUID
		title string
	}{
		{uuid.New(), almatyVenue, "Старое в Алматы"},
		{uuid.New(), astanaVenue, "Старое в Астане"},
	}
	for _, e := range legacy {
		if _, err := pool.Exec(ctx,
			`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status)
			 VALUES ($1, $2, $3, $4, $5, 'published')`,
			e.id, e.venue, e.title, start, start.Add(2*time.Hour)); err != nil {
			t.Fatalf("seed pre-migration event: %v", err)
		}
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	var withACity int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE city IS NOT NULL OR city_id IS NOT NULL`).Scan(&withACity); err != nil {
		t.Fatalf("count backfilled: %v", err)
	}
	if withACity != 0 {
		t.Fatalf("%d existing events were given a city of their own; the migration must backfill nothing", withACity)
	}

	repo := New(pool)
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAlmaty)), "Старое в Алматы")
	assertTitles(t, titlesFor(ctx, t, repo, string(domain.CityAstana)), "Старое в Астане")
}

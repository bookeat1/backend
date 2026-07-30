package venuedashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedGuest inserts a booking with a named guest, so the tests can tell the
// rows apart by who is coming rather than by a uuid.
func seedGuest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, venue uuid.UUID,
	name, phone, status string, guests int, createdAt, startsAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, status,
		                       starts_at, ends_at, created_at, updated_at, source)
		 VALUES ($1,$2,$3,$4,$4,$5,$6,$7::timestamptz,
		         $7::timestamptz + interval '90 minutes',$8::timestamptz,$8::timestamptz,'app')`,
		id, venue, name, phone, guests, status, startsAt, createdAt); err != nil {
		t.Fatalf("seed booking %s: %v", name, err)
	}
	return id
}

// The queue is ordered by when the request ARRIVED, oldest first: the guest who
// has waited longest is the one about to give up. Sorting by the visit time
// instead (the intuitive mistake) would bury a two-hour-old request for
// Saturday under a fresh one for tonight.
func TestAwaitingIsOldestFirstAndIncludesFutureDates(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	// Asked first, wants to come last — must still head the queue.
	seedGuest(t, ctx, pool, venue, "Алия", "+77010000001", "pending", 2,
		now.Add(-3*time.Hour), now.Add(96*time.Hour))
	seedGuest(t, ctx, pool, venue, "Берик", "+77010000002", "pending", 4,
		now.Add(-90*time.Minute), now.Add(4*time.Hour))
	seedGuest(t, ctx, pool, venue, "Ерлан", "+77010000003", "pending", 3,
		now.Add(-10*time.Minute), now.Add(2*time.Hour))
	// Already answered — not work, must not show up in the block.
	seedGuest(t, ctx, pool, venue, "Отвеченный", "+77010000004", "confirmed", 2,
		now.Add(-5*time.Hour), now.Add(3*time.Hour))
	seedGuest(t, ctx, pool, venue, "Лист ожидания", "+77010000005", "waitlist", 2,
		now.Add(-6*time.Hour), now.Add(3*time.Hour))

	got, err := NewToday(pool).Today(ctx, venue, now, 20, 50)
	if err != nil {
		t.Fatalf("today: %v", err)
	}

	if len(got.Awaiting) != 3 || got.AwaitingTotal != 3 {
		t.Fatalf("awaiting = %d rows (total %d), want 3; only 'pending' needs an answer: %+v",
			len(got.Awaiting), got.AwaitingTotal, got.Awaiting)
	}
	want := []string{"Алия", "Берик", "Ерлан"}
	for i, n := range want {
		if got.Awaiting[i].Name != n {
			t.Fatalf("awaiting[%d] = %q, want %q (oldest request first)", i, got.Awaiting[i].Name, n)
		}
	}
	if got.Awaiting[0].WaitingMinutes != 180 {
		t.Fatalf("waiting = %d min, want 180 measured from created_at", got.Awaiting[0].WaitingMinutes)
	}
	if got.Awaiting[2].WaitingMinutes != 10 {
		t.Fatalf("waiting = %d min, want 10", got.Awaiting[2].WaitingMinutes)
	}
	if got.Awaiting[0].Phone != "+77010000001" || got.Awaiting[0].Guests != 2 {
		t.Fatalf("the row must carry the guest the venue has to call back: %+v", got.Awaiting[0])
	}
}

// A created_at in the future (clock skew on an import) must read as "just now",
// not as a negative wait rendered to a hostess as "-14 минут".
func TestWaitingMinutesNeverGoNegative(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	seedGuest(t, ctx, pool, venue, "Из будущего", "+77010000009", "pending", 2,
		now.Add(14*time.Minute), now.Add(3*time.Hour))

	got, err := NewToday(pool).Today(ctx, venue, now, 20, 50)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(got.Awaiting) != 1 || got.Awaiting[0].WaitingMinutes != 0 {
		t.Fatalf("want a single row waiting 0 minutes, got %+v", got.Awaiting)
	}
}

// The tenant boundary. Both venues are busy at the same instant; only the asked
// one may appear, in either block.
func TestTodayShowsOnlyTheRequestedVenue(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	mine := seedVenue(t, ctx, pool, "Мой", "Asia/Almaty")
	theirs := seedVenue(t, ctx, pool, "Чужой", "Asia/Almaty")
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	seedGuest(t, ctx, pool, mine, "Мой гость", "+77010000001", "pending", 2,
		now.Add(-time.Hour), now.Add(5*time.Hour))
	seedGuest(t, ctx, pool, mine, "Мой вечер", "+77010000002", "confirmed", 3,
		now.Add(-time.Hour), now.Add(5*time.Hour))
	seedGuest(t, ctx, pool, theirs, "Чужой гость", "+77010000003", "pending", 9,
		now.Add(-4*time.Hour), now.Add(5*time.Hour))
	seedGuest(t, ctx, pool, theirs, "Чужой вечер", "+77010000004", "confirmed", 9,
		now.Add(-4*time.Hour), now.Add(5*time.Hour))

	got, err := NewToday(pool).Today(ctx, mine, now, 20, 50)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(got.Awaiting) != 1 || got.Awaiting[0].Name != "Мой гость" {
		t.Fatalf("another venue's request leaked into the queue: %+v", got.Awaiting)
	}
	if got.AwaitingTotal != 1 || got.TodayTotal != 2 {
		t.Fatalf("totals count the other venue too: awaiting=%d today=%d", got.AwaitingTotal, got.TodayTotal)
	}
	if got.Guests != 5 {
		t.Fatalf("guests = %d, want 5 (2+3, the other venue's 18 must not count)", got.Guests)
	}
}

// The day boundary is the VENUE's, not the server's. At 20:00 UTC an Almaty
// venue (UTC+5) is already on the next calendar day: a 19:30 UTC booking is its
// tonight, and a 10:00 UTC one belongs to the day that just ended. Read in UTC
// both answers flip, which is exactly the bug this guards.
func TestTodayRollsOverOnTheVenuesMidnightNotTheServers(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	// 2026-07-30 20:00 UTC == 2026-07-31 01:00 in Almaty.
	now := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC)
	made := now.Add(-24 * time.Hour)

	// 2026-07-31 00:30 local — the venue's today, though it is still the 30th in UTC.
	seedGuest(t, ctx, pool, venue, "После полуночи", "+77010000001", "arrived", 2,
		made, time.Date(2026, 7, 30, 19, 30, 0, 0, time.UTC))
	// 2026-07-30 15:00 local — yesterday for the venue, today for a UTC reader.
	seedGuest(t, ctx, pool, venue, "Вчерашний обед", "+77010000002", "completed", 5,
		made, time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC))
	// 2026-07-31 22:00 local — later the same venue day.
	seedGuest(t, ctx, pool, venue, "Вечер", "+77010000003", "confirmed", 4,
		made, time.Date(2026, 7, 31, 17, 0, 0, 0, time.UTC))

	got, err := NewToday(pool).Today(ctx, venue, now, 20, 50)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(got.Today) != 2 {
		t.Fatalf("want the venue's local day (2 rows), got %d: %+v", len(got.Today), got.Today)
	}
	if got.Today[0].Name != "После полуночи" || got.Today[1].Name != "Вечер" {
		t.Fatalf("today must be in time order and use the venue's day: %+v", got.Today)
	}
	if got.Guests != 6 {
		t.Fatalf("guests = %d, want 6 (2+4); yesterday's lunch must not be added", got.Guests)
	}
}

// Same instant, a venue five hours west: its local day is still the 30th, so
// the two venues legitimately disagree about what "today" means. This is what
// makes the previous test about the venue's zone rather than about a constant.
func TestTwoVenuesInDifferentZonesSeeDifferentDays(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	almaty := seedVenue(t, ctx, pool, "Алматы", "Asia/Almaty")
	lisbon := seedVenue(t, ctx, pool, "Лиссабон", "Europe/Lisbon")
	now := time.Date(2026, 7, 30, 20, 0, 0, 0, time.UTC) // 31st in Almaty, 30th 21:00 in Lisbon
	made := now.Add(-24 * time.Hour)
	starts := time.Date(2026, 7, 30, 19, 30, 0, 0, time.UTC)

	seedGuest(t, ctx, pool, almaty, "A", "+77010000001", "confirmed", 2, made, starts)
	seedGuest(t, ctx, pool, lisbon, "L", "+77010000002", "confirmed", 2, made, starts)

	repo := NewToday(pool)
	gotAlmaty, err := repo.Today(ctx, almaty, now, 20, 50)
	if err != nil {
		t.Fatalf("almaty: %v", err)
	}
	gotLisbon, err := repo.Today(ctx, lisbon, now, 20, 50)
	if err != nil {
		t.Fatalf("lisbon: %v", err)
	}
	if len(gotAlmaty.Today) != 1 {
		t.Fatalf("Almaty: 00:30 local is today, got %+v", gotAlmaty.Today)
	}
	if len(gotLisbon.Today) != 1 {
		t.Fatalf("Lisbon: 20:30 local is today, got %+v", gotLisbon.Today)
	}
	// Move the clock past Lisbon's own midnight (23:30 UTC == 00:30 on the 31st
	// there). The 19:30 UTC booking was its 20:30 yesterday, so its day is now
	// empty — while for Almaty, four hours further on, the same booking is still
	// today. One instant, two different answers, decided by the venue's zone.
	later := time.Date(2026, 7, 30, 23, 30, 0, 0, time.UTC)
	if g, err := repo.Today(ctx, lisbon, later, 20, 50); err != nil || len(g.Today) != 0 {
		t.Fatalf("Lisbon after its midnight: want an empty day, got %+v (err %v)", g.Today, err)
	}
}

// Cancelled bookings are not work and must not be counted as expected guests —
// the single most damaging way to get this tile wrong is to overstate the room.
// The head count also has to survive the list being truncated.
func TestGuestsCountTheWholeDayEvenWhenTheListIsTruncated(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Abay", "Asia/Almaty")
	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC) // 14:00 Almaty
	made := now.Add(-24 * time.Hour)
	day := func(hourUTC int) time.Time { return time.Date(2026, 7, 30, hourUTC, 0, 0, 0, time.UTC) }

	seedGuest(t, ctx, pool, venue, "Один", "+77010000001", "confirmed", 2, made, day(6))
	seedGuest(t, ctx, pool, venue, "Два", "+77010000002", "arrived", 3, made, day(8))
	seedGuest(t, ctx, pool, venue, "Три", "+77010000003", "pending", 4, made, day(14))
	seedGuest(t, ctx, pool, venue, "Отменён", "+77010000004", "cancelled", 50, made, day(15))

	got, err := NewToday(pool).Today(ctx, venue, now, 20, 2)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if len(got.Today) != 2 {
		t.Fatalf("the limit must be applied to the list, got %d rows", len(got.Today))
	}
	if got.Today[0].Name != "Один" || got.Today[1].Name != "Два" {
		t.Fatalf("today must be in time order: %+v", got.Today)
	}
	if got.TodayTotal != 3 {
		t.Fatalf("today_total = %d, want 3 (what exists, not what fit)", got.TodayTotal)
	}
	if got.Guests != 9 {
		t.Fatalf("guests = %d, want 9 (2+3+4); a cancelled 50-top must not be expected, "+
			"and the limit must not shrink the count", got.Guests)
	}
	// A pending booking for today belongs in BOTH blocks: it needs an answer and
	// it is on the day's list.
	if got.AwaitingTotal != 1 || got.Awaiting[0].Name != "Три" {
		t.Fatalf("a pending booking for today must also be in the queue: %+v", got.Awaiting)
	}
}

// An empty venue must answer with empty lists and zeros — never nil, which
// serialises as `null` and makes a client crash on .length.
func TestEmptyVenueAnswersWithEmptyListsNotNil(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "booking_items", "bookings", "restaurants")
	ctx := context.Background()

	venue := seedVenue(t, ctx, pool, "Пустой", "Asia/Almaty")
	got, err := NewToday(pool).Today(ctx, venue, time.Now().UTC(), 20, 50)
	if err != nil {
		t.Fatalf("today: %v", err)
	}
	if got.Awaiting == nil || got.Today == nil {
		t.Fatalf("lists must be empty, not nil: %+v", got)
	}
	if len(got.Awaiting) != 0 || len(got.Today) != 0 || got.Guests != 0 ||
		got.AwaitingTotal != 0 || got.TodayTotal != 0 {
		t.Fatalf("an empty venue must be all zeros, got %+v", got)
	}
}

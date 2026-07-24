package event

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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

	got.Status = domain.EventPublished
	got.Title = "Renamed"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, _ := repo.GetByID(ctx, e.ID)
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

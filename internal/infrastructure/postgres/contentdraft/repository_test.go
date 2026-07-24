package contentdraft

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

func seedUser(ctx context.Context, t *testing.T, pool sqltx.Querier) uuid.UUID {
	t.Helper()
	u := &domain.User{ID: uuid.New(), FullName: "Reviewer", Role: domain.RoleRestaurant, PreferredLanguage: "ru"}
	if err := userrepo.New(pool).Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier, name string) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{ID: uuid.New(), Name: name, City: domain.CityAlmaty, PriceCategory: domain.PriceMid, IsActive: true}
	if err := restaurant.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func pendingEventDraft(rid uuid.UUID) *domain.ContentDraft {
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	end := start.Add(2 * time.Hour)
	return &domain.ContentDraft{
		RestaurantID:      rid,
		Kind:              domain.DraftKindEvent,
		Source:            domain.ContentSourceInstagram,
		RawPayload:        json.RawMessage(`{"caption":"parsed"}`),
		SuggestedTitle:    "Parsed Event",
		SuggestedStartsAt: &start,
		SuggestedEndsAt:   &end,
	}
}

func TestCreateAndListPending_TenantScoped(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "content_drafts", "events", "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "A")
	other := seedRestaurant(ctx, t, pool, "B")
	repo := New(pool)

	d := pendingEventDraft(rid)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if err := repo.Create(ctx, pendingEventDraft(other)); err != nil {
		t.Fatalf("create other draft: %v", err)
	}

	items, total, err := repo.ListPendingByRestaurant(ctx, rid, 1, 20)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].RestaurantID != rid {
		t.Fatalf("queue must be tenant-scoped: total=%d items=%+v", total, items)
	}
	if items[0].RawPayload == nil {
		t.Fatal("raw_payload must round-trip")
	}
}

func TestMarkApproved_CASOnPending(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "content_drafts", "events", "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "A")
	repo := New(pool)

	d := pendingEventDraft(rid)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("create draft: %v", err)
	}

	// A real event must exist for the approved-kind CHECK + FK.
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	ev := &domain.Event{RestaurantID: rid, Title: "Parsed Event", StartsAt: start, EndsAt: start.Add(2 * time.Hour), Status: domain.EventPublished}
	if err := eventrepo.New(pool).Create(ctx, ev); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	// reviewed_by references users(id) (nullable, ON DELETE SET NULL); a
	// non-null value must reference a real user, so seed one.
	uid := seedUser(ctx, t, pool)
	now := time.Now().UTC().Truncate(time.Second)
	if err := repo.MarkApproved(ctx, d.ID, uid, now, &ev.ID, nil); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	got, _ := repo.GetByID(ctx, d.ID)
	if got.Status != domain.DraftApproved || got.CreatedEventID == nil || *got.CreatedEventID != ev.ID {
		t.Fatalf("approve not persisted: %+v", got)
	}

	// Second approve must lose the CAS (no longer pending).
	if err := repo.MarkApproved(ctx, d.ID, uid, now, &ev.ID, nil); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("re-approving a non-pending draft must be ErrInvalidStatus, got %v", err)
	}
	// Reject after approve also loses the CAS.
	if err := repo.MarkRejected(ctx, d.ID, uid, now); !errors.Is(err, domain.ErrInvalidStatus) {
		t.Fatalf("rejecting an approved draft must be ErrInvalidStatus, got %v", err)
	}
}

func TestMarkApproved_MissingIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "content_drafts", "events", "promos", "restaurants")
	ctx := context.Background()
	repo := New(pool)
	uid := seedUser(ctx, t, pool)
	eid := uuid.New()
	if err := repo.MarkApproved(ctx, uuid.New(), uid, time.Now(), &eid, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("approving a missing draft must be ErrNotFound, got %v", err)
	}
}

func TestMarkRejected_CreatesNothing(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "content_drafts", "events", "promos", "restaurants")
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool, "A")
	repo := New(pool)

	d := pendingEventDraft(rid)
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	uid := seedUser(ctx, t, pool)
	if err := repo.MarkRejected(ctx, d.ID, uid, time.Now().UTC().Truncate(time.Second)); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := repo.GetByID(ctx, d.ID)
	if got.Status != domain.DraftRejected || got.CreatedEventID != nil || got.CreatedPromoID != nil {
		t.Fatalf("reject must create nothing: %+v", got)
	}
}

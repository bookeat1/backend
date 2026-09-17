package notification

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedBooking creates a real booking row so notifications.booking_id's FK is
// satisfied — the feed's booking_id, unlike outbox_event_id, still points at
// a real table.
func seedBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, Name: "Гость", Phone: "+7 (777) 123-45-67",
		Email: "guest@example.com", PhoneNormalized: "+77771234567", Guests: 2,
		StartsAt: time.Now().Add(24 * time.Hour), EndsAt: time.Now().Add(26 * time.Hour),
		Status: domain.BookingPending, Source: domain.SourceApp,
	}
	if err := bookingrepo.New(pool).Create(context.Background(), b); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return b.ID
}

func seedEvent(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO events (id, restaurant_id, title, starts_at, ends_at, status, created_at, updated_at)
		 VALUES ($1,$2,'E', now() + interval '1 day', now() + interval '1 day 3 hours', 'published', now(), now())`,
		id, rid); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return id
}

func seedPromo(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO promos (id, restaurant_id, title, starts_at, ends_at, status, created_at, updated_at)
		 VALUES ($1,$2,'P', now(), now() + interval '7 days', 'published', now(), now())`,
		id, rid); err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	return id
}

func seedCampaign(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO push_campaigns (id, kind, subject_id, status, estimated_recipients, created_at)
		 VALUES ($1,'event',$2,'sending',0, now())`, id, uuid.New()); err != nil {
		t.Fatalf("seed push campaign: %v", err)
	}
	return id
}

// TestFeedInsertBookingRow pins the pre-existing booking path: dedupe on
// (outbox_event_id, user_id) still works exactly as it did before migration
// 0111 widened the table.
func TestFeedInsertBookingRow(t *testing.T) {
	pool := testdb.Connect(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	eventID := seedOutboxEvent(t, pool, rid)
	bookingID := seedBooking(t, pool, rid)
	repo := NewFeed(pool)
	ctx := context.Background()

	n := &domain.Notification{
		UserID: uid, Type: domain.FeedTypeBooking, Title: "t", Body: "b",
		BookingID: &bookingID, RestaurantID: &rid, OutboxEventID: &eventID,
	}
	inserted, err := repo.Insert(ctx, n)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert should report inserted=true")
	}

	// A redelivery of the same outbox event for the same guest is a no-op —
	// the at-least-once dispatcher's own idempotency.
	n2 := &domain.Notification{
		UserID: uid, Type: domain.FeedTypeBooking, Title: "t2", Body: "b2",
		BookingID: &bookingID, RestaurantID: &rid, OutboxEventID: &eventID,
	}
	inserted, err = repo.Insert(ctx, n2)
	if err != nil {
		t.Fatalf("redeliver insert: %v", err)
	}
	if inserted {
		t.Fatal("a redelivered outbox event must not insert a second row")
	}
}

// TestFeedInsertCampaignRow pins criterion 19: a push-campaign row dedupes on
// (campaign_id, user_id), independent of the booking-outbox key, and its
// event_id/promo_id/type round-trip through ListByUser.
func TestFeedInsertCampaignRow(t *testing.T) {
	pool := testdb.Connect(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	campaignID := seedCampaign(t, pool)
	eventID := seedEvent(t, pool, rid)
	repo := NewFeed(pool)
	ctx := context.Background()

	n := &domain.Notification{
		UserID: uid, Type: domain.FeedTypeEvent, Title: "Новое событие", Body: "b",
		RestaurantID: &rid, CampaignID: &campaignID, EventID: &eventID,
	}
	inserted, err := repo.Insert(ctx, n)
	if err != nil {
		t.Fatalf("insert campaign row: %v", err)
	}
	if !inserted {
		t.Fatal("first campaign insert should report inserted=true")
	}

	// A second attempt for the SAME (campaign, user) — e.g. a retried Sender
	// pass — must be a no-op (criterion 19's uniqueness), never a duplicate.
	n2 := &domain.Notification{
		UserID: uid, Type: domain.FeedTypeEvent, Title: "x", Body: "y",
		RestaurantID: &rid, CampaignID: &campaignID, EventID: &eventID,
	}
	inserted, err = repo.Insert(ctx, n2)
	if err != nil {
		t.Fatalf("duplicate campaign insert: %v", err)
	}
	if inserted {
		t.Fatal("a second (campaign_id, user_id) row must not be inserted")
	}

	items, err := repo.ListByUser(ctx, uid, nil, 10)
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	var found *domain.Notification
	for i := range items {
		if items[i].ID == n.ID {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatal("inserted campaign row not found by ListByUser")
	}
	if found.OutboxEventID != nil {
		t.Errorf("campaign row must have outbox_event_id = nil, got %v", found.OutboxEventID)
	}
	if found.CampaignID == nil || *found.CampaignID != campaignID {
		t.Errorf("campaign_id = %v, want %v", found.CampaignID, campaignID)
	}
	if found.EventID == nil || *found.EventID != eventID {
		t.Errorf("event_id = %v, want %v", found.EventID, eventID)
	}
	if found.PromoID != nil {
		t.Errorf("promo_id must be nil for an event campaign row, got %v", found.PromoID)
	}
	if found.Type != domain.FeedTypeEvent {
		t.Errorf("type = %q, want event", found.Type)
	}
}

// TestFeedInsertBookingAndCampaignRowsForSameGuestCoexist proves the two
// producers' unique keys are independent: a guest can have BOTH a booking row
// and a campaign row without either dedupe key colliding with the other.
func TestFeedInsertBookingAndCampaignRowsForSameGuestCoexist(t *testing.T) {
	pool := testdb.Connect(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	eventOutboxID := seedOutboxEvent(t, pool, rid)
	campaignID := seedCampaign(t, pool)
	bookingID := seedBooking(t, pool, rid)
	repo := NewFeed(pool)
	ctx := context.Background()

	if _, err := repo.Insert(ctx, &domain.Notification{
		UserID: uid, Type: domain.FeedTypeBooking, Title: "t", Body: "b",
		BookingID: &bookingID, RestaurantID: &rid, OutboxEventID: &eventOutboxID,
	}); err != nil {
		t.Fatalf("insert booking row: %v", err)
	}
	promoID := seedPromo(t, pool, rid)
	if _, err := repo.Insert(ctx, &domain.Notification{
		UserID: uid, Type: domain.FeedTypePromo, Title: "Акция", Body: "b",
		RestaurantID: &rid, CampaignID: &campaignID, PromoID: &promoID,
	}); err != nil {
		t.Fatalf("insert campaign row: %v", err)
	}

	items, err := repo.ListByUser(ctx, uid, nil, 10)
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (one booking row, one campaign row)", len(items))
	}
}

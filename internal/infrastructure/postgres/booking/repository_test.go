package booking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// bookingTables lists the tables these tests own, children first. Restaurants,
// users, tables and menu items are deliberately NOT truncated: they are seeded
// with fresh UUIDs per test, so this package never wipes state another
// package's integration tests are relying on.
var bookingTables = []string{
	"booking_outbox", "booking_status_history", "restaurant_surveys",
	"booking_rate_log", "booking_blacklist", "booking_messages", "booking_items",
	"booking_tables", "bookings",
}

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, bookingTables...)
	return pool, context.Background()
}

func seedRestaurant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurants (id, name, city, price_category) VALUES ($1,'R','Алматы','₸')`, id); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return id
}

func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
		id, id.String()+"@example.com", "+7777"+id.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedTable(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity) VALUES ($1,$2,'T1',4)`,
		id, rid); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	return id
}

func newBooking(rid uuid.UUID, startsAt time.Time) *domain.Booking {
	return &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, Name: "Гость", Phone: "+7 (777) 123-45-67",
		Email: "guest@example.com", PhoneNormalized: "+77771234567", Guests: 2,
		StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour),
		Status: domain.BookingPending, Source: domain.SourceApp,
	}
}

func ptr[T any](v T) *T { return &v }

func TestBookingCRUDAndList(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	repo := New(pool)

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	b := newBooking(rid, base)
	b.UserID = &uid
	b.Notes = ptr("у окна")
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.BookingPending || got.Guests != 2 ||
		got.UserID == nil || *got.UserID != uid || got.Notes == nil || *got.Notes != "у окна" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if !got.StartsAt.Equal(base) {
		t.Errorf("starts_at = %v, want %v", got.StartsAt, base)
	}

	// Update rewrites mutable fields and leaves created_at/restaurant_id alone.
	b.Guests = 4
	b.Name = "Дамир"
	b.CancelledBy = ptr(domain.CancelledByGuest)
	if err := repo.Update(ctx, b); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = repo.GetByID(ctx, b.ID)
	if got.Guests != 4 || got.Name != "Дамир" || got.CancelledBy == nil ||
		*got.CancelledBy != domain.CancelledByGuest {
		t.Errorf("update mismatch: %+v", got)
	}
	if got.RestaurantID != rid {
		t.Errorf("restaurant_id changed: %v", got.RestaurantID)
	}

	// A second booking outside the day window, to prove the filters bite.
	other := newBooking(rid, base.AddDate(0, 0, 3))
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}

	dayStart := base.Truncate(24 * time.Hour)
	dayEnd := dayStart.AddDate(0, 0, 1)
	items, total, err := repo.List(ctx, domain.BookingFilter{
		RestaurantID: &rid, From: &dayStart, To: &dayEnd,
	})
	if err != nil {
		t.Fatalf("list by day: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != b.ID {
		t.Errorf("list by day = %d rows total=%d, want the single booking", len(items), total)
	}

	// The half-open upper bound must exclude a booking starting exactly at dayEnd.
	edge := newBooking(rid, dayEnd)
	if err := repo.Create(ctx, edge); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	_, total, _ = repo.List(ctx, domain.BookingFilter{RestaurantID: &rid, From: &dayStart, To: &dayEnd})
	if total != 1 {
		t.Errorf("half-open upper bound leaked: total=%d, want 1", total)
	}

	// Status filter.
	_, total, err = repo.List(ctx, domain.BookingFilter{
		RestaurantID: &rid, Statuses: []domain.BookingStatus{domain.BookingCancelled},
	})
	if err != nil || total != 0 {
		t.Errorf("list(cancelled) total=%d err=%v, want 0", total, err)
	}

	// User filter + ordering by starts_at DESC.
	byUser, total, err := repo.List(ctx, domain.BookingFilter{UserID: &uid})
	if err != nil || total != 1 || len(byUser) != 1 || byUser[0].ID != b.ID {
		t.Errorf("list by user total=%d err=%v", total, err)
	}

	all, total, err := repo.List(ctx, domain.BookingFilter{RestaurantID: &rid, PerPage: 2})
	if err != nil || total != 3 || len(all) != 2 {
		t.Fatalf("paginated list: rows=%d total=%d err=%v", len(all), total, err)
	}
	if all[0].StartsAt.Before(all[1].StartsAt) {
		t.Errorf("expected starts_at DESC, got %v then %v", all[0].StartsAt, all[1].StartsAt)
	}

	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get(missing) = %v, want ErrNotFound", err)
	}
	if err := repo.UpdateStatus(ctx, uuid.New(), domain.BookingConfirmed, time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update status(missing) = %v, want ErrNotFound", err)
	}
}

func TestBookingUpdateStatusSetsTimestamps(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := New(pool)

	b := newBooking(rid, time.Now().Add(24*time.Hour))
	if err := repo.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, at); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got, _ := repo.GetByID(ctx, b.ID)
	if got.Status != domain.BookingConfirmed || got.ConfirmedAt == nil {
		t.Fatalf("confirm did not stamp: %+v", got)
	}
	if got.ArrivedAt != nil || got.CancelledAt != nil {
		t.Error("confirm stamped an unrelated column")
	}

	if err := repo.UpdateStatus(ctx, b.ID, domain.BookingArrived, at); err != nil {
		t.Fatalf("arrive: %v", err)
	}
	got, _ = repo.GetByID(ctx, b.ID)
	if got.ArrivedAt == nil {
		t.Error("arrive did not stamp arrived_at")
	}

	if err := repo.UpdateStatus(ctx, b.ID, domain.BookingCancelled, at); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ = repo.GetByID(ctx, b.ID)
	if got.CancelledAt == nil {
		t.Error("cancel did not stamp cancelled_at")
	}
}

func TestBookingClaimDueSkipLocked(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := New(pool)
	txm := sqltx.NewManager(pool)

	// pending is due relative to created_at …
	old := newBooking(rid, time.Now().Add(48*time.Hour))
	old.CreatedAt = time.Now().Add(-3 * time.Hour)
	if err := repo.Create(ctx, old); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	fresh := newBooking(rid, time.Now().Add(48*time.Hour))
	if err := repo.Create(ctx, fresh); err != nil {
		t.Fatalf("create fresh: %v", err)
	}
	// … while an arrived booking is due relative to ends_at.
	arrived := newBooking(rid, time.Now().Add(-4*time.Hour))
	if err := repo.Create(ctx, arrived); err != nil {
		t.Fatalf("create arrived: %v", err)
	}
	if err := repo.UpdateStatus(ctx, arrived.ID, domain.BookingArrived, time.Now()); err != nil {
		t.Fatalf("arrive: %v", err)
	}

	cutoff := time.Now().Add(-time.Hour)
	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingPending}, domain.ClaimByCreatedAt, cutoff, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != old.ID {
			t.Errorf("claim pending = %d rows, want only the stale one", len(due))
		}

		// A parallel worker must not see the same row.
		locked, err := repo.ClaimDue(context.Background(),
			[]domain.BookingStatus{domain.BookingPending}, domain.ClaimByCreatedAt, cutoff, 10)
		if err != nil {
			return err
		}
		if len(locked) != 0 {
			t.Errorf("second worker claimed %d locked rows, want 0", len(locked))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim tx: %v", err)
	}

	err = txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingArrived}, domain.ClaimByEndsAt, cutoff, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != arrived.ID {
			t.Errorf("claim arrived = %d rows, want 1", len(due))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim arrived tx: %v", err)
	}
}

// ClaimDue must not starve. The query cuts off on one column and therefore has
// to ORDER BY that same column: when the batch is smaller than the candidate
// set, the rows that have waited longest must be the ones returned.
//
// The earlier version cut off on created_at but ordered by starts_at, so a
// genuinely overdue request whose visit is far in the future could be pushed
// out of every batch by fresher ones starting sooner — forever.
func TestBookingClaimDueOrdersByCutoffColumn(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	repo := New(pool)
	txm := sqltx.NewManager(pool)

	// oldest waited 10h but is booked far ahead; newest was created 1h ago for
	// tonight. Ordering by starts_at would return them in exactly this reverse.
	oldest := newBooking(rid, time.Now().Add(400*time.Hour))
	oldest.CreatedAt = time.Now().Add(-10 * time.Hour)
	middle := newBooking(rid, time.Now().Add(100*time.Hour))
	middle.CreatedAt = time.Now().Add(-5 * time.Hour)
	newest := newBooking(rid, time.Now().Add(6*time.Hour))
	newest.CreatedAt = time.Now().Add(-time.Hour)
	for _, b := range []*domain.Booking{newest, middle, oldest} { // inserted out of order on purpose
		if err := repo.Create(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	cutoff := time.Now()
	wantOrder := []uuid.UUID{oldest.ID, middle.ID, newest.ID}

	// A batch of one must take the oldest, not whatever starts soonest.
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingPending},
			domain.ClaimByCreatedAt, cutoff, 1)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != oldest.ID {
			t.Errorf("batch of 1 claimed %v, want the oldest (%s) — the stale row is being starved", ids(due), oldest.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim tx: %v", err)
	}

	// The full batch comes back oldest-first.
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingPending},
			domain.ClaimByCreatedAt, cutoff, 10)
		if err != nil {
			return err
		}
		got := ids(due)
		if len(got) != 3 {
			t.Fatalf("claimed %d rows, want 3", len(got))
		}
		for i := range wantOrder {
			if got[i] != wantOrder[i] {
				t.Fatalf("claim order = %v, want oldest created_at first (%v)", got, wantOrder)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("claim tx: %v", err)
	}

	// The same rows ordered by the other clock are a different sequence, which
	// is what proves the ORDER BY really follows the caller's column.
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingPending},
			domain.ClaimByEndsAt, time.Now().Add(500*time.Hour), 10)
		if err != nil {
			return err
		}
		got := ids(due)
		want := []uuid.UUID{newest.ID, middle.ID, oldest.ID}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("claim order by ends_at = %v, want %v", got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("claim tx: %v", err)
	}
}

// An unknown claim column must be rejected, not interpolated into the SQL.
func TestBookingClaimDueRejectsUnknownColumn(t *testing.T) {
	pool, ctx := setup(t)
	repo := New(pool)
	_, err := repo.ClaimDue(ctx, []domain.BookingStatus{domain.BookingPending},
		domain.ClaimColumn("starts_at; DROP TABLE bookings"), time.Now(), 10)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("= %v, want ErrValidation", err)
	}
}

func ids(bs []domain.Booking) []uuid.UUID {
	out := make([]uuid.UUID, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

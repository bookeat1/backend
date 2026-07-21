package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	idemrepo "backend-core/internal/infrastructure/postgres/idempotency"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// integrationTables are the tables this file owns, children first. Restaurants
// and users are seeded with fresh UUIDs per test and deliberately not wiped —
// same convention as the postgres/booking package tests.
var integrationTables = []string{
	"booking_outbox", "booking_status_history", "booking_rate_log",
	"booking_blacklist", "booking_items", "booking_tables", "bookings",
	"idempotency_keys",
}

// realCreateHarness wires the create + idempotent-create usecases over the REAL
// Postgres repositories and the REAL sqltx.Manager. The in-memory fakeTx used by
// the unit tests never rolls anything back, so it cannot observe what a failed
// transaction does to writes made along the way — which is exactly the bug this
// file is about.
type realCreateHarness struct {
	pool   *pgxpool.Pool
	create CreateUseCase
	idem   IdempotentCreateUseCase

	restaurantID uuid.UUID
	userID       uuid.UUID
	actor        Actor
	startsAt     time.Time
}

func newRealCreateHarness(t *testing.T) *realCreateHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, integrationTables...)
	ctx := context.Background()

	rid, uid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,'R','Алматы','₸',true)`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM restaurants WHERE id=$1`, rid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid)
	})
	// Open every day 10:00–23:00 local, but with NO restaurant_tables rows:
	// the request passes every check and then fails on "no table available",
	// which is the realistic failure this test needs.
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
		uid, uid.String()+"@example.com", "+7777"+uid.String()[:7]); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	txm := sqltx.NewManager(pool)
	create := NewCreateUseCase(
		bookingrepo.New(pool), bookingrepo.NewTables(pool), bookingrepo.NewItems(pool),
		bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool),
		newFakeManagers(), // the actor is a plain guest
		txm, testConfig(),
	)

	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)
	return &realCreateHarness{
		pool: pool, create: create,
		idem:         NewIdempotentCreateUseCase(create, idemrepo.New(pool), txm),
		restaurantID: rid, userID: uid,
		actor:    Actor{UserID: uid, Role: domain.RoleUser},
		startsAt: time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC(),
	}
}

func (h *realCreateHarness) input() CreateInput {
	uid := h.userID
	return CreateInput{
		RestaurantID: h.restaurantID, UserID: &uid, Name: "Дамир",
		Phone: "+7 (707) 123-45-67", Email: "damir@example.com", Guests: 2,
		StartsAt: h.startsAt, Source: domain.SourceApp,
	}
}

func (h *realCreateHarness) rateLogCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM booking_rate_log WHERE restaurant_id=$1 AND action=$2`,
		h.restaurantID, string(domain.RateLogCreate)).Scan(&n); err != nil {
		t.Fatalf("count rate log: %v", err)
	}
	return n
}

// The anti-fraud attempt counter must survive a failed booking. Through the
// idempotent decorator there is already an open transaction on the context when
// Create runs, so a rate-log insert made on that context would be rolled back
// together with the failure — and a rejected attempt that un-happens is a
// counter an attacker can hammer for free. createUseCase therefore writes it on
// a detached context (domain.TxManager.Detach).
//
// Before the fix this test fails at the first assertion: the row count stays 0
// however many attempts are made. The fakeTx used by the unit tests cannot
// catch it, because it never rolls anything back.
func TestIdempotentCreateKeepsRateLogWhenCreateFails(t *testing.T) {
	h := newRealCreateHarness(t)
	ctx := context.Background()

	// The venue has no tables at all, so table selection fails with 409 after
	// the attempt has already been logged.
	_, err := h.idem.CreateIdempotent(ctx, h.actor,
		IdempotencyKey{Key: "attempt-1", RequestHash: "hash-1"}, h.input())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("CreateIdempotent = %v, want ErrAlreadyExists (no table available)", err)
	}

	if got := h.rateLogCount(t); got != 1 {
		t.Fatalf("booking_rate_log rows = %d, want 1 — the attempt was rolled back with the failed transaction", got)
	}
	// The rest of the transaction really did roll back: no booking, no
	// idempotency key (so the client may legitimately retry).
	var bookings, keys int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE restaurant_id=$1`, h.restaurantID).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if bookings != 0 {
		t.Fatalf("bookings = %d, want 0 — the failed attempt must leave nothing behind", bookings)
	}
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE user_id=$1`, h.userID).Scan(&keys); err != nil {
		t.Fatalf("count idempotency keys: %v", err)
	}
	if keys != 0 {
		t.Fatalf("idempotency_keys = %d, want 0", keys)
	}

	// Attempts accumulate across failures — that is what makes the limit bite.
	for i := 2; i <= 3; i++ {
		if _, err := h.idem.CreateIdempotent(ctx, h.actor,
			IdempotencyKey{Key: "attempt-" + string(rune('0'+i)), RequestHash: "hash"}, h.input()); err == nil {
			t.Fatal("want the create to keep failing")
		}
	}
	if got := h.rateLogCount(t); got != 3 {
		t.Fatalf("booking_rate_log rows = %d after three failed attempts, want 3", got)
	}
}

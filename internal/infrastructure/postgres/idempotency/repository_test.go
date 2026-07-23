package idempotency

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "idempotency_keys")
	return pool, context.Background()
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

func TestInsertAndGet(t *testing.T) {
	pool, ctx := setup(t)
	uid := seedUser(t, pool)
	repo := New(pool)

	rec := &domain.IdempotencyRecord{
		UserID: uid, Endpoint: "POST /bookings", Key: "key-1",
		RequestHash: "hash-1", Response: []byte(`{"id":"x"}`),
	}
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := repo.Get(ctx, uid, "POST /bookings", "key-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// jsonb normalizes whitespace on the way back out, so compare without it.
	if got.RequestHash != "hash-1" || strings.ReplaceAll(string(got.Response), " ", "") != `{"id":"x"}` {
		t.Errorf("round trip mismatch: %+v", got)
	}

	// Same key again → conflict, which is what makes the retry replay.
	if err := repo.Insert(ctx, &domain.IdempotencyRecord{
		UserID: uid, Endpoint: "POST /bookings", Key: "key-1", RequestHash: "hash-2",
	}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("duplicate insert err = %v, want ErrAlreadyExists", err)
	}

	// The same key for another user is a different key.
	other := seedUser(t, pool)
	if err := repo.Insert(ctx, &domain.IdempotencyRecord{
		UserID: other, Endpoint: "POST /bookings", Key: "key-1", RequestHash: "hash-1",
	}); err != nil {
		t.Errorf("insert for another user: %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	pool, ctx := setup(t)
	uid := seedUser(t, pool)
	if _, err := New(pool).Get(ctx, uid, "POST /bookings", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestInsertRaceSingleWinner proves the claim the idempotent-create decorator
// relies on: when two first attempts with the same key run at the same time,
// exactly one insert succeeds and the other sees ErrAlreadyExists — so the loser
// can roll its own work back and replay the winner's stored response.
func TestInsertRaceSingleWinner(t *testing.T) {
	pool, ctx := setup(t)
	uid := seedUser(t, pool)
	repo := New(pool)

	const n = 6
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			errs <- repo.Insert(ctx, &domain.IdempotencyRecord{
				UserID: uid, Endpoint: "POST /bookings", Key: "race", RequestHash: "hash",
			})
		}()
	}
	close(start)

	var ok, taken int
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrAlreadyExists):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || taken != n-1 {
		t.Fatalf("winners = %d, already-exists = %d, want 1 and %d", ok, taken, n-1)
	}
}

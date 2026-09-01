package notification

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// seedDeviceToken creates a live guest device the push_tickets FK can point at.
func seedDeviceToken(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	tok := &domain.DevicePushToken{
		UserID:   userID,
		Token:    "ExponentPushToken[" + uuid.NewString() + "]",
		Platform: domain.PlatformAndroid,
	}
	if err := NewDeviceTokens(pool).Upsert(context.Background(), tok); err != nil {
		t.Fatalf("seed device token: %v", err)
	}
	return tok.ID
}

func truncateTickets(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	testdb.Truncate(t, pool, "push_tickets", "device_push_tokens", "users")
}

// recordAt inserts a ticket with an explicit age, which the repository's own
// Record cannot do (it stamps now()) — the age is what every query keys off.
func recordAt(t *testing.T, pool *pgxpool.Pool, id string, deviceTokenID uuid.UUID, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO push_tickets (ticket_id, device_token_id, created_at) VALUES ($1,$2,$3)`,
		id, deviceTokenID, createdAt); err != nil {
		t.Fatalf("seed ticket %s: %v", id, err)
	}
}

// Record is idempotent on the ticket id: a resend that repeats an id must not
// enqueue a second poll, and must not resurrect an already resolved ticket.
func TestPushTickets_RecordIsIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	truncateTickets(t, pool)
	ctx := context.Background()
	uid := seedUser(t, pool)
	dev := seedDeviceToken(t, pool, uid)
	repo := NewPushTickets(pool)
	ev := uuid.New()

	tk := domain.PushTicket{ID: "ticket-1", DeviceTokenID: dev, OutboxEventID: &ev}
	if err := repo.Record(ctx, tk); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := repo.Record(ctx, tk); err != nil {
		t.Fatalf("second record: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM push_tickets`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1 — the ticket id is the primary key", n)
	}

	// Resolved, then recorded again: it must stay resolved.
	if err := repo.Resolve(ctx, []string{"ticket-1"}, time.Now()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := repo.Record(ctx, tk); err != nil {
		t.Fatalf("record after resolve: %v", err)
	}
	left, err := repo.ListUnresolved(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("unresolved = %v, want none — a repeat record must not re-open a closed ticket", left)
	}
}

// ListUnresolved answers exactly the worker's question: unresolved, old enough,
// oldest first, capped.
func TestPushTickets_ListUnresolvedRespectsAgeAndLimit(t *testing.T) {
	pool := testdb.Connect(t)
	truncateTickets(t, pool)
	ctx := context.Background()
	uid := seedUser(t, pool)
	dev := seedDeviceToken(t, pool, uid)
	repo := NewPushTickets(pool)
	now := time.Now()

	recordAt(t, pool, "old-1", dev, now.Add(-3*time.Hour))
	recordAt(t, pool, "old-2", dev, now.Add(-2*time.Hour))
	recordAt(t, pool, "old-3", dev, now.Add(-90*time.Minute))
	recordAt(t, pool, "young", dev, now.Add(-time.Minute))
	if err := repo.Resolve(ctx, []string{"old-2"}, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got, err := repo.ListUnresolved(ctx, now.Add(-15*time.Minute), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, tk := range got {
		ids = append(ids, tk.ID)
	}
	if len(ids) != 2 || ids[0] != "old-1" || ids[1] != "old-3" {
		t.Fatalf("ids = %v, want [old-1 old-3] (resolved and too-young excluded, oldest first)", ids)
	}
	if got[0].DeviceTokenID != dev {
		t.Fatalf("device token id = %s, want %s", got[0].DeviceTokenID, dev)
	}

	limited, err := repo.ListUnresolved(ctx, now.Add(-15*time.Minute), 1)
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "old-1" {
		t.Fatalf("limited = %v, want just the oldest", limited)
	}
}

// Resolve is idempotent and never rewrites an existing timestamp: a re-poll of
// an already closed ticket must not make it look freshly answered.
func TestPushTickets_ResolveIsIdempotent(t *testing.T) {
	pool := testdb.Connect(t)
	truncateTickets(t, pool)
	ctx := context.Background()
	uid := seedUser(t, pool)
	dev := seedDeviceToken(t, pool, uid)
	repo := NewPushTickets(pool)
	now := time.Now().UTC().Truncate(time.Millisecond)

	recordAt(t, pool, "t1", dev, now.Add(-time.Hour))
	if err := repo.Resolve(ctx, []string{"t1"}, now); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM push_tickets WHERE ticket_id='t1'`).Scan(&first); err != nil {
		t.Fatalf("read resolved_at: %v", err)
	}
	if err := repo.Resolve(ctx, []string{"t1", "does-not-exist"}, now.Add(time.Hour)); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM push_tickets WHERE ticket_id='t1'`).Scan(&second); err != nil {
		t.Fatalf("read resolved_at again: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("resolved_at moved from %v to %v on a repeat resolve", first, second)
	}
}

// PROVIDER RETENTION. Expo deletes receipts after 24 hours; a ticket older than
// that can never be answered and must be closed, or the table grows forever.
func TestPushTickets_ExpireOlderThan(t *testing.T) {
	pool := testdb.Connect(t)
	truncateTickets(t, pool)
	ctx := context.Background()
	uid := seedUser(t, pool)
	dev := seedDeviceToken(t, pool, uid)
	repo := NewPushTickets(pool)
	now := time.Now()

	recordAt(t, pool, "ancient", dev, now.Add(-25*time.Hour))
	recordAt(t, pool, "fresh", dev, now.Add(-time.Hour))

	n, err := repo.ExpireOlderThan(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
	// A second pass finds nothing: the row is already closed.
	again, err := repo.ExpireOlderThan(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("expire again: %v", err)
	}
	if again != 0 {
		t.Fatalf("expired = %d on the second pass, want 0", again)
	}
	left, err := repo.ListUnresolved(ctx, now, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].ID != "fresh" {
		t.Fatalf("unresolved = %v, want only the fresh ticket", left)
	}
}

// Deleting the guest cascades: users → device_push_tokens → push_tickets. Without
// ON DELETE CASCADE on this FK, account deletion would fail on a ticket row.
func TestPushTickets_CascadeOnAccountDeletion(t *testing.T) {
	pool := testdb.Connect(t)
	truncateTickets(t, pool)
	ctx := context.Background()
	uid := seedUser(t, pool)
	dev := seedDeviceToken(t, pool, uid)
	recordAt(t, pool, "t1", dev, time.Now())

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM push_tickets`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("push_tickets rows after account deletion = %d, want 0", n)
	}
}

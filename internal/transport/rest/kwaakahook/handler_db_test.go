package kwaakahook

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"backend-core/internal/infrastructure/postgres/kwaakaorder"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// Against the real inbox table: with an empty secret (or the flag off) an
// anonymous POST gets 404 and the database is untouched; with the secret it is stored.
func TestAnonymousPostWritesNothingToDatabase(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM kwaaka_webhook_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inbox := kwaakaorder.NewWebhooks(pool)
	before := count()

	body := `{"orderId":"anon-probe-` + t.Name() + `","status":"open"}`
	for name, h := range map[string]*Handler{
		"empty secret": NewHandler(inbox, "", true, log),
		"flag off":     NewHandler(inbox, "s3", false, log),
	} {
		for _, hdr := range []map[string]string{nil, {secretHeader: "s3"}, {secretHeader: ""}} {
			if w := do(t, h, "/webhooks/kwaaka/order-status", body, hdr); w.Code != 404 {
				t.Fatalf("%s hdr=%v: %d, want 404", name, hdr, w.Code)
			}
		}
	}
	if got := count(); got != before {
		t.Fatalf("anonymous POST wrote %d row(s)", got-before)
	}

	// sanity: the same handler with secret + flag on does store it.
	ok := NewHandler(inbox, "s3", true, log)
	if w := do(t, ok, "/webhooks/kwaaka/order-status", body, map[string]string{secretHeader: "s3"}); w.Code != 200 {
		t.Fatalf("authorised POST: %d", w.Code)
	}
	if got := count(); got != before+1 {
		t.Fatalf("authorised POST stored %d row(s), want 1", got-before)
	}
}

package notification

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// TestListActiveByUsers pins the push-campaign fan-out's batch read: many
// guests, one round trip, only ACTIVE tokens, an absent guest is simply
// missing (never a zero-value entry), and an empty input returns no rows
// without erroring.
func TestListActiveByUsers(t *testing.T) {
	pool := testdb.Connect(t)
	repo := NewDeviceTokens(pool)
	ctx := context.Background()

	withDevice := seedUser(t, pool)
	withDeadDevice := seedUser(t, pool)
	withNoDevice := seedUser(t, pool)

	live := &domain.DevicePushToken{UserID: withDevice, Token: "tok-live-" + uuid.NewString(), Platform: domain.PlatformIOS}
	if err := repo.Upsert(ctx, live); err != nil {
		t.Fatalf("upsert live token: %v", err)
	}
	dead := &domain.DevicePushToken{UserID: withDeadDevice, Token: "tok-dead-" + uuid.NewString(), Platform: domain.PlatformAndroid}
	if err := repo.Upsert(ctx, dead); err != nil {
		t.Fatalf("upsert dead token: %v", err)
	}
	if err := repo.DeactivateByID(ctx, dead.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	got, err := repo.ListActiveByUsers(ctx, []uuid.UUID{withDevice, withDeadDevice, withNoDevice})
	if err != nil {
		t.Fatalf("list active by users: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("active tokens = %d, want 1 (only the live one)", len(got))
	}
	if got[0].UserID != withDevice || got[0].Token != live.Token {
		t.Fatalf("got %+v, want the live token for %s", got[0], withDevice)
	}

	empty, err := repo.ListActiveByUsers(ctx, nil)
	if err != nil {
		t.Fatalf("list with empty input: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty input returned %d rows, want 0", len(empty))
	}
}

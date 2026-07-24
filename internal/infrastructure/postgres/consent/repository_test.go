package consent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/infrastructure/sqltx"
)

// seedUser satisfies the user_id foreign key both tables need, going through
// the user repository (same pattern as favorite/*_test.go) rather than raw SQL.
func seedUser(ctx context.Context, t *testing.T, pool sqltx.Querier) uuid.UUID {
	t.Helper()
	repo := userrepo.New(pool)
	u := &domain.User{ID: uuid.New(), FullName: "Consent User", Role: domain.RoleUser, PreferredLanguage: "ru"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u.ID
}

// TestAppendIsAppendOnlyAndCurrentStateReflectsLatest records a grant then a
// revoke of the same consent type and asserts (a) the current state reflects
// the LATEST decision (revoked), and (b) the earlier grant is still in the log
// (history is preserved, nothing is overwritten).
func TestAppendIsAppendOnlyAndCurrentStateReflectsLatest(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "user_consents", "user_notification_preferences", "users")
	ctx := context.Background()

	uid := seedUser(ctx, t, pool)
	repo := NewConsentRepository(pool)

	grant := &domain.ConsentRecord{
		UserID: uid, ConsentType: "privacy_policy", Version: "v1",
		Granted: true, Source: domain.ConsentSourceApp,
	}
	if err := repo.Append(ctx, grant); err != nil {
		t.Fatalf("append grant: %v", err)
	}
	revoke := &domain.ConsentRecord{
		UserID: uid, ConsentType: "privacy_policy", Version: "v1",
		Granted: false, Source: domain.ConsentSourceWeb,
	}
	if err := repo.Append(ctx, revoke); err != nil {
		t.Fatalf("append revoke: %v", err)
	}

	state, err := repo.CurrentState(ctx, uid)
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if len(state) != 1 {
		t.Fatalf("current state should collapse to 1 row per type, got %d", len(state))
	}
	if state[0].Granted {
		t.Errorf("current state should reflect the latest decision (revoked), got granted=true")
	}
	if state[0].Source != domain.ConsentSourceWeb {
		t.Errorf("current state source = %q, want the latest (web)", state[0].Source)
	}

	// History preserved: both rows still exist in the append-only log.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_consents WHERE user_id=$1 AND consent_type='privacy_policy'`, uid).
		Scan(&count); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if count != 2 {
		t.Errorf("append-only log should keep both rows, got %d", count)
	}
}

// TestCurrentStateIsPerUserIsolated proves one user's CurrentState never leaks
// another user's consent records.
func TestCurrentStateIsPerUserIsolated(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "user_consents", "user_notification_preferences", "users")
	ctx := context.Background()

	alice := seedUser(ctx, t, pool)
	bob := seedUser(ctx, t, pool)
	repo := NewConsentRepository(pool)

	if err := repo.Append(ctx, &domain.ConsentRecord{
		UserID: alice, ConsentType: "marketing", Version: "v1", Granted: true, Source: domain.ConsentSourceApp,
	}); err != nil {
		t.Fatalf("append alice: %v", err)
	}
	if err := repo.Append(ctx, &domain.ConsentRecord{
		UserID: bob, ConsentType: "analytics", Version: "v1", Granted: true, Source: domain.ConsentSourceApp,
	}); err != nil {
		t.Fatalf("append bob: %v", err)
	}

	state, err := repo.CurrentState(ctx, alice)
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if len(state) != 1 || state[0].ConsentType != "marketing" {
		t.Fatalf("alice must see only her own consent, got %+v", state)
	}
}

// TestPreferenceDefaultsThenPersists asserts a missing preference row reads as
// the all-enabled default, and an opt-out persists and is read back.
func TestPreferenceDefaultsThenPersists(t *testing.T) {
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "user_consents", "user_notification_preferences", "users")
	ctx := context.Background()

	uid := seedUser(ctx, t, pool)
	repo := NewPreferenceRepository(pool)

	def, err := repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if !def.NotificationsEnabled || !def.PushEnabled || !def.EmailEnabled {
		t.Fatalf("unset preference should default to all-enabled, got %+v", def)
	}

	// Opt out entirely.
	if err := repo.Upsert(ctx, domain.NotificationPreference{
		UserID: uid, NotificationsEnabled: false, PushEnabled: false, EmailEnabled: false,
	}); err != nil {
		t.Fatalf("upsert opt-out: %v", err)
	}
	got, err := repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("get after opt-out: %v", err)
	}
	if got.NotificationsEnabled {
		t.Errorf("opt-out did not persist: notifications_enabled still true")
	}
	if got.Allows(domain.ChannelWebPush) {
		t.Errorf("opted-out user should not allow web push")
	}

	// Re-enable master but keep push off: upsert must replace, not accumulate.
	if err := repo.Upsert(ctx, domain.NotificationPreference{
		UserID: uid, NotificationsEnabled: true, PushEnabled: false, EmailEnabled: true,
	}); err != nil {
		t.Fatalf("upsert re-enable: %v", err)
	}
	got, err = repo.Get(ctx, uid)
	if err != nil {
		t.Fatalf("get after re-enable: %v", err)
	}
	if !got.NotificationsEnabled || got.PushEnabled || !got.EmailEnabled {
		t.Fatalf("upsert should replace the row wholesale, got %+v", got)
	}
	if got.Allows(domain.ChannelWebPush) {
		t.Errorf("push off should mute web push even with master on")
	}
}

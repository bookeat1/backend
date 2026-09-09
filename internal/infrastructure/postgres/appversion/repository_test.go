package appversion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// reset puts the two seeded rows back the way migration 0103 leaves them:
// store URL and wording present, BOTH thresholds empty. Every test starts from
// the shipped state, so a test that forgets to clear a threshold cannot leak a
// forced update into the next one.
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE mobile_app_policies
		    SET min_supported_version = '', min_recommended_version = '',
		        store_url = CASE platform
		            WHEN 'ios' THEN 'https://apps.apple.com/app/id6757542577'
		            ELSE 'https://play.google.com/store/apps/details?id=kz.bookeat.app' END,
		        required_title = 'Нужно обновить приложение',
		        required_message = 'Эта версия больше не поддерживается',
		        required_title_i18n = '{"ru":"Нужно обновить приложение","en":"Update required"}'::jsonb,
		        required_message_i18n = NULL,
		        recommended_title = 'Доступно обновление',
		        recommended_message = 'Обновите приложение',
		        recommended_title_i18n = NULL,
		        recommended_message_i18n = NULL`); err != nil {
		t.Fatalf("reset policies: %v", err)
	}
}

// TestMigrationSeedsBothPlatformsAndForcesNobody is the assertion that matters
// on deploy day: the migration lands two rows with EMPTY thresholds, so the new
// endpoint answers "none" to every build already in the stores.
func TestMigrationSeedsBothPlatformsAndForcesNobody(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	repo := New(pool)

	items, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d rows, want ios and android", len(items))
	}
	if items[0].Platform != domain.PlatformAndroid || items[1].Platform != domain.PlatformIOS {
		t.Errorf("List is not ordered by platform: %q, %q", items[0].Platform, items[1].Platform)
	}
	for _, p := range items {
		if p.MinSupportedVersion != "" || p.MinRecommendedVersion != "" {
			t.Errorf("%s ships with a threshold set (%q / %q) — the migration must force nobody",
				p.Platform, p.MinSupportedVersion, p.MinRecommendedVersion)
		}
		if p.StoreURL == "" {
			t.Errorf("%s has no store URL", p.Platform)
		}
		if p.RequiredTitle == "" || p.RecommendedTitle == "" {
			t.Errorf("%s ships with no wording, so a mode could not be switched on without writing copy first", p.Platform)
		}
		for _, v := range []string{"0.1", "1.0", "1.5", "99.99"} {
			if got := p.Decide(v); got != domain.AppUpdateNone {
				t.Errorf("the shipped %s policy tells %q to %q", p.Platform, v, got)
			}
		}
	}
}

// TestGetUnknownPlatformIsNotFound — the sentinel the usecase turns into
// "action=none", never a 500.
func TestGetUnknownPlatformIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	if _, err := New(pool).Get(context.Background(), domain.DevicePlatform("web")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get(web) = %v, want ErrNotFound", err)
	}
}

// TestUpsertRoundTrip writes every field and reads it back through a SECOND
// call, so a column mapped to the wrong placeholder cannot hide behind the
// in-memory struct.
func TestUpsertRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })
	repo := New(pool)
	ctx := context.Background()

	want := domain.MobileAppPolicy{
		Platform:               domain.PlatformIOS,
		MinSupportedVersion:    "1.5",
		MinRecommendedVersion:  "1.7.2",
		StoreURL:               "https://apps.apple.com/app/id6757542577",
		RecommendedTitle:       "Доступно обновление",
		RecommendedTitleI18n:   domain.I18n{"ru": "Доступно обновление", "kk": "Жаңарту қолжетімді"},
		RecommendedMessage:     "Обновите приложение",
		RecommendedMessageI18n: domain.I18n{"en": "Please update"},
		RequiredTitle:          "Нужно обновить",
		RequiredTitleI18n:      domain.I18n{"en": "Update required"},
		RequiredMessage:        "Эта версия больше не поддерживается",
		RequiredMessageI18n:    domain.I18n{"kk": "Бұл нұсқа қолдау көрсетілмейді"},
	}
	saved := want
	if err := repo.Upsert(ctx, &saved); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if saved.UpdatedAt.IsZero() {
		t.Error("Upsert did not return updated_at")
	}

	got, err := repo.Get(ctx, domain.PlatformIOS)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MinSupportedVersion != want.MinSupportedVersion ||
		got.MinRecommendedVersion != want.MinRecommendedVersion ||
		got.StoreURL != want.StoreURL ||
		got.RecommendedTitle != want.RecommendedTitle ||
		got.RecommendedMessage != want.RecommendedMessage ||
		got.RequiredTitle != want.RequiredTitle ||
		got.RequiredMessage != want.RequiredMessage {
		t.Fatalf("scalars did not round-trip:\n got %+v\nwant %+v", *got, want)
	}
	for name, pair := range map[string][2]domain.I18n{
		"recommended_title_i18n":   {got.RecommendedTitleI18n, want.RecommendedTitleI18n},
		"recommended_message_i18n": {got.RecommendedMessageI18n, want.RecommendedMessageI18n},
		"required_title_i18n":      {got.RequiredTitleI18n, want.RequiredTitleI18n},
		"required_message_i18n":    {got.RequiredMessageI18n, want.RequiredMessageI18n},
	} {
		if len(pair[0]) != len(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
			continue
		}
		for k, v := range pair[1] {
			if pair[0][k] != v {
				t.Errorf("%s[%s] = %q, want %q", name, k, pair[0][k], v)
			}
		}
	}

	// The verdict the app will actually get, read back out of Postgres.
	if a := got.Decide("1.4.9"); a != domain.AppUpdateRequired {
		t.Errorf("stored policy: Decide(1.4.9) = %q, want required", a)
	}
	if a := got.Decide("1.7.1"); a != domain.AppUpdateRecommended {
		t.Errorf("stored policy: Decide(1.7.1) = %q, want recommended", a)
	}
	if a := got.Decide("1.7.2"); a != domain.AppUpdateNone {
		t.Errorf("stored policy: Decide(1.7.2) = %q, want none", a)
	}
}

// TestUpsertUpdatesInPlace — a second write must not create a second row for the
// same platform, and must not need a pre-existing one either.
func TestUpsertUpdatesInPlace(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })
	repo := New(pool)
	ctx := context.Background()

	p := domain.MobileAppPolicy{Platform: domain.PlatformAndroid, MinSupportedVersion: "1.1",
		StoreURL: "https://play.google.com/store/apps/details?id=kz.bookeat.app"}
	if err := repo.Upsert(ctx, &p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	p.MinSupportedVersion = "1.2"
	if err := repo.Upsert(ctx, &p); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mobile_app_policies WHERE platform = 'android'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("android rows = %d, want 1", n)
	}
	got, err := repo.Get(ctx, domain.PlatformAndroid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.MinSupportedVersion != "1.2" {
		t.Errorf("min_supported_version = %q, want 1.2", got.MinSupportedVersion)
	}
}

// TestPlatformCheckConstraintHolds. The closed row set is a database rule, not
// only a Go one: a code path that ever tried to store "web" must fail loudly
// rather than create a policy nothing can act on.
func TestPlatformCheckConstraintHolds(t *testing.T) {
	pool := testdb.Connect(t)
	p := domain.MobileAppPolicy{Platform: domain.DevicePlatform("web")}
	if err := New(pool).Upsert(context.Background(), &p); err == nil {
		t.Fatal("the database accepted platform=web")
	} else if !strings.Contains(err.Error(), "mobile_app_policies_platform_check") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

// TestNullTranslationsReadAsNilNotEmptyMap: a NULL jsonb column must not turn
// into an object, or every payload would carry empty translation maps.
func TestNullTranslationsReadAsNilNotEmptyMap(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	got, err := New(pool).Get(context.Background(), domain.PlatformAndroid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RecommendedTitleI18n != nil {
		t.Errorf("a NULL jsonb column read as %v, want nil", got.RecommendedTitleI18n)
	}
}

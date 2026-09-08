package platformpages

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

// reset puts every seeded row back the way migration 0105 leaves it: title
// from the seed, empty body, no translations, unpublished, no updated_by.
// Every test starts from the shipped state so a page one test publishes
// cannot leak into the next.
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE platform_pages SET
			title = CASE slug
				WHEN 'about' THEN 'О BookEat'
				WHEN 'jobs' THEN 'Вакансии'
				WHEN 'contacts' THEN 'Контакты'
				WHEN 'how-it-works' THEN 'Как это работает'
				WHEN 'cancellation' THEN 'Отмена брони'
				WHEN 'offer' THEN 'Оферта'
				WHEN 'privacy' THEN 'Политика данных'
			END,
			title_i18n = NULL,
			body = '',
			body_i18n = NULL,
			published_at = NULL,
			updated_by = NULL`); err != nil {
		t.Fatalf("reset platform_pages: %v", err)
	}
}

// TestMigrationSeedsAllSevenSlugsUnpublished is the assertion that matters on
// deploy day: the migration lands exactly the seven allowed slugs, all
// unpublished — a guest hitting any of the seven footer links before Дамир
// fills them in must get "not found", never an empty page.
func TestMigrationSeedsAllSevenSlugsUnpublished(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	repo := New(pool)

	items, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != len(domain.PlatformPageSlugs) {
		t.Fatalf("got %d rows, want %d", len(items), len(domain.PlatformPageSlugs))
	}
	seen := map[domain.PlatformPageSlug]bool{}
	for _, p := range items {
		seen[p.Slug] = true
		if p.Published() {
			t.Errorf("%s ships published — the migration must force nothing live", p.Slug)
		}
		if p.Title == "" {
			t.Errorf("%s has no title", p.Slug)
		}
		if p.Format != domain.PlatformPageFormatMarkdown {
			t.Errorf("%s format = %q, want markdown", p.Slug, p.Format)
		}
	}
	for _, s := range domain.PlatformPageSlugs {
		if !seen[s] {
			t.Errorf("seeded rows are missing %s", s)
		}
	}
}

// TestGetUnknownSlugIsNotFound — the sentinel the usecase turns into "not
// found" for both an unknown slug and (one layer up) an unpublished one.
func TestGetUnknownSlugIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	_, err := New(pool).Get(context.Background(), domain.PlatformPageSlug("does-not-exist"))
	if err == nil {
		t.Fatal("want an error for an unknown slug")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

// TestUpdateRoundTrip writes every field and reads it back through a SECOND
// call, so a column bound to the wrong placeholder cannot hide behind the
// in-memory struct.
func TestUpdateRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })
	repo := New(pool)
	ctx := context.Background()

	p, err := repo.Get(ctx, domain.PlatformPageOffer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// No FK on updated_by (see migration 0105's comment) — any uuid round-trips.
	actor := uuid.New()
	p.Title = "Публичная оферта"
	p.TitleI18n = domain.I18n{"ru": "Публичная оферта", "en": "Public offer"}
	p.Body = "# Оферта\n\nУсловия оказания услуг."
	p.BodyI18n = domain.I18n{"kk": "# Оферта (kk)"}
	now := p.CreatedAt // any non-nil instant works; only nil-ness is asserted below
	p.PublishedAt = &now
	p.UpdatedBy = &actor

	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p.UpdatedAt.IsZero() {
		t.Error("Update did not return updated_at")
	}

	got, err := repo.Get(ctx, domain.PlatformPageOffer)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if got.Title != "Публичная оферта" || got.Body != "# Оферта\n\nУсловия оказания услуг." {
		t.Fatalf("scalars did not round-trip: %+v", *got)
	}
	if !got.Published() {
		t.Error("published_at did not round-trip")
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != actor {
		t.Errorf("updated_by = %v, want %s", got.UpdatedBy, actor)
	}
	if got.TitleI18n["en"] != "Public offer" {
		t.Errorf("title_i18n did not round-trip: %+v", got.TitleI18n)
	}
	if got.BodyI18n["kk"] != "# Оферта (kk)" {
		t.Errorf("body_i18n did not round-trip: %+v", got.BodyI18n)
	}
}

// TestUpdateUnpublishClearsPublishedAt — writing a nil PublishedAt must
// actually clear the column, not leave the old timestamp behind (a pointer
// bound wrong could silently no-op the "unset" case while the "set" case
// still passes).
func TestUpdateUnpublishClearsPublishedAt(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })
	repo := New(pool)
	ctx := context.Background()

	p, err := repo.Get(ctx, domain.PlatformPageOffer)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	published := p.CreatedAt
	p.Body = "# x"
	p.PublishedAt = &published
	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("publish: %v", err)
	}

	p.PublishedAt = nil
	if err := repo.Update(ctx, p); err != nil {
		t.Fatalf("unpublish: %v", err)
	}

	got, err := repo.Get(ctx, domain.PlatformPageOffer)
	if err != nil {
		t.Fatalf("Get after unpublish: %v", err)
	}
	if got.Published() {
		t.Error("published_at was not cleared")
	}
}

// TestUpdateUnknownSlugIsNotFound — the row set is closed; a slug that never
// existed must not silently insert one.
func TestUpdateUnknownSlugIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	p := &domain.PlatformPage{Slug: domain.PlatformPageSlug("does-not-exist"), Title: "x"}
	err := New(pool).Update(context.Background(), p)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update(unknown slug) = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_pages WHERE slug = 'does-not-exist'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatal("Update on an unknown slug inserted a row")
	}
}

// TestSlugCheckConstraintHolds. The closed set of seven pages is a database
// rule, not only a Go one: a write that ever tried an eighth slug must fail
// loudly rather than create a page nothing routes to.
func TestSlugCheckConstraintHolds(t *testing.T) {
	pool := testdb.Connect(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO platform_pages (slug, title) VALUES ('eighth-page', 'x')`)
	if err == nil {
		t.Fatal("the database accepted an eighth slug")
	}
	if !strings.Contains(err.Error(), "platform_pages_slug_check") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

// TestNullTranslationsReadAsNilNotEmptyMap: a NULL jsonb column must not turn
// into an object, or every public payload would carry empty translation maps.
func TestNullTranslationsReadAsNilNotEmptyMap(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	t.Cleanup(func() { reset(t, pool) })

	got, err := New(pool).Get(context.Background(), domain.PlatformPageAbout)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TitleI18n != nil {
		t.Errorf("a NULL jsonb column read as %v, want nil", got.TitleI18n)
	}
	if got.BodyI18n != nil {
		t.Errorf("a NULL jsonb column read as %v, want nil", got.BodyI18n)
	}
}

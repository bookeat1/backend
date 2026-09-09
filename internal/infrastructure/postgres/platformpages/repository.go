// Package platformpages is the Postgres implementation of
// domain.PlatformPageRepository: the seven footer text pages (migration
// 0105). The row set is fixed by the migration's seed, so unlike most
// repositories in this codebase there is no Create/Delete, only Get/List and
// a full-row Update.
package platformpages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository reads and writes platform_pages.
type Repository struct{ pool sqltx.Querier }

// New builds the repository over a pool or an active transaction.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PlatformPageRepository = (*Repository)(nil)

const cols = `slug, title, title_i18n, body, body_i18n, format, published_at,
	updated_by, created_at, updated_at`

// Get reads one page by slug. A missing row is domain.ErrNotFound — for a
// slug in domain.PlatformPageSlugs this means the migration's seed is
// missing (a database that was restored without it, or a row deleted by
// hand), not a normal outcome the caller should expect.
func (r *Repository) Get(ctx context.Context, slug domain.PlatformPageSlug) (*domain.PlatformPage, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM platform_pages WHERE slug = $1`, string(slug))
	p, err := scanPage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return p, err
}

// List returns every page ordered by slug (a stable, if not admin-screen
// specific, order — the transport layer re-orders into
// domain.PlatformPageSlugs' order for the cabinet).
func (r *Repository) List(ctx context.Context) ([]domain.PlatformPage, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM platform_pages ORDER BY slug ASC`)
	if err != nil {
		return nil, fmt.Errorf("list platform pages: %w", err)
	}
	defer rows.Close()

	out := make([]domain.PlatformPage, 0, len(domain.PlatformPageSlugs))
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list platform pages: %w", err)
	}
	return out, nil
}

// Update writes the whole row. UPDATE rather than UPSERT: the row set is
// closed (seeded by the migration, never created here), so a slug this reaches
// with zero rows affected means the slug does not exist in this database —
// domain.ErrNotFound, not a silent insert.
func (r *Repository) Update(ctx context.Context, p *domain.PlatformPage) error {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE platform_pages SET
			title = $2,
			title_i18n = $3,
			body = $4,
			body_i18n = $5,
			published_at = $6,
			updated_by = $7,
			updated_at = now()
		 WHERE slug = $1
		 RETURNING updated_at`,
		string(p.Slug), p.Title, i18nToDB(p.TitleI18n), p.Body, i18nToDB(p.BodyI18n),
		p.PublishedAt, p.UpdatedBy)
	if err := row.Scan(&p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("update platform page: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanPage(row scanner) (*domain.PlatformPage, error) {
	var (
		p         domain.PlatformPage
		slug      string
		titleI18n []byte
		bodyI18n  []byte
	)
	if err := row.Scan(&slug, &p.Title, &titleI18n, &p.Body, &bodyI18n, &p.Format,
		&p.PublishedAt, &p.UpdatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan platform page: %w", err)
	}
	p.Slug = domain.PlatformPageSlug(slug)
	p.TitleI18n = i18nFromDB(titleI18n)
	p.BodyI18n = i18nFromDB(bodyI18n)
	return &p, nil
}

func i18nToDB(m domain.I18n) any {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
}

func i18nFromDB(b []byte) domain.I18n {
	if len(b) == 0 {
		return nil
	}
	var m domain.I18n
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

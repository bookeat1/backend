// Package promo is the Postgres implementation of domain.PromoRepository.
package promo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.PromoRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the promo repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PromoRepository = (*Repository)(nil)

const selectCols = `id, restaurant_id, title, title_i18n, description, description_i18n,
	starts_at, ends_at, terms, status, created_at, updated_at`

// Create inserts a new promo. An unknown restaurant_id (FK violation) maps to
// ErrNotFound.
func (r *Repository) Create(ctx context.Context, p *domain.Promo) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO promos (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, terms, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING created_at, updated_at`,
		p.ID, p.RestaurantID, p.Title, i18nToDB(p.TitleI18n), p.Description, i18nToDB(p.DescriptionI18n),
		p.StartsAt, p.EndsAt, p.Terms, p.Status).
		Scan(&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create promo: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create promo: %w", err)
	}
	return nil
}

// GetByID returns a promo by its id regardless of status.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Promo, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selectCols+` FROM promos WHERE id = $1`, id)
	return scanPromo(row, "get promo")
}

// Update overwrites the mutable fields of an existing promo. A zero-rows UPDATE
// means the id is absent.
func (r *Repository) Update(ctx context.Context, p *domain.Promo) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE promos SET title = $2, title_i18n = $3, description = $4, description_i18n = $5,
			starts_at = $6, ends_at = $7, terms = $8, status = $9, updated_at = now()
		 WHERE id = $1`,
		p.ID, p.Title, i18nToDB(p.TitleI18n), p.Description, i18nToDB(p.DescriptionI18n),
		p.StartsAt, p.EndsAt, p.Terms, p.Status)
	if err != nil {
		return fmt.Errorf("update promo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update promo: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes a promo. A zero-rows DELETE means the id is absent.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM promos WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete promo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete promo: %w", domain.ErrNotFound)
	}
	return nil
}

// ListByRestaurant returns a restaurant's promos for the admin cabinet,
// optionally status-filtered, newest start first with id as a stable tie-breaker.
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)
	statusArg := statusStrings(statuses)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM promos
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))`,
		restaurantID, statusArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM promos
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))
		 ORDER BY starts_at DESC, id DESC
		 LIMIT $3 OFFSET $4`,
		restaurantID, statusArg, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list promos: %w", err)
	}
	return collect(rows, total)
}

// ListActive returns a restaurant's published promos whose validity window
// contains now (starts_at <= now AND ends_at > now), soonest-to-expire first
// with id as a stable tie-breaker. Matches idx_promos_active.
func (r *Repository) ListActive(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]domain.Promo, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM promos
		 WHERE restaurant_id = $1 AND status = 'published' AND starts_at <= $2 AND ends_at > $2`,
		restaurantID, now).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count active promos: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM promos
		 WHERE restaurant_id = $1 AND status = 'published' AND starts_at <= $2 AND ends_at > $2
		 ORDER BY ends_at ASC, id ASC
		 LIMIT $3 OFFSET $4`,
		restaurantID, now, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list active promos: %w", err)
	}
	return collect(rows, total)
}

func collect(rows pgx.Rows, total int) ([]domain.Promo, int, error) {
	defer rows.Close()
	var items []domain.Promo
	for rows.Next() {
		p, err := scanPromoRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan promo: %w", err)
		}
		items = append(items, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate promos: %w", err)
	}
	return items, total, nil
}

func scanPromo(row pgx.Row, op string) (*domain.Promo, error) {
	p, err := scanPromoRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return p, nil
}

func scanPromoRow(row pgx.Row) (*domain.Promo, error) {
	var p domain.Promo
	var titleI18n, descI18n []byte
	if err := row.Scan(&p.ID, &p.RestaurantID, &p.Title, &titleI18n, &p.Description, &descI18n,
		&p.StartsAt, &p.EndsAt, &p.Terms, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.TitleI18n = i18nFromDB(titleI18n)
	p.DescriptionI18n = i18nFromDB(descI18n)
	return &p, nil
}

func statusStrings(statuses []domain.PromoStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, string(s))
	}
	return out
}

func normalizePage(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	return page, perPage
}

func i18nToDB(m domain.I18n) any {
	if m == nil {
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

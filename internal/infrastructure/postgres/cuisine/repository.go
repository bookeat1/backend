// Package cuisine is the Postgres implementation of domain.CuisineRepository:
// the platform-wide cuisine dictionary (migration 0079) and the many-to-many
// link between a restaurant and its cuisines.
package cuisine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.CuisineRepository = (*Repository)(nil)

const cols = `id, code, name, name_i18n, image_url, display_order, is_active, created_at, updated_at`

// listOrder is the dictionary's canonical ordering, shared by every read so
// the app, the panel and the venue picker never see the same list in two
// different orders. `name` is the tie-break, `id` makes it total.
const listOrder = `ORDER BY display_order ASC, name ASC, id ASC`

func (r *Repository) List(ctx context.Context, f domain.CuisineFilter) ([]domain.Cuisine, error) {
	q := `SELECT ` + cols + ` FROM cuisines`
	if !f.IncludeInactive {
		q += ` WHERE is_active = true`
	}
	q += ` ` + listOrder

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list cuisines: %w", err)
	}
	defer rows.Close()

	out := make([]domain.Cuisine, 0)
	for rows.Next() {
		c, err := scanCuisine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cuisines: %w", err)
	}
	return out, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Cuisine, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM cuisines WHERE id = $1`, id)
	c, err := scanCuisine(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return c, err
}

func (r *Repository) Create(ctx context.Context, c *domain.Cuisine) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO cuisines (`+cols+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,now(),now())`,
		c.ID, c.Code, c.Name, i18nToDB(c.NameI18n), c.ImageURL, c.DisplayOrder, c.IsActive)
	if err != nil {
		return mapWrite(err, "create cuisine")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, c *domain.Cuisine) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE cuisines SET code=$2, name=$3, name_i18n=$4, image_url=$5,
		        display_order=$6, is_active=$7, updated_at=now()
		 WHERE id=$1`,
		c.ID, c.Code, c.Name, i18nToDB(c.NameI18n), c.ImageURL, c.DisplayOrder, c.IsActive)
	if err != nil {
		return mapWrite(err, "update cuisine")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ListByRestaurants loads the cuisine sets of a whole page in ONE query. The
// ordering inside a venue is the link position first (the venue decides which
// cuisine is its main one), then the dictionary order as a stable tie-break.
func (r *Repository) ListByRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.Cuisine, error) {
	out := make(map[uuid.UUID][]domain.Cuisine, len(restaurantIDs))
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT rc.restaurant_id, `+prefixed(cols, "c")+`
		 FROM restaurant_cuisines rc
		 JOIN cuisines c ON c.id = rc.cuisine_id
		 WHERE rc.restaurant_id = ANY($1)
		 ORDER BY rc.restaurant_id, rc.position ASC, c.display_order ASC, c.name ASC`,
		restaurantIDs)
	if err != nil {
		return nil, fmt.Errorf("list restaurant cuisines: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid uuid.UUID
		var c domain.Cuisine
		var nameI18n []byte
		if err := rows.Scan(&rid, &c.ID, &c.Code, &c.Name, &nameI18n, &c.ImageURL,
			&c.DisplayOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list restaurant cuisines: %w", err)
		}
		c.NameI18n = i18nFromDB(nameI18n)
		out[rid] = append(out[rid], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restaurant cuisines: %w", err)
	}
	return out, nil
}

// ResolveIDs returns the requested cuisines IN THE ORDER GIVEN (the caller's
// order is the venue's chosen order, and position 0 is its main cuisine), and
// fails when any id is unknown.
func (r *Repository) ResolveIDs(ctx context.Context, ids []uuid.UUID) ([]domain.Cuisine, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM cuisines WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve cuisines: %w", err)
	}
	defer rows.Close()

	byID := make(map[uuid.UUID]domain.Cuisine, len(ids))
	for rows.Next() {
		c, err := scanCuisine(rows)
		if err != nil {
			return nil, err
		}
		byID[c.ID] = *c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve cuisines: %w", err)
	}

	out := make([]domain.Cuisine, 0, len(ids))
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: unknown cuisine %s", domain.ErrValidation, id)
		}
		out = append(out, c)
	}
	return out, nil
}

// SetForRestaurant replaces the venue's whole set. Delete + inserts are
// separate statements: run inside a transaction (see the interface doc).
func (r *Repository) SetForRestaurant(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM restaurant_cuisines WHERE restaurant_id = $1`, restaurantID); err != nil {
		return fmt.Errorf("set restaurant cuisines: %w", err)
	}
	for i, id := range ids {
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_cuisines (restaurant_id, cuisine_id, position, created_at)
			 VALUES ($1,$2,$3,now())
			 ON CONFLICT (restaurant_id, cuisine_id) DO UPDATE SET position = EXCLUDED.position`,
			restaurantID, id, i); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("%w: unknown restaurant or cuisine", domain.ErrValidation)
			}
			return fmt.Errorf("set restaurant cuisines: %w", err)
		}
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanCuisine(row scanner) (*domain.Cuisine, error) {
	var c domain.Cuisine
	var nameI18n []byte
	if err := row.Scan(&c.ID, &c.Code, &c.Name, &nameI18n, &c.ImageURL,
		&c.DisplayOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan cuisine: %w", err)
	}
	c.NameI18n = i18nFromDB(nameI18n)
	return &c, nil
}

// prefixed qualifies a comma-separated column list with a table alias, so the
// dictionary's column list is written once and reused in a join.
func prefixed(list, alias string) string {
	parts := strings.Split(list, ",")
	for i, col := range parts {
		parts[i] = alias + "." + strings.TrimSpace(col)
	}
	return strings.Join(parts, ", ")
}

// mapWrite turns a unique-index violation into domain.ErrAlreadyExists. The
// two unique indexes (code, normalized name) are the ONLY duplicate guard:
// checking first and inserting after loses the race between two admins.
func mapWrite(err error, ctxMsg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: cuisine code or name already used", domain.ErrAlreadyExists)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
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

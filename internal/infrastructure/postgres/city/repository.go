// Package city is the Postgres implementation of domain.CityRepository: the
// platform-wide city dictionary and its spelling aliases (migration 0081).
package city

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

const uniqueViolation = "23505"

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.CityRepository = (*Repository)(nil)

const cols = `id, code, name, name_i18n, display_order, is_active, created_at, updated_at`

// listOrder is the dictionary's canonical ordering, shared by every read so the
// app, the panel and the admin screen never see the same list in two different
// orders. `name` is the tie-break, `id` makes it total.
const listOrder = `ORDER BY display_order ASC, name ASC, id ASC`

func (r *Repository) List(ctx context.Context, f domain.CityFilter) ([]domain.CityEntry, error) {
	q := `SELECT ` + cols + ` FROM cities`
	if !f.IncludeInactive {
		q += ` WHERE is_active = true`
	}
	q += ` ` + listOrder

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list cities: %w", err)
	}
	defer rows.Close()

	out := make([]domain.CityEntry, 0)
	for rows.Next() {
		c, err := scanCity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cities: %w", err)
	}
	return out, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.CityEntry, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+` FROM cities WHERE id = $1`, id)
	c, err := scanCity(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return c, err
}

// Create inserts the entry and immediately registers its name and code as
// aliases, because every read path (the catalog filter, the legacy-insert
// trigger) resolves through city_aliases and NOT through cities.name. A city
// created without its own alias would be invisible to the very filter it was
// created for. Both statements are here rather than in the usecase so the
// invariant cannot be forgotten by a second caller.
func (r *Repository) Create(ctx context.Context, c *domain.CityEntry) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO cities (`+cols+`) VALUES ($1,$2,$3,$4,$5,$6,now(),now())`,
		c.ID, c.Code, c.Name, i18nToDB(c.NameI18n), c.DisplayOrder, c.IsActive); err != nil {
		return mapWrite(err, "create city")
	}
	if err := r.AddAlias(ctx, c.ID, c.Name); err != nil {
		return err
	}
	return r.AddAlias(ctx, c.ID, c.Code)
}

// Update writes the mutable fields and keeps the alias table complete.
//
// The OLD name is deliberately left in city_aliases: a build in the store may
// keep sending the previous spelling as ?city= for as long as it lives, and a
// rename that silently emptied its results would be indistinguishable from an
// outage. The NEW name is added for the same reason the create path adds one.
func (r *Repository) Update(ctx context.Context, c *domain.CityEntry) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE cities SET code=$2, name=$3, name_i18n=$4, display_order=$5,
		        is_active=$6, updated_at=now()
		 WHERE id=$1`,
		c.ID, c.Code, c.Name, i18nToDB(c.NameI18n), c.DisplayOrder, c.IsActive)
	if err != nil {
		return mapWrite(err, "update city")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if err := r.AddAlias(ctx, c.ID, c.Name); err != nil {
		return err
	}
	return r.AddAlias(ctx, c.ID, c.Code)
}

// Reorder rewrites display_order in one set-based UPDATE via
// `unnest($1) WITH ORDINALITY`: atomic without an explicit transaction, and an
// id that is not in the dictionary is simply not matched by the join rather
// than failing the whole batch.
//
// The step is 10, not 1, so a later "insert this city between those two" needs
// no full renumbering.
func (r *Repository) Reorder(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE cities c
		    SET display_order = ord.pos * 10, updated_at = now()
		   FROM (SELECT id, ordinality AS pos
		           FROM unnest($1::uuid[]) WITH ORDINALITY AS t(id, ordinality)) ord
		  WHERE c.id = ord.id`, ids); err != nil {
		return fmt.Errorf("reorder cities: %w", err)
	}
	return nil
}

// ResolveAlias answers "which city is this written spelling", using the same
// normalization the SQL side uses (city_key). It is the single entry point for
// turning a ?city= value — a Russian name from an old build, a code from a new
// one, or a historical spelling — into a dictionary entry.
func (r *Repository) ResolveAlias(ctx context.Context, raw string) (*domain.CityEntry, error) {
	key := domain.NormalizeCityKey(raw)
	if key == "" {
		return nil, domain.ErrNotFound
	}
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+prefixed(cols, "c")+`
		   FROM city_aliases a JOIN cities c ON c.id = a.city_id
		  WHERE a.alias = $1`, key)
	c, err := scanCity(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return c, err
}

// AddAlias registers a spelling. Idempotent for the same city; a spelling
// already taken by ANOTHER city is a conflict, never a silent re-point — that
// would move every venue matching it to a different city's results.
func (r *Repository) AddAlias(ctx context.Context, cityID uuid.UUID, alias string) error {
	key := domain.NormalizeCityKey(alias)
	if key == "" {
		return fmt.Errorf("%w: empty city alias", domain.ErrValidation)
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO city_aliases (alias, city_id, created_at)
		 VALUES ($1,$2,now())
		 ON CONFLICT (alias) DO NOTHING`, key, cityID)
	if err != nil {
		return fmt.Errorf("add city alias: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var owner uuid.UUID
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT city_id FROM city_aliases WHERE alias = $1`, key).Scan(&owner); err != nil {
		return fmt.Errorf("add city alias: %w", err)
	}
	if owner != cityID {
		return fmt.Errorf("%w: spelling %q already belongs to another city", domain.ErrAlreadyExists, key)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanCity(row scanner) (*domain.CityEntry, error) {
	var c domain.CityEntry
	var nameI18n []byte
	if err := row.Scan(&c.ID, &c.Code, &c.Name, &nameI18n,
		&c.DisplayOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan city: %w", err)
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
		return fmt.Errorf("%w: city code or name already used", domain.ErrAlreadyExists)
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

// Package foodieoption is the Postgres implementation of
// domain.FoodieOptionRepository: the platform-wide "Фуди-профиль" option
// dictionary and the cuisine-tile <-> cuisine-dictionary links (migration
// 0110).
package foodieoption

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

// Constraint names from migration 0110, used to tell a duplicate CODE from a
// duplicate NAME so the error text points the admin at the right field.
const (
	uqKindCode           = "uq_foodie_options_kind_code"
	uqKindNameNormalized = "uq_foodie_options_kind_name_normalized"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.FoodieOptionRepository = (*Repository)(nil)

const cols = `id, kind, code, name, name_i18n, image_url,
	description, description_i18n, price_label, price_label_i18n, price_category,
	display_order, is_active, created_at, updated_at`

// listOrder is the dictionary's canonical ordering — kind first (each of the
// four wizard steps is its own bucket), then display_order/name/id exactly
// like cuisine.listOrder, so the admin table and the wizard never disagree
// about order within a kind.
const listOrder = `ORDER BY kind ASC, display_order ASC, name ASC, id ASC`

func (r *Repository) List(ctx context.Context, f domain.FoodieOptionFilter) ([]domain.FoodieOption, error) {
	q := `SELECT ` + cols + ` FROM foodie_options`
	var (
		args  []any
		where []string
	)
	if f.Kind != nil {
		args = append(args, *f.Kind)
		where = append(where, fmt.Sprintf("kind = $%d", len(args)))
	}
	if !f.IncludeInactive {
		where = append(where, "is_active = true")
	}
	for i, w := range where {
		if i == 0 {
			q += " WHERE " + w
		} else {
			q += " AND " + w
		}
	}
	q += " " + listOrder

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list foodie options: %w", err)
	}
	defer rows.Close()

	out := make([]domain.FoodieOption, 0)
	for rows.Next() {
		o, err := scanFoodieOption(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list foodie options: %w", err)
	}

	if err := r.attachCuisines(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.FoodieOption, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+` FROM foodie_options WHERE id = $1`, id)
	o, err := scanFoodieOption(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// attachCuisines mutates the slice in place, so route o through one.
	tmp := []domain.FoodieOption{*o}
	if err := r.attachCuisines(ctx, tmp); err != nil {
		return nil, err
	}
	return &tmp[0], nil
}

func (r *Repository) Create(ctx context.Context, o *domain.FoodieOption) error {
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO foodie_options (`+cols+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now(),now())
		 RETURNING id, created_at, updated_at`,
		o.ID, o.Kind, o.Code, o.Name, i18nToDB(o.NameI18n), o.ImageURL,
		o.Description, i18nToDB(o.DescriptionI18n), o.PriceLabel, i18nToDB(o.PriceLabelI18n),
		priceCategoryToDB(o.PriceCategory), o.DisplayOrder, o.IsActive).
		Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return mapWrite(err, "create foodie option")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, o *domain.FoodieOption) error {
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`UPDATE foodie_options SET
			kind=$2, code=$3, name=$4, name_i18n=$5, image_url=$6,
			description=$7, description_i18n=$8, price_label=$9, price_label_i18n=$10,
			price_category=$11, display_order=$12, is_active=$13, updated_at=now()
		 WHERE id=$1
		 RETURNING id, created_at, updated_at`,
		o.ID, o.Kind, o.Code, o.Name, i18nToDB(o.NameI18n), o.ImageURL,
		o.Description, i18nToDB(o.DescriptionI18n), o.PriceLabel, i18nToDB(o.PriceLabelI18n),
		priceCategoryToDB(o.PriceCategory), o.DisplayOrder, o.IsActive).
		Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return mapWrite(err, "update foodie option")
	}
	return nil
}

// SetCuisineLinks replaces optionID's linked cuisine set wholesale. Delete +
// insert are separate statements: run inside a transaction (see the
// interface doc).
func (r *Repository) SetCuisineLinks(ctx context.Context, optionID uuid.UUID, cuisineIDs []uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM foodie_option_cuisines WHERE option_id = $1`, optionID); err != nil {
		return fmt.Errorf("set foodie option cuisine links: %w", err)
	}
	for _, cid := range cuisineIDs {
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO foodie_option_cuisines (option_id, cuisine_id, created_at)
			 VALUES ($1,$2,now())
			 ON CONFLICT (option_id, cuisine_id) DO NOTHING`,
			optionID, cid); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("%w: unknown foodie option or cuisine", domain.ErrValidation)
			}
			return fmt.Errorf("set foodie option cuisine links: %w", err)
		}
	}
	return nil
}

func (r *Repository) CountActive(ctx context.Context, kind domain.FoodieOptionKind) (int, error) {
	var n int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM foodie_options WHERE kind = $1 AND is_active = true`, kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active foodie options: %w", err)
	}
	return n, nil
}

func (r *Repository) AllExistingCodes(ctx context.Context) (map[domain.FoodieOptionKind]map[string]struct{}, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, `SELECT kind, code FROM foodie_options`)
	if err != nil {
		return nil, fmt.Errorf("list foodie option codes: %w", err)
	}
	defer rows.Close()

	out := make(map[domain.FoodieOptionKind]map[string]struct{}, 4)
	for rows.Next() {
		var kind, code string
		if err := rows.Scan(&kind, &code); err != nil {
			return nil, fmt.Errorf("scan foodie option code: %w", err)
		}
		k := domain.FoodieOptionKind(kind)
		if out[k] == nil {
			out[k] = make(map[string]struct{})
		}
		out[k][code] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list foodie option codes: %w", err)
	}
	return out, nil
}

// LoadTasteMappings reads, in ONE query, everything usecase/tastematch.Loader
// needs from this dictionary: the cuisine-tile -> cuisine-dictionary-code
// links (kind=cuisine) and the budget-tier -> PriceCategory links
// (kind=budget). Both active AND hidden options are included — a hidden
// option a guest still has picked must keep resolving to its mapping and
// keep scoring (criterion 12 of spec
// foodie-profile-admin-dictionaries-20260916.md).
//
// This is deliberately NOT List() + a second cuisine-link query: BE-2's own
// per-request SQL budget only has room for ONE extra query from this whole
// feature (criterion 13: "/restaurants/picks не стал медленнее чем на один
// запрос к БД, 15 -> ≤ 16"), so the two mappings are read with a single JOIN
// instead of List's "select options, then batch-attach links" shape.
func (r *Repository) LoadTasteMappings(ctx context.Context) (cuisineTiles map[string][]string, budgetTiers map[string]domain.PriceCategory, err error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT o.kind, o.code, o.price_category, c.code
		 FROM foodie_options o
		 LEFT JOIN foodie_option_cuisines fo ON fo.option_id = o.id AND o.kind = 'cuisine'
		 LEFT JOIN cuisines c ON c.id = fo.cuisine_id
		 WHERE o.kind IN ('cuisine', 'budget')`)
	if err != nil {
		return nil, nil, fmt.Errorf("load foodie taste mappings: %w", err)
	}
	defer rows.Close()

	cuisineTiles = make(map[string][]string)
	budgetTiers = make(map[string]domain.PriceCategory)
	for rows.Next() {
		var kind, code string
		var priceCategory, cuisineCode *string
		if err := rows.Scan(&kind, &code, &priceCategory, &cuisineCode); err != nil {
			return nil, nil, fmt.Errorf("scan foodie taste mapping row: %w", err)
		}
		switch domain.FoodieOptionKind(kind) {
		case domain.FoodieOptionKindCuisine:
			if _, ok := cuisineTiles[code]; !ok {
				cuisineTiles[code] = nil
			}
			if cuisineCode != nil {
				cuisineTiles[code] = append(cuisineTiles[code], *cuisineCode)
			}
		case domain.FoodieOptionKindBudget:
			if priceCategory != nil {
				budgetTiers[code] = domain.PriceCategory(*priceCategory)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("load foodie taste mappings: %w", err)
	}
	return cuisineTiles, budgetTiers, nil
}

// attachCuisines batch-loads the cuisine links of every CUISINE-kind entry in
// out and writes them back onto out[i].Cuisines, in ONE query regardless of
// how many cuisine options are in the list — the same "one query for a whole
// page" rule cuisine.Repository.ListByRestaurants follows.
func (r *Repository) attachCuisines(ctx context.Context, out []domain.FoodieOption) error {
	ids := make([]uuid.UUID, 0, len(out))
	index := make(map[uuid.UUID]int, len(out))
	for i, o := range out {
		if o.Kind != domain.FoodieOptionKindCuisine {
			continue
		}
		ids = append(ids, o.ID)
		index[o.ID] = i
	}
	if len(ids) == 0 {
		return nil
	}

	const cuisineCols = `id, code, name, name_i18n, image_url, display_order, is_active, created_at, updated_at`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT fo.option_id, `+prefixed(cuisineCols, "c")+`
		 FROM foodie_option_cuisines fo
		 JOIN cuisines c ON c.id = fo.cuisine_id
		 WHERE fo.option_id = ANY($1)
		 ORDER BY fo.option_id, c.display_order, c.name, c.id`, ids)
	if err != nil {
		return fmt.Errorf("list foodie option cuisine links: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var optionID uuid.UUID
		var c domain.Cuisine
		var nameI18n []byte
		if err := rows.Scan(&optionID, &c.ID, &c.Code, &c.Name, &nameI18n, &c.ImageURL,
			&c.DisplayOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return fmt.Errorf("scan foodie option cuisine link: %w", err)
		}
		c.NameI18n = i18nFromDB(nameI18n)
		i := index[optionID]
		out[i].Cuisines = append(out[i].Cuisines, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list foodie option cuisine links: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanFoodieOption(row scanner) (*domain.FoodieOption, error) {
	var o domain.FoodieOption
	var nameI18n, descriptionI18n, priceLabelI18n []byte
	var priceCategory *string
	if err := row.Scan(&o.ID, &o.Kind, &o.Code, &o.Name, &nameI18n, &o.ImageURL,
		&o.Description, &descriptionI18n, &o.PriceLabel, &priceLabelI18n, &priceCategory,
		&o.DisplayOrder, &o.IsActive, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan foodie option: %w", err)
	}
	o.NameI18n = i18nFromDB(nameI18n)
	o.DescriptionI18n = i18nFromDB(descriptionI18n)
	o.PriceLabelI18n = i18nFromDB(priceLabelI18n)
	if priceCategory != nil {
		pc := domain.PriceCategory(*priceCategory)
		o.PriceCategory = &pc
	}
	return &o, nil
}

// prefixed qualifies a comma-separated column list with a table alias, so a
// shared column list can be reused in a join. Identical helper to
// cuisine.prefixed; not shared across packages to avoid a cross-package
// dependency for six lines of string plumbing. strings.TrimSpace handles the
// newlines/tabs the multi-line cuisineCols const is written with, same as any
// single-line list.
func prefixed(list, alias string) string {
	parts := strings.Split(list, ",")
	for i, col := range parts {
		parts[i] = alias + "." + strings.TrimSpace(col)
	}
	return strings.Join(parts, ", ")
}

// mapWrite turns a unique-index violation into domain.ErrAlreadyExists, with
// text naming which uniqueness rule fired — code vs. name — so the admin
// knows which field to change (spec criterion 6/3.8). Never a
// read-then-write check: the two indexes are the only guard, so two admins
// racing the same code/name both hit this, one wins.
func mapWrite(err error, ctxMsg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		switch pgErr.ConstraintName {
		case uqKindCode:
			return fmt.Errorf("%w: code already used for this kind", domain.ErrAlreadyExists)
		case uqKindNameNormalized:
			return fmt.Errorf("%w: name already used for this kind", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("%w: foodie option code or name already used", domain.ErrAlreadyExists)
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

func priceCategoryToDB(pc *domain.PriceCategory) any {
	if pc == nil {
		return nil
	}
	return string(*pc)
}

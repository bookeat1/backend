// Package menu is the Postgres implementation of the menu repositories.
package menu

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

const uniqueViolation = "23505"

// Repository implements domain.MenuItemRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the menu item repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.MenuItemRepository = (*Repository)(nil)

// selCols lists menu_items columns for reads; price is rendered as text so the
// domain can carry it as a decimal string without a float round-trip.
const selCols = `id, restaurant_id, name, name_i18n, description, description_i18n,
	price::text, image_url, is_available, category, category_i18n, subcategory,
	subcategory_i18n, portion_size, portion_size_i18n, language, display_order,
	created_at, updated_at`

func (r *Repository) ListByRestaurant(ctx context.Context, f domain.MenuItemFilter) ([]domain.MenuItem, error) {
	q := `SELECT ` + selCols + ` FROM menu_items WHERE restaurant_id=$1`
	args := []any{f.RestaurantID}
	if f.Language == nil {
		q += ` AND (language = 'ru' OR language IS NULL)`
	} else {
		args = append(args, *f.Language)
		q += ` AND language = $2`
	}
	q += ` ORDER BY display_order ASC NULLS LAST, name ASC`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list menu items: %w", err)
	}
	defer rows.Close()
	var items []domain.MenuItem
	ids := []uuid.UUID{}
	for rows.Next() {
		m, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list menu items: %w", err)
	}
	if len(items) == 0 {
		return items, nil
	}
	tagsByItem, err := r.tagsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Tags = tagsByItem[items[i].ID]
	}
	return items, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.MenuItem, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+selCols+` FROM menu_items WHERE id=$1`, id)
	m, err := scanItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get menu item: %w", err)
	}
	tagsByItem, err := r.tagsFor(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	m.Tags = tagsByItem[id]
	return m, nil
}

func (r *Repository) tagsFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.MenuItemTag, error) {
	out := map[uuid.UUID][]domain.MenuItemTag{}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, menu_item_id, tag, created_at FROM menu_item_tags
		 WHERE menu_item_id = ANY($1) ORDER BY tag`, ids)
	if err != nil {
		return nil, fmt.Errorf("list menu tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t domain.MenuItemTag
		if err := rows.Scan(&t.ID, &t.MenuItemID, &t.Tag, &t.CreatedAt); err != nil {
			return nil, err
		}
		out[t.MenuItemID] = append(out[t.MenuItemID], t)
	}
	return out, rows.Err()
}

const insCols = `id, restaurant_id, name, name_i18n, description, description_i18n,
	price, image_url, is_available, category, category_i18n, subcategory,
	subcategory_i18n, portion_size, portion_size_i18n, language, display_order,
	created_at, updated_at`

func (r *Repository) Create(ctx context.Context, m *domain.MenuItem) error {
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	q := `INSERT INTO menu_items (` + insCols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(m)...); err != nil {
		return mapWrite(err, "create menu item")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, m *domain.MenuItem) error {
	m.UpdatedAt = time.Now()
	q := `UPDATE menu_items SET name=$2, name_i18n=$3, description=$4, description_i18n=$5,
		price=$6::numeric, image_url=$7, is_available=$8, category=$9, category_i18n=$10,
		subcategory=$11, subcategory_i18n=$12, portion_size=$13, portion_size_i18n=$14,
		language=$15, display_order=$16, updated_at=$17 WHERE id=$1`
	// Built explicitly (not sliced out of r.args) so adding an INSERT column
	// can't silently shift the UPDATE placeholders. Update omits restaurant_id
	// and created_at.
	args := []any{
		m.ID, m.Name, i18nToDB(m.NameI18n), m.Description, i18nToDB(m.DescriptionI18n),
		m.Price, m.ImageURL, m.IsAvailable, m.Category, i18nToDB(m.CategoryI18n),
		m.Subcategory, i18nToDB(m.SubcategoryI18n), m.PortionSize, i18nToDB(m.PortionSizeI18n),
		m.Language, m.DisplayOrder, m.UpdatedAt,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update menu item")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM menu_items WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete menu item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) SetAvailable(ctx context.Context, id uuid.UUID, available bool) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE menu_items SET is_available=$2, updated_at=now() WHERE id=$1`, id, available)
	if err != nil {
		return fmt.Errorf("set available: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetAvailableBulk flips is_available for the given ids that belong to
// restaurantID in one UPDATE. The restaurant_id predicate is the tenant guard:
// any id in the list that belongs to another venue matches zero rows and is
// left untouched. An empty ids slice short-circuits (no statement, no error).
func (r *Repository) SetAvailableBulk(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID, available bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE menu_items SET is_available=$3, updated_at=now()
		 WHERE restaurant_id=$1 AND id = ANY($2)`, restaurantID, ids, available)
	if err != nil {
		return 0, fmt.Errorf("set available bulk: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (r *Repository) ReplaceTags(ctx context.Context, itemID uuid.UUID, tags []domain.MenuItemTag) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM menu_item_tags WHERE menu_item_id=$1`, itemID); err != nil {
		return fmt.Errorf("replace tags: %w", err)
	}
	for i := range tags {
		if tags[i].ID == uuid.Nil {
			tags[i].ID = uuid.New()
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO menu_item_tags (id, menu_item_id, tag, created_at) VALUES ($1,$2,$3,now())`,
			tags[i].ID, itemID, tags[i].Tag); err != nil {
			// Map the UNIQUE(menu_item_id, tag) violation to ErrAlreadyExists
			// (409) instead of leaking a raw 500. The facade also de-dups, so
			// this is a backstop for any duplicate that slips through.
			return mapWrite(err, "replace tags")
		}
	}
	return nil
}

func (r *Repository) args(m *domain.MenuItem) []any {
	return []any{
		m.ID, m.RestaurantID, m.Name, i18nToDB(m.NameI18n), m.Description,
		i18nToDB(m.DescriptionI18n), m.Price, m.ImageURL, m.IsAvailable, m.Category,
		i18nToDB(m.CategoryI18n), m.Subcategory, i18nToDB(m.SubcategoryI18n), m.PortionSize,
		i18nToDB(m.PortionSizeI18n), m.Language, m.DisplayOrder, m.CreatedAt, m.UpdatedAt,
	}
}

type scanner interface{ Scan(dest ...any) error }

func scanItem(row scanner) (*domain.MenuItem, error) {
	var m domain.MenuItem
	var name, desc, cat, subcat, portion []byte
	if err := row.Scan(
		&m.ID, &m.RestaurantID, &m.Name, &name, &m.Description, &desc, &m.Price,
		&m.ImageURL, &m.IsAvailable, &m.Category, &cat, &m.Subcategory, &subcat,
		&m.PortionSize, &portion, &m.Language, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
	); err != nil {
		return nil, err
	}
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CategoryI18n = i18nFromDB(cat)
	m.SubcategoryI18n = i18nFromDB(subcat)
	m.PortionSizeI18n = i18nFromDB(portion)
	return &m, nil
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

func mapWrite(err error, ctx string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, ctx)
	}
	return fmt.Errorf("%s: %w", ctx, err)
}

package menu

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Categories implements domain.MenuCategoryRepository.
type Categories struct{ pool sqltx.Querier }

// NewCategories builds the menu category repository.
func NewCategories(pool sqltx.Querier) *Categories { return &Categories{pool: pool} }

var _ domain.MenuCategoryRepository = (*Categories)(nil)

func (c *Categories) List(ctx context.Context) ([]domain.MenuCategory, error) {
	rows, err := sqltx.From(ctx, c.pool).Query(ctx,
		`SELECT id, name, name_i18n, parent_id, display_order, created_at
		 FROM menu_categories ORDER BY display_order, name`)
	if err != nil {
		return nil, fmt.Errorf("list menu categories: %w", err)
	}
	defer rows.Close()
	var out []domain.MenuCategory
	for rows.Next() {
		var cat domain.MenuCategory
		var i18n []byte
		if err := rows.Scan(&cat.ID, &cat.Name, &i18n, &cat.ParentID, &cat.DisplayOrder, &cat.CreatedAt); err != nil {
			return nil, err
		}
		cat.NameI18n = i18nFromDB(i18n)
		out = append(out, cat)
	}
	return out, rows.Err()
}

func (c *Categories) Create(ctx context.Context, cat *domain.MenuCategory) error {
	if cat.ID == uuid.Nil {
		cat.ID = uuid.New()
	}
	cat.CreatedAt = time.Now()
	_, err := sqltx.From(ctx, c.pool).Exec(ctx,
		`INSERT INTO menu_categories (id, name, name_i18n, parent_id, display_order, created_at)
		 VALUES ($1,$2,$3,$4,$5,now())`,
		cat.ID, cat.Name, i18nToDB(cat.NameI18n), cat.ParentID, cat.DisplayOrder)
	if err != nil {
		return mapWrite(err, "create menu category")
	}
	return nil
}

func (c *Categories) Update(ctx context.Context, cat *domain.MenuCategory) error {
	tag, err := sqltx.From(ctx, c.pool).Exec(ctx,
		`UPDATE menu_categories SET name=$2, name_i18n=$3, parent_id=$4, display_order=$5 WHERE id=$1`,
		cat.ID, cat.Name, i18nToDB(cat.NameI18n), cat.ParentID, cat.DisplayOrder)
	if err != nil {
		return mapWrite(err, "update menu category")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (c *Categories) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, c.pool).Exec(ctx, `DELETE FROM menu_categories WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete menu category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

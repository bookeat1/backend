package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// MenuCategory is a back-office category reference with an optional parent
// (hierarchy). It is not FK-linked to menu items (items carry a text category).
type MenuCategory struct {
	ID           uuid.UUID
	Name         string
	NameI18n     I18n
	ParentID     *uuid.UUID
	DisplayOrder int
	CreatedAt    time.Time
}

// MenuCategoryRepository persists menu categories.
type MenuCategoryRepository interface {
	List(ctx context.Context) ([]MenuCategory, error)
	Create(ctx context.Context, c *MenuCategory) error
	Update(ctx context.Context, c *MenuCategory) error
	Delete(ctx context.Context, id uuid.UUID) error
}

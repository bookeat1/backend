package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Cuisine is a cuisine tag shown in the mobile Home ("Explore") screen's
// "Выберите кухню" picker. Read-only from the app; curated elsewhere.
type Cuisine struct {
	ID        uuid.UUID
	Name      string
	ImageURL  *string
	Sort      int
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CuisineRepository reads the cuisine catalog.
type CuisineRepository interface {
	// ListActive returns active cuisines ordered by sort, then name.
	ListActive(ctx context.Context) ([]Cuisine, error)
}

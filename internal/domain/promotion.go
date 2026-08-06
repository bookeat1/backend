package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Promotion is a marketing offer shown in the mobile Home ("Explore") screen's
// "Акции" strip. RestaurantID is optional: a promotion may be global (nil) or
// tied to a specific venue. StartsAt/EndsAt bound its active window; a nil bound
// means "open-ended" on that side.
type Promotion struct {
	ID            uuid.UUID
	RestaurantID  *uuid.UUID
	Title         string
	DiscountLabel *string
	StartsAt      *time.Time
	EndsAt        *time.Time
	ImageURL      *string
	IsActive      bool
	Sort          int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PromotionRepository reads the promotions catalog.
type PromotionRepository interface {
	// ListActive returns active promotions whose window contains the current
	// time (a NULL bound is treated as open-ended on that side), ordered by
	// sort. The window is evaluated against the database clock.
	ListActive(ctx context.Context) ([]Promotion, error)
}

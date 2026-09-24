package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// KwaakaOrderSettings is a venue's kitchen-order sending config
// (restaurant_kwaaka_order_settings + its table pool). Superadmin-managed.
type KwaakaOrderSettings struct {
	RestaurantID       uuid.UUID
	OrdersEnabled      bool
	KwaakaRestaurantID string // linkage the pool was chosen for
	LeadMinutes        *int   // nil = platform default
	EnabledAt          *time.Time
	UpdatedAt          time.Time
	UpdatedBy          *uuid.UUID
	Pool               []KwaakaPoolTable
}

// KwaakaPoolTable is one POS table of the pool. Position is the priority.
type KwaakaPoolTable struct {
	KwaakaTableID string
	Position      int
	Label         string
}

// KwaakaOrderSettingsRepository persists the settings and pool.
type KwaakaOrderSettingsRepository interface {
	// Get returns the settings with pool (ordered by position); ErrNotFound if none.
	Get(ctx context.Context, restaurantID uuid.UUID) (*KwaakaOrderSettings, error)
	// LockForUpdate is Get with SELECT ... FOR UPDATE (inside a transaction).
	LockForUpdate(ctx context.Context, restaurantID uuid.UUID) (*KwaakaOrderSettings, error)
	// Save upserts the settings row and replaces the pool wholesale. enabled_at
	// is set here only on the false→true transition (or first insert enabled).
	Save(ctx context.Context, s *KwaakaOrderSettings) error
}

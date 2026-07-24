package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PromoStatus is a promo's publication state, stored as VARCHAR (validated
// here, never a Postgres ENUM). Mirrors EventStatus: draft → published →
// hidden. Only a published promo whose validity window contains "now" is ever
// served by the public listing.
type PromoStatus string

const (
	// PromoDraft is a work-in-progress promo, invisible to guests.
	PromoDraft PromoStatus = "draft"
	// PromoPublished is eligible for the public active-promos listing (subject
	// to its validity window still containing now).
	PromoPublished PromoStatus = "published"
	// PromoHidden was published once but is now withdrawn from public view.
	PromoHidden PromoStatus = "hidden"
)

// Valid reports whether s is a known promo status.
func (s PromoStatus) Valid() bool {
	switch s {
	case PromoDraft, PromoPublished, PromoHidden:
		return true
	}
	return false
}

// Promo is a time-boxed offer a restaurant runs (a happy hour, a seasonal set
// menu discount). Title/Description are localized like the catalog (base ru
// column + optional *_i18n jsonb — see I18n.Resolve). StartsAt/EndsAt is the
// validity window: the public listing shows a promo only while
// StartsAt <= now < EndsAt AND Status == published.
type Promo struct {
	ID              uuid.UUID
	RestaurantID    uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	StartsAt        time.Time
	EndsAt          time.Time
	// Terms is free-text fine print ("dine-in only, not combinable with other
	// offers"). Not localized in this increment.
	Terms     string
	Status    PromoStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PromoRepository persists restaurant promos. Get* return ErrNotFound when
// absent.
type PromoRepository interface {
	// Create inserts a new promo. An unknown restaurant_id (FK violation) maps
	// to ErrNotFound.
	Create(ctx context.Context, p *Promo) error
	// GetByID returns a promo by its id regardless of status.
	GetByID(ctx context.Context, id uuid.UUID) (*Promo, error)
	// Update overwrites the mutable fields of an existing promo by id. Returns
	// ErrNotFound if id is absent.
	Update(ctx context.Context, p *Promo) error
	// Delete removes a promo. Returns ErrNotFound if id is absent.
	Delete(ctx context.Context, id uuid.UUID) error
	// ListByRestaurant returns a restaurant's promos for the admin cabinet,
	// optionally filtered to the given statuses (empty = all), newest-start
	// first with id as a stable tie-breaker, paginated, plus the total count.
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []PromoStatus, page, perPage int) ([]Promo, int, error)
	// ListActive returns a restaurant's PUBLISHED promos whose validity window
	// contains now (starts_at <= now AND ends_at > now), soonest-to-expire
	// first with id as a stable tie-breaker, paginated, plus the total count.
	// This is the public listing.
	ListActive(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]Promo, int, error)
}

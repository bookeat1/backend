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
	Terms string
	// CoverImageURL is the promo card's picture — the FULL public URL, exactly
	// like Event.CoverImageURL and Image.ImageURL. Nil means the promo has no
	// picture: the API omits the field rather than inventing a placeholder, and
	// the client draws its own.
	CoverImageURL *string
	// Images — дополнительная галерея акции в порядке редактора, БЕЗ обложки
	// (та в CoverImageURL). Пустой срез — «галереи нет».
	Images []string
	// DiscountPercent is the whole-percent price cut the card's «−30%» badge
	// renders, 0..100. Nil means the promo is not a percentage-off offer: the
	// API omits the field and the client draws no badge (a NULL is not a 0%).
	DiscountPercent *int
	Status          PromoStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
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
	// ReplaceImages overwrites the promo's gallery with urls, in order. An empty
	// slice clears it. The cover is NOT part of this set.
	ReplaceImages(ctx context.Context, promoID uuid.UUID, urls []string) error
	// ImagesByPromo loads galleries for several promos at once.
	ImagesByPromo(ctx context.Context, promoIDs []uuid.UUID) (map[uuid.UUID][]string, error)
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

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

// Promo is a time-boxed offer (a happy hour, a seasonal set menu discount). It
// is run EITHER by a restaurant or by the platform itself — see RestaurantID. Title/Description are localized like the catalog (base ru
// column + optional *_i18n jsonb — see I18n.Resolve). StartsAt/EndsAt is the
// validity window: the public listing shows a promo only while
// StartsAt <= now < EndsAt AND Status == published.
type Promo struct {
	ID uuid.UUID
	// RestaurantID is the venue running the offer, and nil means the PLATFORM
	// itself runs it — «акция без привязки к ресторану» (migration 0085). The
	// mirror of Event.RestaurantID, with the same two consequences: only the
	// platform may create or edit it, and the card carries no venue.
	RestaurantID    *uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	StartsAt        time.Time
	EndsAt          time.Time
	// Terms is free-text fine print ("dine-in only, not combinable with other
	// offers"). Localized like the rest of the card: the column is the Russian
	// text and TermsI18n carries the translations (see I18n.Resolve).
	Terms     string
	TermsI18n I18n
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
	// City OVERRIDES the city this promo is shown in (migration 0085). Exactly
	// the shape events got in 0084, deliberately and not a second invention:
	//
	//   nil + a venue    → the promo lives in the VENUE's city, resolved on
	//                      every read (COALESCE(p.city, r.city)), so it can
	//                      never go stale when the venue moves. This is what
	//                      every promo written before 0085 has.
	//   nil + no venue   → shown in EVERY city.
	//   set              → shown in that city whatever the venue says.
	//
	// The stored value is the dictionary's own spelling of the city name, kept
	// in step by the database trigger trg_promos_sync_city.
	City      *City
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsPlatform reports whether the platform itself runs this promo (no venue).
func (p Promo) IsPlatform() bool { return p.RestaurantID == nil }

// PromoListItem is one row of a cross-venue promo read: the promo plus the
// venue that runs it, so a card needs no per-item follow-up query. It mirrors
// EventListItem and reuses EventRestaurant — that type is the minimal venue
// identity a content card carries, named after the listing it first served
// rather than after events specifically.
// Restaurant is nil for a PLATFORM promo — see EventListItem.Restaurant for
// why this is a pointer and not a zero value.
type PromoListItem struct {
	Promo
	Restaurant *EventRestaurant
}

// PublicPromoFilter narrows the cross-venue public promos listing. Every filter
// is optional; the zero value lists every visible promo on the platform.
// Visibility itself is NOT a filter — published, inside its window, at an
// active venue (or hosted by the platform) is always enforced, see
// PromoRepository.ListPublicActive.
type PublicPromoFilter struct {
	// City filters by the promo's EFFECTIVE city: its own override when set,
	// otherwise the venue's. A promo with no effective city at all — a platform
	// promo with no override — is shown for every value of this filter. Same
	// contract, word for word, as PublicEventFilter.City.
	City *City
	// RestaurantID narrows to one venue.
	RestaurantID *uuid.UUID
	Page         int // 1-based; <=0 means 1
	// PerPage <=0 means the default (20). The transport layer caps it.
	PerPage int
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
	// ListPlatform is ListByRestaurant for the promos NOBODY runs but us
	// (restaurant_id IS NULL). See EventRepository.ListPlatform.
	ListPlatform(ctx context.Context, statuses []PromoStatus, page, perPage int) ([]Promo, int, error)
	// ListActive returns a restaurant's PUBLISHED promos whose validity window
	// contains now (starts_at <= now AND ends_at > now), soonest-to-expire
	// first with id as a stable tie-breaker, paginated, plus the total count.
	// This is the public listing.
	ListActive(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]Promo, int, error)
	// ListPublicActive is the CROSS-VENUE public listing, the promo twin of
	// EventRepository.ListPublicUpcoming: PUBLISHED promos inside their window
	// (starts_at <= now < ends_at) run either by an ACTIVE restaurant or by the
	// platform itself, soonest-to-expire first with id as a stable tie-breaker,
	// narrowed by f, paginated, plus the total count. The venue is joined in
	// (PromoListItem.Restaurant, nil for a platform promo) so a card needs no
	// follow-up query.
	ListPublicActive(ctx context.Context, f PublicPromoFilter, now time.Time) ([]PromoListItem, int, error)
	// GetPublic returns ONE published promo inside its window, whoever runs it,
	// with its venue when it has one. ErrNotFound for a draft/hidden/expired
	// promo — to a guest it simply does not exist.
	GetPublic(ctx context.Context, id uuid.UUID, now time.Time) (*PromoListItem, error)
}

package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Favorite links a user to a restaurant they bookmarked.
type Favorite struct {
	UserID       uuid.UUID
	RestaurantID uuid.UUID
	CreatedAt    time.Time
}

// FavoriteItemKind names what a favorites-screen row is made of. Restaurants,
// events and promos are three different entities with three different cards;
// the guest screen shows them under «Все / Рестораны / События / Акции», and
// this is the discriminator the combined read carries.
type FavoriteItemKind string

const (
	// FavoriteRestaurant is a bookmarked venue (restaurant_favorites).
	FavoriteRestaurant FavoriteItemKind = "restaurant"
	// FavoriteEvent is a bookmarked event or event series (event_favorites).
	FavoriteEvent FavoriteItemKind = "event"
	// FavoritePromo is a bookmarked promo (promo_favorites).
	FavoritePromo FavoriteItemKind = "promo"
)

// Valid reports whether k is a known favorite kind.
func (k FavoriteItemKind) Valid() bool {
	switch k {
	case FavoriteRestaurant, FavoriteEvent, FavoritePromo:
		return true
	}
	return false
}

// FavoriteRestaurantItem is a bookmarked venue plus WHEN it was bookmarked. The
// venue payload is the exact same public shape the catalog listing returns —
// the favorites screen renders the catalog's card, not a second one. FavoritedAt
// exists so a client can merge the three kinds into one chronological «Все» tab
// without asking the server for a fourth list.
type FavoriteRestaurantItem struct {
	Restaurant  RestaurantListItem
	FavoritedAt time.Time
}

// FavoriteEventItem is a bookmarked event resolved to the occurrence a guest can
// actually open right now, plus the venue that hosts it.
//
// SeriesID is set when the bookmark is series-level (the guest saved a recurring
// event): the Event carried here is then the NEAREST UPCOMING occurrence of that
// series at read time, not the date the guest happened to tap. For a one-off
// event SeriesID is nil and Event is that event.
type FavoriteEventItem struct {
	EventListItem
	SeriesID    *uuid.UUID
	FavoritedAt time.Time
}

// FavoritePromoItem is a bookmarked promo that is still live, plus the venue
// running it.
type FavoritePromoItem struct {
	PromoListItem
	FavoritedAt time.Time
}

// FavoriteCollection is everything one guest has bookmarked, in three typed
// lists, each newest-saved first. Only items the guest can still open are here —
// see FavoriteRepository.ListEventsByUser / ListPromosByUser for the exact
// visibility rule.
type FavoriteCollection struct {
	Restaurants []FavoriteRestaurantItem
	Events      []FavoriteEventItem
	Promos      []FavoritePromoItem
}

// FavoriteRepository persists a user's bookmarked restaurants. Every operation
// is scoped by userID: there is no "get favorite by id" — a favorite is
// addressed by the (user, restaurant) pair only, so a caller can never reach
// another user's bookmark.
type FavoriteRepository interface {
	// Add bookmarks restaurantID for userID. Idempotent: bookmarking an
	// already-favorited restaurant is a silent no-op, not an error. Returns
	// ErrNotFound if restaurantID does not exist.
	Add(ctx context.Context, userID, restaurantID uuid.UUID) error
	// Remove un-bookmarks restaurantID for userID. Idempotent: removing a
	// favorite that doesn't exist (or never existed) is a silent no-op, not
	// an error.
	Remove(ctx context.Context, userID, restaurantID uuid.UUID) error
	// ListByUser returns userID's bookmarked restaurants that are still
	// active, most recently favorited first. A restaurant deactivated after
	// being favorited is excluded, same visibility rule as the public catalog.
	ListByUser(ctx context.Context, userID uuid.UUID) ([]RestaurantListItem, error)
	// FavoriteSet reports which of restaurantIDs are favorited by userID.
	// A restaurant id absent from the returned map is "not favorited" — the
	// map never holds an explicit false entry.
	FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// ListRestaurantsByUser is ListByUser plus the timestamp each venue was
	// bookmarked at — the combined favorites screen needs it to interleave the
	// three kinds. Same visibility rule (active venues only), same order.
	ListRestaurantsByUser(ctx context.Context, userID uuid.UUID) ([]FavoriteRestaurantItem, error)

	// AddEvent bookmarks eventID for userID. A GENERATED occurrence is stored
	// against its series (events.recurrence_id), a one-off event against
	// itself — see the read contract on ListEventsByUser for why. Idempotent:
	// saving the same event, or a second occurrence of the same series, is a
	// silent no-op. Returns ErrNotFound if eventID does not exist.
	AddEvent(ctx context.Context, userID, eventID uuid.UUID) error
	// RemoveEvent un-bookmarks eventID for userID. When eventID belongs to a
	// series, the SERIES bookmark is removed — whichever occurrence the guest
	// tapped the filled heart on. Idempotent: removing something not saved (or
	// an id that does not exist) is a silent no-op.
	RemoveEvent(ctx context.Context, userID, eventID uuid.UUID) error
	// ListEventsByUser returns the user's bookmarked events, most recently saved
	// first, each resolved to a card the guest can open:
	//   - a series bookmark resolves to its NEAREST UPCOMING occurrence, so a
	//     saved weekly event never rots into a past date;
	//   - only published events at an active venue that have not ended
	//     (ends_at > now) are returned — the same visibility rule as the public
	//     Афиша (ListPublicUpcoming);
	//   - a bookmark whose target is currently invisible (unpublished, ended,
	//     venue deactivated) is simply absent from the result. The ROW survives,
	//     so re-publishing brings the item back with the heart still on.
	ListEventsByUser(ctx context.Context, userID uuid.UUID, now time.Time) ([]FavoriteEventItem, error)

	// AddPromo bookmarks promoID for userID. Idempotent. Returns ErrNotFound if
	// promoID does not exist.
	AddPromo(ctx context.Context, userID, promoID uuid.UUID) error
	// RemovePromo un-bookmarks promoID for userID. Idempotent.
	RemovePromo(ctx context.Context, userID, promoID uuid.UUID) error
	// ListPromosByUser returns the user's bookmarked promos that are published,
	// inside their validity window at now and run by an active venue — the same
	// rule as the public listing (PromoRepository.ListActive). Most recently
	// saved first. An expired or withdrawn promo is absent, not flagged; its row
	// survives so a re-run promo comes back.
	ListPromosByUser(ctx context.Context, userID uuid.UUID, now time.Time) ([]FavoritePromoItem, error)
}

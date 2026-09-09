package favorites

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	restaurantsrest "backend-core/internal/transport/rest/restaurants"
)

// RegisterContentRoutes mounts the event/promo bookmark writes and the combined
// favorites read. Mount on a group already protected by middleware.Auth, exactly
// like RegisterRoutes — every operation acts on the caller's own user id only.
//
// The two write pairs deliberately sit on the ITEM's own path rather than under
// /favorites: gin builds one radix tree per method, and /favorites/:restaurantId
// already owns that position for PUT and DELETE, so /favorites/events/:eventId
// would panic the router at boot (see routes_conflict_test.go). Hanging the
// action off the resource reads better anyway — it is the same
// "PUT /<thing>/<id>/favorite" shape for both kinds.
func (h *Handler) RegisterContentRoutes(rg *gin.RouterGroup) {
	rg.GET("/favorites/items", h.listAll)
	rg.PUT("/events/:eventId/favorite", h.addEvent)
	rg.DELETE("/events/:eventId/favorite", h.removeEvent)
	rg.PUT("/promos/:promoId/favorite", h.addPromo)
	rg.DELETE("/promos/:promoId/favorite", h.removePromo)
}

// favoriteItemResponse is one row of the favorites screen. The kind is the
// discriminator the four tabs («Все / Рестораны / События / Акции») switch on,
// and exactly one of the three payload fields is present.
//
// Each payload is the SAME shape the item has elsewhere in the API — a venue is
// serialized by the catalog's own PublicListItem — so the client reuses its
// existing cards instead of maintaining a favorites-only variant.
type favoriteItemResponse struct {
	Kind string `json:"kind"`
	// FavoritedAt is when the guest saved it. Present on every kind, which is
	// what lets the client build the mixed «Все» list from this one response
	// without asking the server for a fourth ordering.
	FavoritedAt string                 `json:"favorited_at"`
	Restaurant  any                    `json:"restaurant,omitempty"`
	Event       *favoriteEventResponse `json:"event,omitempty"`
	Promo       *favoritePromoResponse `json:"promo,omitempty"`
}

// favoriteEventResponse carries everything an «Афиша» card draws, venue name
// included, so the screen never fans out to fetch each event.
type favoriteEventResponse struct {
	ID string `json:"id"`
	// RestaurantID / RestaurantName are ABSENT for a PLATFORM event — one the
	// platform itself hosts, with no venue at all (migration 0085). They used
	// to be unconditional; a client must now treat their absence as a real
	// state and draw the card without a venue line, not as broken data.
	RestaurantID   *string `json:"restaurant_id,omitempty"`
	RestaurantName string  `json:"restaurant_name,omitempty"`
	// City is the item's EFFECTIVE city: its venue's, or its own override when
	// it has no venue. Empty means a platform item that runs in every city.
	City          string  `json:"city"`
	Title         string  `json:"title"`
	Description   string  `json:"description"`
	StartsAt      string  `json:"starts_at"`
	EndsAt        string  `json:"ends_at"`
	Venue         string  `json:"venue,omitempty"`
	CoverImageURL *string `json:"cover_image_url,omitempty"`
	// Tags is always an array (never null): the card draws a chip row.
	Tags             []string `json:"tags"`
	Ticketed         bool     `json:"ticketed"`
	TicketPriceMinor *int64   `json:"ticket_price_minor,omitempty"`
	// IsRecurring / RecurrenceID say that this card is one date of a series and
	// that the bookmark follows the SERIES: the id above is whichever occurrence
	// is next right now, and it will be a different one next week. A client must
	// therefore not cache this id as "the favorited event".
	IsRecurring  bool    `json:"is_recurring"`
	RecurrenceID *string `json:"recurrence_id,omitempty"`
}

// favoritePromoResponse carries everything a promo card draws, including the
// validity window the card renders as «до 31 августа».
type favoritePromoResponse struct {
	ID string `json:"id"`
	// RestaurantID / RestaurantName are absent for a PLATFORM promo — see
	// favoriteEventResponse above.
	RestaurantID   *string `json:"restaurant_id,omitempty"`
	RestaurantName string  `json:"restaurant_name,omitempty"`
	// City is the effective city; empty = "every city" (platform item).
	City            string  `json:"city"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	Terms           string  `json:"terms,omitempty"`
	StartsAt        string  `json:"starts_at"`
	EndsAt          string  `json:"ends_at"`
	CoverImageURL   *string `json:"cover_image_url,omitempty"`
	DiscountPercent *int    `json:"discount_percent,omitempty"`
}

// favoriteCountsResponse is the per-tab badge. It always counts ALL kinds, even
// when ?type= narrowed the returned items — a tab bar that only knew about the
// tab you are on would need a second request to draw itself.
type favoriteCountsResponse struct {
	All         int `json:"all"`
	Restaurants int `json:"restaurants"`
	Events      int `json:"events"`
	Promos      int `json:"promos"`
}

// listAll answers the favorites screen in ONE request: every bookmarked venue,
// event and promo, newest-saved first, each carrying its whole card.
//
// ?type=restaurant|event|promo returns only that kind (the counts still cover
// all three). It is an optimisation for a deep link into a single tab, not the
// normal path: the screen loads everything once and switches tabs locally.
//
// Items the guest can no longer open are absent, not flagged: an unpublished or
// expired item has no public page to navigate to, so a card for it could only
// lead to a 404. The bookmark row itself survives, so a re-published item
// returns to the screen with the heart still on it.
// @Summary     List everything I favorited (venues, events, promos)
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       type query string false "restaurant|event|promo; omit for all"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid type"
// @Router      /api/v1/favorites/items [get]
func (h *Handler) listAll(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	kind := domain.FavoriteItemKind(c.Query("type"))
	if kind != "" && !kind.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid type")
		return
	}
	col, err := h.facade.ListAll(c.Request.Context(), au.ID)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)

	items := make([]favoriteItemResponse, 0,
		len(col.Restaurants)+len(col.Events)+len(col.Promos))
	if kind == "" || kind == domain.FavoriteRestaurant {
		for _, it := range col.Restaurants {
			items = append(items, favoriteItemResponse{
				Kind:        string(domain.FavoriteRestaurant),
				FavoritedAt: it.FavoritedAt.UTC().Format(time.RFC3339),
				Restaurant:  restaurantsrest.PublicListItem(it.Restaurant, lang),
			})
		}
	}
	if kind == "" || kind == domain.FavoriteEvent {
		for _, it := range col.Events {
			ev := eventItem(it, lang)
			items = append(items, favoriteItemResponse{
				Kind:        string(domain.FavoriteEvent),
				FavoritedAt: it.FavoritedAt.UTC().Format(time.RFC3339),
				Event:       &ev,
			})
		}
	}
	if kind == "" || kind == domain.FavoritePromo {
		for _, it := range col.Promos {
			pr := promoItem(it, lang)
			items = append(items, favoriteItemResponse{
				Kind:        string(domain.FavoritePromo),
				FavoritedAt: it.FavoritedAt.UTC().Format(time.RFC3339),
				Promo:       &pr,
			})
		}
	}

	response.OK(c.Writer, gin.H{
		"items": items,
		"counts": favoriteCountsResponse{
			All:         len(col.Restaurants) + len(col.Events) + len(col.Promos),
			Restaurants: len(col.Restaurants),
			Events:      len(col.Events),
			Promos:      len(col.Promos),
		},
	})
}

func eventItem(it domain.FavoriteEventItem, lang string) favoriteEventResponse {
	e := it.Event
	out := favoriteEventResponse{
		ID:               e.ID.String(),
		Title:            e.TitleI18n.Resolve(lang, e.Title),
		Description:      e.DescriptionI18n.Resolve(lang, e.Description),
		StartsAt:         e.StartsAt.UTC().Format(time.RFC3339),
		EndsAt:           e.EndsAt.UTC().Format(time.RFC3339),
		Venue:            e.VenueI18n.Resolve(lang, e.Venue),
		CoverImageURL:    e.CoverImageURL,
		Tags:             tagsOrEmpty(e.Tags),
		Ticketed:         e.Ticketed,
		TicketPriceMinor: e.TicketPriceMinor,
	}
	// A platform event carries no venue, so nothing here may dereference one:
	// this used to be three unconditional reads through it.Restaurant and would
	// have panicked on the guest's own favorites screen.
	if it.Restaurant != nil {
		id := it.Restaurant.ID.String()
		out.RestaurantID = &id
		out.RestaurantName = it.Restaurant.NameI18n.Resolve(lang, it.Restaurant.Name)
		out.City = string(it.Restaurant.City)
	} else if e.City != nil {
		out.City = string(*e.City)
	}
	if it.SeriesID != nil {
		s := it.SeriesID.String()
		out.IsRecurring = true
		out.RecurrenceID = &s
	}
	return out
}

func promoItem(it domain.FavoritePromoItem, lang string) favoritePromoResponse {
	p := it.Promo
	out := favoritePromoResponse{
		ID:              p.ID.String(),
		Title:           p.TitleI18n.Resolve(lang, p.Title),
		Description:     p.DescriptionI18n.Resolve(lang, p.Description),
		Terms:           p.TermsI18n.Resolve(lang, p.Terms),
		StartsAt:        p.StartsAt.UTC().Format(time.RFC3339),
		EndsAt:          p.EndsAt.UTC().Format(time.RFC3339),
		CoverImageURL:   p.CoverImageURL,
		DiscountPercent: p.DiscountPercent,
	}
	if it.Restaurant != nil {
		id := it.Restaurant.ID.String()
		out.RestaurantID = &id
		out.RestaurantName = it.Restaurant.NameI18n.Resolve(lang, it.Restaurant.Name)
		out.City = string(it.Restaurant.City)
	} else if p.City != nil {
		out.City = string(*p.City)
	}
	return out
}

// tagsOrEmpty keeps the JSON tags field an array rather than null.
func tagsOrEmpty(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// addEvent bookmarks an event for the authenticated caller. Idempotent, and for
// a recurring event it saves the SERIES: saving a second Wednesday of the same
// weekly event is the same bookmark, not a second one.
// @Summary     Add an event to my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       eventId path string true "Event id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     404 {object} response.Envelope "event not found"
// @Failure     422 {object} response.Envelope "invalid event id"
// @Router      /api/v1/events/{eventId}/favorite [put]
func (h *Handler) addEvent(c *gin.Context) {
	h.write(c, "eventId", "invalid event id", "favorited", h.facade.AddEvent)
}

// removeEvent un-bookmarks an event. Idempotent; removes the series bookmark
// when the id names an occurrence of a series.
// @Summary     Remove an event from my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       eventId path string true "Event id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid event id"
// @Router      /api/v1/events/{eventId}/favorite [delete]
func (h *Handler) removeEvent(c *gin.Context) {
	h.write(c, "eventId", "invalid event id", "unfavorited", h.facade.RemoveEvent)
}

// addPromo bookmarks a promo for the authenticated caller. Idempotent.
// @Summary     Add a promo to my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       promoId path string true "Promo id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     404 {object} response.Envelope "promo not found"
// @Failure     422 {object} response.Envelope "invalid promo id"
// @Router      /api/v1/promos/{promoId}/favorite [put]
func (h *Handler) addPromo(c *gin.Context) {
	h.write(c, "promoId", "invalid promo id", "favorited", h.facade.AddPromo)
}

// removePromo un-bookmarks a promo. Idempotent.
// @Summary     Remove a promo from my favorites
// @Tags        favorites
// @Produce     json
// @Security    BearerAuth
// @Param       promoId path string true "Promo id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid promo id"
// @Router      /api/v1/promos/{promoId}/favorite [delete]
func (h *Handler) removePromo(c *gin.Context) {
	h.write(c, "promoId", "invalid promo id", "unfavorited", h.facade.RemovePromo)
}

// write is the shared body of the four bookmark writes: same auth guard, same
// id parsing, same idempotent 200. Kept in one place so a new kind cannot ship
// with a subtly different status code or a missing auth check.
func (h *Handler) write(c *gin.Context, param, badIDMsg, okStatus string,
	action func(ctx context.Context, userID, itemID uuid.UUID) error) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := uuid.Parse(c.Param(param))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, badIDMsg)
		return
	}
	if err := action(c.Request.Context(), au.ID, id); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": okStatus})
}

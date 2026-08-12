// Package gastroguide exposes the guest-facing HTTP endpoints of the editorial
// guide. All routes are public reads, localized via reqlocale, and mounted on
// the plain /api/v1 group — there is nothing to personalize here (unlike the
// merchandising feed, which sits on OptionalAuth because it ranks by the
// guest's cuisine preferences), so the extra user lookup would buy nothing.
//
// Why these are separate endpoints and not extra blocks inside GET /feed:
// /feed is a RANKED rail of time-boxed items a venue submitted and the platform
// moderated — every card carries a window, a moderation state, a score and its
// breakdown, and the whole rail is city-mandatory. A guide collection has none
// of those: it is evergreen, written by us, ordered by hand and readable
// without a city. Folding it in would either force a fake window and a fake
// score onto every collection, or turn the feed's page into a union of two
// incompatible card shapes that the client must branch on anyway.
//
// The app composes the home screen from several independent calls (feed,
// guide), which also means a slow or empty guide does not take the promo rail
// down with it.
package gastroguide

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/reqlocale"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/gastroguide"
)

const (
	defaultPerPage = 20
	maxPerPage     = 100
)

// Handler serves the gastroguide endpoints.
type Handler struct{ facade uc.Facade }

// NewHandler builds the gastroguide HTTP handler.
func NewHandler(f uc.Facade) *Handler { return &Handler{facade: f} }

// RegisterPublic mounts the guest read routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/gastroguide/categories", h.listCategories)
	rg.GET("/gastroguide/collections", h.listCollections)
	rg.GET("/gastroguide/collections/:slug", h.getCollection)
}

func (h *Handler) listCategories(c *gin.Context) {
	city, ok := optionalCity(c)
	if !ok {
		return
	}
	items, err := h.facade.ListCategories(c.Request.Context(), city)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]categoryResponse, 0, len(items))
	for _, cat := range items {
		out = append(out, newCategoryResponse(cat, lang))
	}
	response.OK(c.Writer, gin.H{"items": out})
}

func (h *Handler) listCollections(c *gin.Context) {
	city, ok := optionalCity(c)
	if !ok {
		return
	}
	var categorySlug *string
	if raw := strings.TrimSpace(c.Query("category")); raw != "" {
		categorySlug = &raw
	}
	page, perPage := pagination(c)
	items, total, err := h.facade.ListCollections(c.Request.Context(), uc.ListInput{
		City: city, CategorySlug: categorySlug, Page: page, PerPage: perPage,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := make([]collectionResponse, 0, len(items))
	for _, col := range items {
		out = append(out, newCollectionResponse(col, lang))
	}
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) getCollection(c *gin.Context) {
	detail, err := h.facade.GetCollection(c.Request.Context(), c.Param("slug"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := reqlocale.Resolve(c)
	out := collectionDetailResponse{
		collectionResponse: newCollectionResponse(detail.GuideCollection, lang),
		Venues:             make([]venueResponse, 0, len(detail.Venues)),
	}
	for _, v := range detail.Venues {
		out.Venues = append(out.Venues, newVenueResponse(v, lang))
	}
	response.OK(c.Writer, out)
}

// --- helpers ---

// optionalCity reads the ?city= filter. An empty value means "no filter"; an
// unknown one is a 422 with a machine-readable code rather than a silently
// empty list — the guide is a short, hand-made list, and "nothing found"
// because of a typo in a parameter is indistinguishable from "we have nothing
// for your city", which is exactly the wrong thing to show on a home screen.
func optionalCity(c *gin.Context) (*domain.City, bool) {
	raw := strings.TrimSpace(c.Query("city"))
	if raw == "" {
		return nil, true
	}
	city := domain.City(raw)
	if !city.Valid() {
		response.ErrorWithCode(c.Writer, http.StatusUnprocessableEntity,
			domain.CodeCityRequired, "city must be a known city")
		return nil, false
	}
	return &city, true
}

func pagination(c *gin.Context) (page, perPage int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ = strconv.Atoi(c.Query("per_page"))
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}
	return page, perPage
}

// --- DTOs ---

type categoryResponse struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Position int    `json:"position"`
}

func newCategoryResponse(c domain.GuideCategory, lang string) categoryResponse {
	return categoryResponse{
		ID:       c.ID.String(),
		Slug:     c.Slug,
		Title:    c.TitleI18n.Resolve(lang, c.Title),
		Position: c.Position,
	}
}

// collectionResponse is one guide card. Localized fields are already resolved
// for the caller's language; the raw *_i18n maps are not exposed on a public
// read, exactly like the public promo/event responses.
type collectionResponse struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	// Description is the long editorial text, present on both the listing and
	// the detail — the same collection reads the same everywhere.
	Description string `json:"description,omitempty"`
	// CoverImageURL is omitted when the collection has no cover. Never a
	// placeholder URL: the client draws its own.
	CoverImageURL *string `json:"cover_image_url,omitempty"`
	// City is omitted when the collection is not tied to one city.
	City string `json:"city,omitempty"`
	// Position is the editor's explicit order. Exposed so a client that caches
	// pages can re-sort them itself and land on the same sequence.
	Position int `json:"position"`
	// VenueCount is how many venues the guest can actually open right now.
	VenueCount    int      `json:"venue_count"`
	CategorySlugs []string `json:"category_slugs,omitempty"`
}

func newCollectionResponse(c domain.GuideCollection, lang string) collectionResponse {
	out := collectionResponse{
		ID:            c.ID.String(),
		Slug:          c.Slug,
		Title:         c.TitleI18n.Resolve(lang, c.Title),
		Subtitle:      c.SubtitleI18n.Resolve(lang, c.Subtitle),
		Description:   c.DescriptionI18n.Resolve(lang, c.Description),
		CoverImageURL: c.CoverImageURL,
		Position:      c.Position,
		VenueCount:    c.VenueCount,
		CategorySlugs: c.CategorySlugs,
	}
	if c.City != nil {
		out.City = string(*c.City)
	}
	return out
}

type collectionDetailResponse struct {
	collectionResponse
	Venues []venueResponse `json:"venues"`
}

// venueResponse is a venue card inside a collection: enough to render the row
// and open the venue, not a copy of the whole catalog entry.
type venueResponse struct {
	RestaurantID string `json:"restaurant_id"`
	Position     int    `json:"position"`
	// Note is the editor's line about why this venue is in this collection.
	Note            string  `json:"note,omitempty"`
	Name            string  `json:"name"`
	Address         string  `json:"address,omitempty"`
	CuisineType     string  `json:"cuisine_type,omitempty"`
	City            string  `json:"city"`
	PriceCategory   string  `json:"price_category,omitempty"`
	PrimaryImageURL *string `json:"primary_image_url,omitempty"`
	// Instagram — ссылка на инстаграм ЗАВЕДЕНИЯ; отсутствует, если её нет.
	// В макете подпись блока выглядит как «адрес · @инстаграм».
	Instagram string `json:"instagram,omitempty"`
	// Highlight — событие или акция, которыми проиллюстрирован блок. Поля нет,
	// когда блок остаётся простой карточкой заведения.
	Highlight *highlightResponse `json:"highlight,omitempty"`
}

// highlightResponse — событие или акция внутри блока подборки: заголовок, текст
// и лента фотографий, которые в макете стоят выше адреса заведения.
type highlightResponse struct {
	// Kind — "event" или "promo": по нему клиент выбирает, куда вести по тапу.
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	StartsAt    *string `json:"starts_at,omitempty"`
	// CoverImageURL — обложка события/акции; отсутствует, если её нет.
	CoverImageURL *string `json:"cover_image_url,omitempty"`
	// Images — лента фотографий БЕЗ обложки. Всегда массив: клиент рисует
	// ленту, и null пришлось бы защищать отдельно в каждом месте.
	Images []string `json:"images"`
}

func newVenueResponse(v domain.GuideCollectionVenue, lang string) venueResponse {
	return venueResponse{
		RestaurantID:    v.RestaurantID.String(),
		Position:        v.Position,
		Note:            v.NoteI18n.Resolve(lang, v.Note),
		Name:            v.NameI18n.Resolve(lang, v.Name),
		Address:         v.AddressI18n.Resolve(lang, v.Address),
		CuisineType:     v.CuisineTypeI18n.Resolve(lang, v.CuisineType),
		City:            string(v.City),
		PriceCategory:   string(v.PriceCategory),
		PrimaryImageURL: v.PrimaryImageURL,
		Instagram:       v.Instagram,
		Highlight:       newHighlightResponse(v.Highlight, lang),
	}
}

func newHighlightResponse(h *domain.GuideHighlight, lang string) *highlightResponse {
	if h == nil {
		return nil
	}
	out := &highlightResponse{
		Kind:          string(h.Kind),
		ID:            h.ID.String(),
		Title:         h.TitleI18n.Resolve(lang, h.Title),
		Description:   h.DescriptionI18n.Resolve(lang, h.Description),
		CoverImageURL: h.CoverImageURL,
		Images:        h.Images,
	}
	if out.Images == nil {
		out.Images = []string{}
	}
	if !h.StartsAt.IsZero() {
		formatted := h.StartsAt.UTC().Format(time.RFC3339)
		out.StartsAt = &formatted
	}
	return out
}

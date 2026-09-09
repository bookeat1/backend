package restaurants

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
	"backend-core/internal/usecase/homepicks"
)

// PicksHandler serves «Выбрали для вас» — the hand-curated venue rail on the
// main screen (usecase/homepicks).
//
// It lives in this package, next to the catalog handler, for one concrete
// reason: the rail's cards ARE catalog cards. Sharing listItemToResponse and
// attachFavorites means the app can keep one mapper for both endpoints and the
// two payloads cannot drift apart field by field. A separate transport package
// would have had to re-export the whole response shape to get there.
type PicksHandler struct {
	facade    homepicks.Facade
	favorites favoriteChecker
}

// NewPicksHandler builds the rail's handler. favorites may be nil — the flag is
// then simply never attached, exactly as in the catalog handler.
func NewPicksHandler(f homepicks.Facade, favorites favoriteChecker) *PicksHandler {
	return &PicksHandler{facade: f, favorites: favorites}
}

// RegisterPublic mounts the guest read.
//
// The static segment sits alongside GET /restaurants/:id the same way
// /restaurants/search already does — gin matches the literal first.
func (h *PicksHandler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants/picks", h.guestPicks)
}

// RegisterAdminGlobal mounts the editor's read/write pair. Superadmin only, by
// the group it is mounted on: the rail is platform editorial content, and a
// venue owner who could write it would be putting themselves on the main
// screen of the app.
func (h *PicksHandler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/restaurants/picks", h.adminPicks)
	rg.PUT("/admin/restaurants/picks", h.replacePicks)
}

// guestPicks is the rail as the app draws it.
//
// `city` is OPTIONAL and an unknown one is not an error: a city with no rail of
// its own falls through to the all-cities list and then to the automatic rule,
// so the worst a wrong city can do is give the guest the default rail. This
// endpoint must not be able to answer 422 — it is the main screen.
//
// @Summary     «Выбрали для вас» — the main screen's venue rail
// @Tags        restaurants
// @Produce     json
// @Param       city  query string false "City; the all-cities rail is used when it has none of its own"
// @Param       limit query int    false "How many venues (default 8, max 50)"
// @Success     200 {object} response.Page
// @Router      /api/v1/restaurants/picks [get]
func (h *PicksHandler) guestPicks(c *gin.Context) {
	items, err := h.facade.Guest(c.Request.Context(), c.Query("city"), queryLimit(c))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	h.writePage(c, items)
}

// adminPicks is the editor's view: only what was curated for THIS city key,
// deactivated venues included. See homepicks.Facade.Editor.
//
// An empty `city` is a valid key here, not a missing parameter — it is the
// all-cities rail. That is why there is no city_required refusal.
//
// @Summary     The curated «Выбрали для вас» list of one city (superadmin)
// @Tags        restaurants
// @Produce     json
// @Param       city query string false "City; empty means the all-cities rail"
// @Success     200 {object} response.Page
// @Router      /api/v1/admin/restaurants/picks [get]
func (h *PicksHandler) adminPicks(c *gin.Context) {
	items, err := h.facade.Editor(c.Request.Context(), c.Query("city"))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	h.writePage(c, items)
}

// replacePicks sets one city's whole rail in one atomic call — what a
// drag-and-drop editor needs. An empty list clears the curation and the city
// falls back to the automatic rail.
//
// @Summary     Replace the «Выбрали для вас» list of one city (superadmin)
// @Tags        restaurants
// @Accept      json
// @Produce     json
// @Success     200 {object} map[string]string
// @Router      /api/v1/admin/restaurants/picks [put]
func (h *PicksHandler) replacePicks(c *gin.Context) {
	var req homePicksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ids, err := req.toUUIDs()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	if err := h.facade.Replace(c.Request.Context(), req.City, ids); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "ok"})
}

// writePage answers in the SAME envelope as GET /restaurants, so the app reuses
// one page reader and one card mapper. The rail is never paginated — it is one
// short strip — so page/per_page describe the single page it is.
func (h *PicksHandler) writePage(c *gin.Context, items []domain.RestaurantListItem) {
	lang := resolveLocale(c)
	out := make([]restaurantResponse, 0, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it, lang))
		ids = append(ids, it.Restaurant.ID)
	}
	attachFavoritesTo(c.Request.Context(), h.favorites, out, ids)
	perPage := len(out)
	if perPage == 0 {
		perPage = 1
	}
	response.OK(c.Writer, response.NewPage(out, len(out), 1, perPage))
}

// queryLimit reads ?limit=. A garbage or negative value is IGNORED rather than
// refused, for the same reason the catalog ignores a garbage is_popular: this
// endpoint paints the first screen after a cold start and must always have an
// answer. 0 means "use the default" (see homepicks.DefaultLimit).
func queryLimit(c *gin.Context) int {
	n, err := strconv.Atoi(c.Query("limit"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// homePicksRequest is the editor's save: the city key plus the whole ordered
// list. Both fields are required in shape, but both may be empty — an empty
// city is the all-cities rail, and an empty list clears the curation.
type homePicksRequest struct {
	City          string   `json:"city"`
	RestaurantIDs []string `json:"restaurant_ids"`
}

func (r homePicksRequest) toUUIDs() ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(r.RestaurantIDs))
	for _, s := range r.RestaurantIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a restaurant id", domain.ErrValidation, s)
		}
		out = append(out, id)
	}
	return out, nil
}

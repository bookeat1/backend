// Package restaurants exposes the restaurant catalog HTTP endpoints.
package restaurants

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/restaurants"
)

// favoriteChecker is the minimal slice of the favorites usecase this handler
// needs to attach an "is_favorite" flag to a listing/detail response for the
// current caller. A nil favoriteChecker is valid (the flag is then simply
// never attached, same as for an anonymous caller) so this handler never
// hard-depends on the favorites feature being wired.
type favoriteChecker interface {
	FavoriteSet(ctx context.Context, userID uuid.UUID, restaurantIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

type Handler struct {
	facade    uc.Facade
	managers  uc.ManagerUseCase
	favorites favoriteChecker
}

func NewHandler(f uc.Facade, m uc.ManagerUseCase, favorites favoriteChecker) *Handler {
	return &Handler{facade: f, managers: m, favorites: favorites}
}

// RegisterPublic mounts the unauthenticated catalog routes.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/restaurants", h.list)
	rg.GET("/restaurants/search", h.search)
	rg.GET("/restaurants/:id", h.get)
	rg.GET("/restaurant-categories", h.categories)
	rg.POST("/partnership-requests", h.submitPartnership)
}

// RegisterAdminGlobal mounts admin-only routes: the superadmin catalog listing
// and creating a new restaurant.
func (h *Handler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/restaurants", h.adminList)
	rg.POST("/restaurants", h.create)
}

// RegisterRestaurantScoped mounts mutations on an existing restaurant's own
// fields. Mount on a RequireRestaurantManager(..., "id") group (admin or the
// restaurant's own manager).
func (h *Handler) RegisterRestaurantScoped(rg *gin.RouterGroup) {
	rg.PATCH("/restaurants/:id", h.update)
	rg.DELETE("/restaurants/:id", h.deactivate)
}

// RegisterStaffRoutes mounts staff-roster management (list/assign/set
// role/remove). Authorization is NOT done by transport middleware here — it
// is fully resolved inside usecase/restaurants.ManagerUseCase (which role may
// touch which restaurant's roster, per the RBAC matrix), so this only needs
// to run after middleware.Auth (any authenticated caller may reach the
// handler; the usecase itself returns ErrForbidden for anyone who isn't the
// target restaurant's own owner or a superadmin). This deliberately replaces
// the old admin-only gate: removeManager/setRole used to be admin-only
// specifically because deleting/re-roling by a bare manager id had no other
// way to resolve which restaurant it belonged to — ManagerUseCase now
// resolves that itself before authorizing (see its doc comments).
func (h *Handler) RegisterStaffRoutes(rg *gin.RouterGroup) {
	rg.GET("/restaurants/:id/managers", h.listManagers)
	rg.POST("/restaurants/:id/managers", h.assignManager)
	rg.PATCH("/restaurants/:id/managers/:managerID", h.setManagerRole)
	rg.DELETE("/restaurants/:id/managers/:managerID", h.removeManager)
}

// staffActorFrom builds the usecase/restaurants.Actor from the authenticated
// principal, writing 401 when the request never passed middleware.Auth.
func staffActorFrom(c *gin.Context) (uc.Actor, bool) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return uc.Actor{}, false
	}
	return uc.Actor{UserID: au.ID, Role: domain.Role(au.Role)}, true
}

// adminList is the catalog as its OWNER sees it: hidden venues included.
//
// The public listing filters `is_active = true`, which is right for a guest and
// wrong for the person who hid the venue — without this they could hide a venue
// and never find it again. Same filters and same response shape as the public
// list, so the panel reuses one renderer; the only difference is that a hidden
// venue is present and says so through `is_active`.
func (h *Handler) adminList(c *gin.Context) {
	f := domain.RestaurantFilter{Search: c.Query("search"), IncludeInactive: true}
	if v := c.Query("city"); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))

	items, total, err := h.facade.List(c.Request.Context(), f, domain.VenueStateFilter{})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := resolveLocale(c)
	out := make([]restaurantResponse, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it, lang))
	}
	page, perPage := domain.NormalizePaging(f.Page, f.PerPage)
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) list(c *gin.Context) {
	f := domain.RestaurantFilter{Search: c.Query("search")}
	if v := c.Query("city"); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	if v := c.Query("category"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			f.Category = &id
		}
	}
	if v := c.Query("is_popular"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			f.IsPopular = &b
		}
	}
	if v := c.Query("is_new"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			f.IsNew = &b
		}
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))

	items, total, err := h.facade.List(c.Request.Context(), f, venueStateFilter(c))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := resolveLocale(c)
	out := make([]restaurantResponse, 0, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it, lang))
		ids = append(ids, it.Restaurant.ID)
	}
	h.attachFavorites(c.Request.Context(), out, ids)
	page, perPage := domain.NormalizePaging(f.Page, f.PerPage)
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

// venueStateFilter reads the two server-computed catalog filters off the query
// string. Both are optional and both accept the full strconv.ParseBool
// vocabulary, exactly like the existing is_popular / is_new filters — including
// their behaviour on garbage, which is to ignore the parameter rather than fail
// the request.
//
//   - open_now=true|false — evaluated in the VENUE's timezone by the same
//     domain.IsOpenAt call that fills the response's schedule.open_now. A venue
//     with no usable working hours is not open, so it appears only under
//     open_now=false;
//   - accepts_online_bookings=true|false — the same flag the payload carries.
//
// The response's `total` counts everything matching these too, so a client can
// show the guest an honest "забронировать онлайн можно в N из M" without
// counting one page by hand.
func venueStateFilter(c *gin.Context) domain.VenueStateFilter {
	var vs domain.VenueStateFilter
	if v := c.Query("open_now"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			vs.OpenNow = &b
		}
	}
	if v := c.Query("accepts_online_bookings"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			vs.AcceptsOnlineBookings = &b
		}
	}
	vs.Availability = availabilityFilter(c)
	return vs
}

// availabilityFilter reads the "гости + дата" filter: ?date=2026-08-20&guests=2
// plus the optional window ?time_from=19:00&time_to=21:00.
//
// Unlike the boolean filters above, a malformed value here is NOT ignored. They
// are booleans with two possible answers, and dropping one still gives the
// guest a catalog that means something; this one carries the guest's whole
// question. Silently dropping "guests=4" would answer a query about a table for
// four with the entire catalog — so a broken value fails the request instead
// (the usecase validates and returns 400).
//
// Both parts are required together: a date with no party size cannot be
// evaluated (the engine needs to know who it is seating), and a party size with
// no date has no day to look at. One without the other is ignored.
func availabilityFilter(c *gin.Context) *domain.AvailabilitySearch {
	date, guests := strings.TrimSpace(c.Query("date")), strings.TrimSpace(c.Query("guests"))
	if date == "" || guests == "" {
		return nil
	}
	n, err := strconv.Atoi(guests)
	if err != nil {
		n = 0 // the usecase rejects it with a message the client can show
	}
	q := &domain.AvailabilitySearch{Date: date, Guests: n}
	if v := clockMinutes(c.Query("time_from")); v != nil {
		q.FromMinutes = v
	}
	if v := clockMinutes(c.Query("time_to")); v != nil {
		q.ToMinutes = v
	}
	return q
}

// clockMinutes parses "HH:MM" into minutes since midnight, nil on anything
// else. The window is a narrowing convenience, not the guest's core question,
// so a broken one degrades to "the whole day" rather than failing the search.
func clockMinutes(v string) *int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	mins, err := domain.ParseClockMinutes(v)
	if err != nil || mins < 0 || mins > 24*60 {
		return nil
	}
	return &mins
}

// search runs the public full-text + fuzzy catalog search. It is a distinct
// endpoint from list: the list response shape is frozen (other clients depend
// on it), so this route returns the same per-item shape but is free to evolve
// its own query surface (q, cuisine[], price, ranking) independently.
func (h *Handler) search(c *gin.Context) {
	f := domain.RestaurantSearchFilter{Query: c.Query("q")}
	if v := c.Query("city"); v != "" {
		city := domain.City(v)
		f.City = &city
	}
	// cuisine may repeat (?cuisine=Итальянская&cuisine=Азиатская) or be a single
	// comma-separated value; both collapse to an OR-set. Blank entries dropped.
	for _, raw := range c.QueryArray("cuisine") {
		for _, part := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(part); s != "" {
				f.Cuisines = append(f.Cuisines, s)
			}
		}
	}
	if v := c.Query("price"); v != "" {
		price := domain.PriceCategory(v)
		f.Price = &price
	}
	f.Page, _ = strconv.Atoi(c.Query("page"))
	f.PerPage, _ = strconv.Atoi(c.Query("per_page"))

	items, total, err := h.facade.Search(c.Request.Context(), f, venueStateFilter(c))
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	lang := resolveLocale(c)
	out := make([]restaurantResponse, 0, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		out = append(out, listItemToResponse(it, lang))
		ids = append(ids, it.Restaurant.ID)
	}
	h.attachFavorites(c.Request.Context(), out, ids)
	page, perPage := domain.NormalizePaging(f.Page, f.PerPage)
	response.OK(c.Writer, response.NewPage(out, total, page, perPage))
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	agg, err := h.facade.Get(c.Request.Context(), id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// This is the unauthenticated catalog route. A deactivated (soft-deleted)
	// restaurant must not be reachable by direct id, same as it is excluded
	// from the listing. hidden_from_home is intentionally still served so
	// deep links to off-home venues keep working.
	if !agg.IsActive {
		response.HandleError(c.Writer, domain.ErrNotFound)
		return
	}
	lang := resolveLocale(c)
	list := []restaurantResponse{aggregateToResponse(agg, lang)}
	h.attachFavorites(c.Request.Context(), list, []uuid.UUID{agg.Restaurant.ID})
	response.OK(c.Writer, list[0])
}

// attachFavorites sets IsFavorite on each element of out (in place, matched
// by index against ids) for the current authenticated caller. A no-op for an
// anonymous caller, a nil favoriteChecker, or when the lookup itself fails —
// the favorites flag is a secondary enhancement and must never break the
// catalog response it's attached to.
func (h *Handler) attachFavorites(ctx context.Context, out []restaurantResponse, ids []uuid.UUID) {
	if h.favorites == nil || len(out) != len(ids) {
		return
	}
	au, ok := middleware.GetAuthUser(ctx)
	if !ok {
		return
	}
	set, err := h.favorites.FavoriteSet(ctx, au.ID, ids)
	if err != nil {
		slog.Warn("favorite lookup failed, serving catalog without is_favorite", "error", err)
		return
	}
	for i := range out {
		v := set[ids[i]]
		out[i].IsFavorite = &v
	}
}

// GET /cities used to live here, backed by the two constants in
// internal/domain. Since migration 0081 the cities are a dictionary table and
// the route belongs to transport/rest/cities — same path, same default body.

func (h *Handler) categories(c *gin.Context) {
	cats, err := h.facade.Categories(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]categoryResponse, 0, len(cats))
	for _, cat := range cats {
		out = append(out, categoryToResponse(cat))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) submitPartnership(c *gin.Context) {
	var req partnershipRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := h.facade.SubmitPartnership(c.Request.Context(), req.toInput()); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, gin.H{"status": "received"})
}

func (h *Handler) create(c *gin.Context) {
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	agg, err := h.facade.Create(c.Request.Context(), in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, aggregateToResponse(agg, resolveLocale(c)))
}

func (h *Handler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req saveRestaurantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := req.toInput()
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	// This route is reachable by a restaurant's own manager (not just admins).
	// Marketing/curation fields are admin-only: a manager must not be able to
	// self-promote (is_premium/is_popular/is_new/display_order) or reactivate a
	// venue an admin deactivated (is_active). Strip them for non-admin callers;
	// managers deactivate via DELETE, and only an admin can reactivate.
	if au, ok := middleware.GetAuthUser(c.Request.Context()); !ok || au.Role != string(domain.RoleAdmin) {
		in.IsActive = nil
		in.IsNew = nil
		in.IsPopular = nil
		in.IsPremium = nil
		in.DisplayOrder = nil
	}
	agg, err := h.facade.Update(c.Request.Context(), id, in)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, aggregateToResponse(agg, resolveLocale(c)))
}

func (h *Handler) deactivate(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	if err := h.facade.SetActive(c.Request.Context(), id, false); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "deactivated"})
}

func (h *Handler) listManagers(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	ms, err := h.managers.List(c.Request.Context(), actor, id)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := make([]managerResponse, 0, len(ms))
	for _, m := range ms {
		out = append(out, managerToResponse(m))
	}
	response.OK(c.Writer, out)
}

func (h *Handler) assignManager(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	rid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid id")
		return
	}
	var req assignManagerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	uid, err := uuid.Parse(req.UserID)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid user_id")
		return
	}
	role := domain.StaffRole(req.Role)
	if !role.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "role must be one of: owner, manager, hostess")
		return
	}
	createdBy := &actor.UserID
	m, err := h.managers.Assign(c.Request.Context(), actor, uc.AssignManagerInput{
		RestaurantID: rid, UserID: uid, Role: role, CreatedBy: createdBy,
		WhatsappOptIn: req.WhatsappOptIn, WhatsappPhone: req.WhatsappPhone,
	})
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.Created(c.Writer, managerToResponse(*m))
}

func (h *Handler) setManagerRole(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	mid, err := uuid.Parse(c.Param("managerID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid manager id")
		return
	}
	var req setManagerRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, err.Error())
		return
	}
	role := domain.StaffRole(req.Role)
	if !role.Valid() {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "role must be one of: owner, manager, hostess")
		return
	}
	m, err := h.managers.SetRole(c.Request.Context(), actor, mid, role)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, managerToResponse(*m))
}

func (h *Handler) removeManager(c *gin.Context) {
	actor, ok := staffActorFrom(c)
	if !ok {
		return
	}
	mid, err := uuid.Parse(c.Param("managerID"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid manager id")
		return
	}
	if err := h.managers.Remove(c.Request.Context(), actor, mid); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "removed"})
}

package favorites_test

import (
	"testing"

	"github.com/gin-gonic/gin"

	eventsrest "backend-core/internal/transport/rest/events"
	favoritesrest "backend-core/internal/transport/rest/favorites"
	promosrest "backend-core/internal/transport/rest/promos"
)

// gin builds one radix tree per HTTP method and PANICS at registration time
// when two routes cannot coexist. The bookmark writes sit at
// /events/:eventId/favorite and /promos/:promoId/favorite precisely because
// /favorites/:restaurantId already owns the PUT and DELETE slot under
// /favorites — and they now share a tree with the events/promos admin routes.
// Assert the mount here rather than discovering it when the service refuses to
// boot.
func TestFavoritesRoutesMountWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")

	eventsrest.NewHandler(nil).RegisterPublic(api)
	eventsrest.NewHandler(nil).RegisterAdminRoutes(authed)
	promosrest.NewHandler(nil).RegisterPublic(api)
	promosrest.NewHandler(nil).RegisterAdminRoutes(authed)

	h := favoritesrest.NewHandler(nil)
	h.RegisterRoutes(authed)
	h.RegisterContentRoutes(authed)
}

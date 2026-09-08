package feed

import (
	"testing"

	"github.com/gin-gonic/gin"

	contentrest "backend-core/internal/transport/rest/content"
	eventsrest "backend-core/internal/transport/rest/events"
	promosrest "backend-core/internal/transport/rest/promos"
)

// TestRoutesRegisterWithoutConflict mounts the feed routes NEXT TO the
// neighbours that share their /admin prefix, exactly the way bootstrap/app.go
// does. Gin panics at registration time when a static segment and a wildcard
// become siblings in the router tree, and that panic would only surface at
// server start — i.e. in production. Registering them here turns it into a
// failing test instead.
//
// The handlers are built with nil usecases on purpose: registration never calls
// them, and this test is about the route tree, not about behaviour.
func TestRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")

	public := api.Group("")
	authed := api.Group("")
	adminGlobal := authed.Group("")

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("route registration panicked (a router conflict): %v", rec)
		}
	}()

	h := NewHandler(nil)
	h.RegisterPublic(public)
	h.RegisterVenueRoutes(authed)
	h.RegisterPlatformRoutes(adminGlobal)

	// The neighbours under the same /admin prefix.
	promosrest.NewHandler(nil).RegisterAdminRoutes(authed)
	eventsrest.NewHandler(nil).RegisterAdminRoutes(authed)
	contentrest.NewHandler(nil).RegisterStaffRoutes(authed)

	want := map[string]bool{
		"GET /api/v1/feed":                                            false,
		"GET /api/v1/admin/restaurants/:id/feed":                      false,
		"GET /api/v1/admin/feed/items/:kind/:itemId":                  false,
		"POST /api/v1/admin/feed/items/:kind/:itemId/submit":          false,
		"POST /api/v1/admin/feed/items/:kind/:itemId/withdraw":        false,
		"GET /api/v1/admin/feed/queue":                                false,
		"POST /api/v1/admin/feed/items/:kind/:itemId/review":          false,
		"PUT /api/v1/admin/feed/items/:kind/:itemId/placement-weight": false,
	}
	for _, route := range r.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route not registered: %s", key)
		}
	}
}

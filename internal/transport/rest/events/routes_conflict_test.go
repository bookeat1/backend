package events_test

import (
	"testing"

	"github.com/gin-gonic/gin"

	eventsrest "backend-core/internal/transport/rest/events"
	feedrest "backend-core/internal/transport/rest/feed"
)

// gin builds a radix tree per method and PANICS at registration time when two
// routes cannot coexist. The new rule-feed routes sit next to the item-feed
// ones (/admin/feed/event-recurrence-queue beside /admin/feed/queue and
// /admin/feed/items/:kind/:itemId), which is exactly the shape that has bitten
// this codebase before, so the mount itself is asserted here rather than
// discovered when the service refuses to boot.
func TestFeedRoutesMountWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	admin := authed.Group("")
	eventsrest.NewRecurrenceHandler(nil).RegisterAdminRoutes(authed)
	eventsrest.NewHandler(nil).RegisterAdminRoutes(authed)
	fh := feedrest.NewHandler(nil)
	fh.RegisterVenueRoutes(authed)
	fh.RegisterPlatformRoutes(admin)
	eventsrest.NewRecurrenceHandler(nil).RegisterPlatformRoutes(admin)
}

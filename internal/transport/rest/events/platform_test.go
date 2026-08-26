package events

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	feedrest "backend-core/internal/transport/rest/feed"
	promosrest "backend-core/internal/transport/rest/promos"
)

// The wire shape is a contract, and "restaurant is always present" was part of
// it until now. These tests pin the new shape: the venue block is absent — not
// null-filled, not zero-filled — for an event the platform hosts itself.

func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return env.Data
}

func platformListItem(title string) domain.EventListItem {
	start := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	return domain.EventListItem{
		Event: domain.Event{
			ID: uuid.New(), Title: title,
			StartsAt: start, EndsAt: start.Add(2 * time.Hour),
			Status: domain.EventPublished, Tags: []string{},
		},
	}
}

// A platform event in the cross-venue listing carries neither `restaurant` nor
// `restaurant_id`. A client that read `restaurant.name` unconditionally is the
// bug this shape exists to make visible at compile/parse time rather than at
// render time.
func TestListUpcoming_PlatformEventHasNoRestaurantKey(t *testing.T) {
	f := &fakeFacade{items: []domain.EventListItem{platformListItem("Фестиваль платформы")}}
	rec := do(t, newPublicRouter(f), "/api/v1/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(env.Data.Items))
	}
	item := env.Data.Items[0]
	if _, ok := item["restaurant"]; ok {
		t.Fatalf("a platform event must carry no restaurant block, got %v", item["restaurant"])
	}
	if _, ok := item["restaurant_id"]; ok {
		t.Fatalf("a platform event must carry no restaurant_id, got %v", item["restaurant_id"])
	}
	if item["title"] != "Фестиваль платформы" {
		t.Fatalf("title = %v", item["title"])
	}
}

// ...while a venue-bound event keeps the block it always had, in the same shape.
func TestListUpcoming_VenueBoundEventKeepsItsRestaurantKey(t *testing.T) {
	it := sampleItem(24*time.Hour, "Винный ужин", "Bistro", domain.CityAlmaty)
	f := &fakeFacade{items: []domain.EventListItem{it}}
	rec := do(t, newPublicRouter(f), "/api/v1/events")

	var env struct {
		Data struct {
			Items []struct {
				RestaurantID string `json:"restaurant_id"`
				Restaurant   *struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					City string `json:"city"`
				} `json:"restaurant"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(env.Data.Items))
	}
	got := env.Data.Items[0]
	if got.Restaurant == nil || got.Restaurant.Name != "Bistro" || got.Restaurant.City != string(domain.CityAlmaty) {
		t.Fatalf("venue block = %+v, want the hosting venue", got.Restaurant)
	}
	if got.RestaurantID != got.Restaurant.ID {
		t.Fatalf("restaurant_id %q disagrees with restaurant.id %q", got.RestaurantID, got.Restaurant.ID)
	}
}

// The event's own page answers for a platform event — the read a
// restaurant-scoped path cannot address at all.
func TestGetPublicDetail_PlatformEvent(t *testing.T) {
	it := platformListItem("Фестиваль платформы")
	f := &fakeFacade{detail: &it}
	rec := do(t, newPublicRouter(f), "/api/v1/events/"+it.ID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	data := decodeObject(t, rec.Body.Bytes())
	if _, ok := data["restaurant"]; ok {
		t.Fatalf("a platform event must carry no restaurant block, got %v", data["restaurant"])
	}
	if data["id"] != it.ID.String() {
		t.Fatalf("id = %v, want %s", data["id"], it.ID)
	}
}

// The action button is rendered as label + DERIVED target + optional url, so a
// client never has to reconcile two fields that could disagree.
func TestActionButtonShape(t *testing.T) {
	link := "https://tickets.kz/e/42"
	tests := []struct {
		name       string
		action     *domain.EventAction
		wantTarget string
		wantURL    any
	}{
		{"external link", &domain.EventAction{Label: "Купить билет", URL: &link}, "external", link},
		{"the event's own page", &domain.EventAction{Label: "Подробнее"}, "event", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := platformListItem("Концерт")
			it.Action = tt.action
			f := &fakeFacade{detail: &it}
			rec := do(t, newPublicRouter(f), "/api/v1/events/"+it.ID.String())

			data := decodeObject(t, rec.Body.Bytes())
			action, ok := data["action"].(map[string]any)
			if !ok {
				t.Fatalf("action = %v, want an object", data["action"])
			}
			if action["label"] != tt.action.Label {
				t.Fatalf("label = %v, want %q", action["label"], tt.action.Label)
			}
			if action["target"] != tt.wantTarget {
				t.Fatalf("target = %v, want %q", action["target"], tt.wantTarget)
			}
			if tt.wantURL == nil {
				if _, present := action["url"]; present {
					t.Fatalf("url = %v, want it absent for an in-app target", action["url"])
				}
			} else if action["url"] != tt.wantURL {
				t.Fatalf("url = %v, want %v", action["url"], tt.wantURL)
			}
		})
	}

	// No button at all: the key is absent, not null.
	it := platformListItem("Концерт")
	f := &fakeFacade{detail: &it}
	rec := do(t, newPublicRouter(f), "/api/v1/events/"+it.ID.String())
	if _, present := decodeObject(t, rec.Body.Bytes())["action"]; present {
		t.Fatal("an event with no button must carry no action key")
	}
}

// gin panics at registration time when two routes cannot coexist, so the new
// platform routes are mounted here rather than discovered when the service
// refuses to boot. GET /events/:eventId sits next to GET /events, and
// /admin/platform/events next to /admin/events/:eventId — both are the shape
// that has bitten this codebase before.
func TestPlatformRoutesMountWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	authed := api.Group("")
	admin := authed.Group("")

	NewHandler(nil).RegisterPublic(api)
	NewHandler(nil).RegisterAdminRoutes(authed)
	NewRecurrenceHandler(nil).RegisterAdminRoutes(authed)
	promosrest.NewHandler(nil).RegisterPublic(api)
	promosrest.NewHandler(nil).RegisterAdminRoutes(authed)
	fh := feedrest.NewHandler(nil)
	fh.RegisterVenueRoutes(authed)
	fh.RegisterPlatformRoutes(admin)
	NewRecurrenceHandler(nil).RegisterPlatformRoutes(admin)
}

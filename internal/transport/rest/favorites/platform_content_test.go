package favorites

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	promorepo "backend-core/internal/infrastructure/postgres/promo"
)

// The favorites screen is where a nil venue hurts twice: the read JOINed
// restaurants (so a platform item would vanish from a list the guest built by
// hand), and the response mapper dereferenced the venue (so it would panic
// instead). Both halves are asserted end-to-end, through HTTP.

// platformEnvelope reads the same payload as favEnvelope but with the venue
// fields OPTIONAL — which is the point: they are absent for a platform item.
type platformEnvelope struct {
	Data struct {
		Items []struct {
			Kind  string `json:"kind"`
			Event *struct {
				ID             string  `json:"id"`
				RestaurantID   *string `json:"restaurant_id"`
				RestaurantName *string `json:"restaurant_name"`
				City           string  `json:"city"`
				Title          string  `json:"title"`
			} `json:"event"`
			Promo *struct {
				ID             string  `json:"id"`
				RestaurantID   *string `json:"restaurant_id"`
				RestaurantName *string `json:"restaurant_name"`
				City           string  `json:"city"`
				Title          string  `json:"title"`
			} `json:"promo"`
		} `json:"items"`
		Counts struct {
			All    int `json:"all"`
			Events int `json:"events"`
			Promos int `json:"promos"`
		} `json:"counts"`
	} `json:"data"`
}

func (h *contentHarness) seedPlatformEvent(title string, startsAt time.Time, city *domain.City) uuid.UUID {
	h.t.Helper()
	e := &domain.Event{
		ID: uuid.New(), Title: title, City: city,
		StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour),
		Status: domain.EventPublished,
	}
	if err := eventrepo.New(h.pool).Create(context.Background(), e); err != nil {
		h.t.Fatalf("create platform event %s: %v", title, err)
	}
	return e.ID
}

func (h *contentHarness) seedPlatformPromo(title string, startsAt, endsAt time.Time, city *domain.City) uuid.UUID {
	h.t.Helper()
	p := &domain.Promo{
		ID: uuid.New(), Title: title, City: city,
		StartsAt: startsAt, EndsAt: endsAt, Status: domain.PromoPublished,
	}
	if err := promorepo.New(h.pool).Create(context.Background(), p); err != nil {
		h.t.Fatalf("create platform promo %s: %v", title, err)
	}
	return p.ID
}

func (h *contentHarness) readPlatform(query string) platformEnvelope {
	h.t.Helper()
	body := h.expect(http.MethodGet, "/api/v1/favorites/items"+query, http.StatusOK)
	var env platformEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.t.Fatalf("decode favorites: %v (%s)", err, body)
	}
	return env
}

// A saved platform event and promo come back on the favorites screen — with no
// venue fields, and without taking the venue-bound items down with them.
func TestFavorites_PlatformContentSurvivesTheScreen(t *testing.T) {
	h := newContentHarness(t)

	almaty := domain.CityAlmaty
	platformEvent := h.seedPlatformEvent("Фестиваль платформы", h.now.Add(48*time.Hour), &almaty)
	platformPromo := h.seedPlatformPromo("Акция платформы", h.now.Add(-time.Hour), h.now.Add(48*time.Hour), nil)
	venueEvent := h.seedEvent("Ужин у Del Papa", h.now.Add(24*time.Hour), domain.EventPublished)

	h.expect(http.MethodPut, "/api/v1/events/"+platformEvent.String()+"/favorite", http.StatusOK)
	h.expect(http.MethodPut, "/api/v1/promos/"+platformPromo.String()+"/favorite", http.StatusOK)
	h.expect(http.MethodPut, "/api/v1/events/"+venueEvent.String()+"/favorite", http.StatusOK)

	env := h.readPlatform("")
	if env.Data.Counts.All != 3 || env.Data.Counts.Events != 2 || env.Data.Counts.Promos != 1 {
		t.Fatalf("counts = %+v, want 2 events and 1 promo", env.Data.Counts)
	}

	seen := map[string]bool{}
	for _, it := range env.Data.Items {
		switch {
		case it.Event != nil && it.Event.ID == platformEvent.String():
			seen["platform event"] = true
			if it.Event.RestaurantID != nil || it.Event.RestaurantName != nil {
				t.Fatalf("a platform event must carry no venue, got %+v", it.Event)
			}
			if it.Event.City != string(domain.CityAlmaty) {
				t.Fatalf("city = %q, want the event's own override", it.Event.City)
			}
		case it.Promo != nil && it.Promo.ID == platformPromo.String():
			seen["platform promo"] = true
			if it.Promo.RestaurantID != nil || it.Promo.RestaurantName != nil {
				t.Fatalf("a platform promo must carry no venue, got %+v", it.Promo)
			}
			if it.Promo.City != "" {
				t.Fatalf("city = %q, want empty: this promo runs everywhere", it.Promo.City)
			}
		case it.Event != nil && it.Event.ID == venueEvent.String():
			seen["venue event"] = true
			if it.Event.RestaurantID == nil || it.Event.RestaurantName == nil || *it.Event.RestaurantName != "Del Papa" {
				t.Fatalf("a venue-bound event must still carry its venue, got %+v", it.Event)
			}
			if it.Event.City != string(domain.CityAlmaty) {
				t.Fatalf("city = %q, want the venue's city", it.Event.City)
			}
		}
	}
	for _, want := range []string{"platform event", "platform promo", "venue event"} {
		if !seen[want] {
			t.Fatalf("the favorites screen lost the %s", want)
		}
	}
}

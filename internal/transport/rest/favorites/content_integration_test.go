package favorites

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	eventrepo "backend-core/internal/infrastructure/postgres/event"
	recurrencerepo "backend-core/internal/infrastructure/postgres/eventrecurrence"
	favoriterepo "backend-core/internal/infrastructure/postgres/favorite"
	promorepo "backend-core/internal/infrastructure/postgres/promo"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	userrepo "backend-core/internal/infrastructure/postgres/user"
	"backend-core/internal/transport/rest/middleware"
	favoritesuc "backend-core/internal/usecase/favorites"
)

// ---- harness ---------------------------------------------------------------

// favEnvelope is the favorites-screen payload this file asserts against.
type favEnvelope struct {
	Data struct {
		Items []struct {
			Kind        string          `json:"kind"`
			FavoritedAt string          `json:"favorited_at"`
			Restaurant  json.RawMessage `json:"restaurant"`
			Event       *struct {
				ID             string   `json:"id"`
				RestaurantID   string   `json:"restaurant_id"`
				RestaurantName string   `json:"restaurant_name"`
				City           string   `json:"city"`
				Title          string   `json:"title"`
				StartsAt       string   `json:"starts_at"`
				EndsAt         string   `json:"ends_at"`
				CoverImageURL  *string  `json:"cover_image_url"`
				Tags           []string `json:"tags"`
				IsRecurring    bool     `json:"is_recurring"`
				RecurrenceID   *string  `json:"recurrence_id"`
			} `json:"event"`
			Promo *struct {
				ID              string  `json:"id"`
				RestaurantName  string  `json:"restaurant_name"`
				Title           string  `json:"title"`
				StartsAt        string  `json:"starts_at"`
				EndsAt          string  `json:"ends_at"`
				DiscountPercent *int    `json:"discount_percent"`
				CoverImageURL   *string `json:"cover_image_url"`
			} `json:"promo"`
		} `json:"items"`
		Counts struct {
			All         int `json:"all"`
			Restaurants int `json:"restaurants"`
			Events      int `json:"events"`
			Promos      int `json:"promos"`
		} `json:"counts"`
	} `json:"data"`
}

type contentHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router *gin.Engine
	user   uuid.UUID
	rid    uuid.UUID
	now    time.Time
}

// newContentHarness seeds one active Almaty venue and one guest, and mounts the
// favorites routes the way bootstrap does.
func newContentHarness(t *testing.T) *contentHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, "restaurants", "users")

	h := &contentHarness{
		t:    t,
		pool: pool,
		user: seedUser(t, pool),
		rid:  uuid.New(),
		now:  time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	}
	order := 0
	if err := restrepo.New(pool).Create(context.Background(), &domain.Restaurant{
		ID: h.rid, Name: "Del Papa", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true, DisplayOrder: &order,
	}); err != nil {
		t.Fatalf("create restaurant: %v", err)
	}

	// The clock is read through the pointer, so a test can move "now" forward
	// between two reads of the same facade — which is exactly what the
	// recurring-event expectation is about.
	facade := favoritesuc.NewFacade(favoriterepo.New(pool),
		favoritesuc.WithClock(func() time.Time { return h.now }))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	authed := r.Group("/api/v1")
	authed.Use(middleware.Auth(fakeIssuer{}, userrepo.New(pool)))
	handler := NewHandler(facade)
	handler.RegisterRoutes(authed)
	handler.RegisterContentRoutes(authed)
	h.router = r
	return h
}

func (h *contentHarness) do(method, path string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+h.user.String())
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// expect asserts the status code and returns the body, so a failure prints the
// server's own error instead of a bare number.
func (h *contentHarness) expect(method, path string, want int) []byte {
	h.t.Helper()
	w := h.do(method, path)
	if w.Code != want {
		h.t.Fatalf("%s %s = %d, want %d; body %s", method, path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}

func (h *contentHarness) read(query string) favEnvelope {
	h.t.Helper()
	body := h.expect(http.MethodGet, "/api/v1/favorites/items"+query, http.StatusOK)
	var env favEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.t.Fatalf("decode favorites: %v (%s)", err, body)
	}
	return env
}

func (h *contentHarness) seedEvent(title string, startsAt time.Time, status domain.EventStatus) uuid.UUID {
	h.t.Helper()
	cover := "https://cdn.example.com/" + title + ".jpg"
	e := &domain.Event{
		ID: uuid.New(), RestaurantID: h.rid, Title: title,
		StartsAt: startsAt, EndsAt: startsAt.Add(2 * time.Hour),
		Venue: "Терраса", CoverImageURL: &cover, Status: status,
		Tags: []string{"Живая музыка"},
	}
	if err := eventrepo.New(h.pool).Create(context.Background(), e); err != nil {
		h.t.Fatalf("create event %s: %v", title, err)
	}
	return e.ID
}

func (h *contentHarness) seedPromo(title string, startsAt, endsAt time.Time, status domain.PromoStatus) uuid.UUID {
	h.t.Helper()
	cover := "https://cdn.example.com/" + title + ".jpg"
	pct := 30
	p := &domain.Promo{
		ID: uuid.New(), RestaurantID: h.rid, Title: title,
		StartsAt: startsAt, EndsAt: endsAt, Terms: "Только зал",
		CoverImageURL: &cover, DiscountPercent: &pct, Status: status,
	}
	if err := promorepo.New(h.pool).Create(context.Background(), p); err != nil {
		h.t.Fatalf("create promo %s: %v", title, err)
	}
	return p.ID
}

// ---- save / unsave / idempotency / combined read ---------------------------

func TestFavorites_SaveUnsaveAndCombinedRead(t *testing.T) {
	h := newContentHarness(t)
	eventID := h.seedEvent("Ужин с виноделом", h.now.Add(24*time.Hour), domain.EventPublished)
	promoID := h.seedPromo("Счастливые часы", h.now.Add(-24*time.Hour), h.now.Add(240*time.Hour), domain.PromoPublished)

	// Saving twice is not an error — for every kind.
	for i := 0; i < 2; i++ {
		h.expect(http.MethodPut, "/api/v1/events/"+eventID.String()+"/favorite", http.StatusOK)
		h.expect(http.MethodPut, "/api/v1/promos/"+promoID.String()+"/favorite", http.StatusOK)
		h.expect(http.MethodPut, "/api/v1/favorites/"+h.rid.String(), http.StatusOK)
	}

	env := h.read("")
	if env.Data.Counts.All != 3 || env.Data.Counts.Restaurants != 1 ||
		env.Data.Counts.Events != 1 || env.Data.Counts.Promos != 1 {
		t.Fatalf("counts = %+v, want 1 of each (3 total)", env.Data.Counts)
	}
	if len(env.Data.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(env.Data.Items))
	}

	// The card must be renderable WITHOUT a follow-up request: assert the
	// fields positively, one by one, rather than "it is not empty".
	var ev, promo, venue int
	for _, it := range env.Data.Items {
		switch it.Kind {
		case "event":
			ev++
			if it.Event == nil {
				t.Fatal("event item carries no event payload")
			}
			if it.Event.ID != eventID.String() {
				t.Errorf("event id = %s, want %s", it.Event.ID, eventID)
			}
			if it.Event.Title != "Ужин с виноделом" {
				t.Errorf("event title = %q", it.Event.Title)
			}
			if it.Event.RestaurantName != "Del Papa" {
				t.Errorf("event restaurant_name = %q, want Del Papa", it.Event.RestaurantName)
			}
			if it.Event.City != string(domain.CityAlmaty) {
				t.Errorf("event city = %q, want %q", it.Event.City, domain.CityAlmaty)
			}
			if it.Event.CoverImageURL == nil || *it.Event.CoverImageURL == "" {
				t.Error("event cover_image_url missing")
			}
			if want := h.now.Add(24 * time.Hour).Format(time.RFC3339); it.Event.StartsAt != want {
				t.Errorf("event starts_at = %s, want %s", it.Event.StartsAt, want)
			}
			if it.Event.IsRecurring {
				t.Error("a one-off event must not read as recurring")
			}
			if it.FavoritedAt == "" {
				t.Error("favorited_at missing on the event item")
			}
		case "promo":
			promo++
			if it.Promo == nil {
				t.Fatal("promo item carries no promo payload")
			}
			if it.Promo.ID != promoID.String() {
				t.Errorf("promo id = %s, want %s", it.Promo.ID, promoID)
			}
			if it.Promo.RestaurantName != "Del Papa" {
				t.Errorf("promo restaurant_name = %q", it.Promo.RestaurantName)
			}
			if want := h.now.Add(240 * time.Hour).Format(time.RFC3339); it.Promo.EndsAt != want {
				t.Errorf("promo ends_at = %s, want %s", it.Promo.EndsAt, want)
			}
			if it.Promo.DiscountPercent == nil || *it.Promo.DiscountPercent != 30 {
				t.Errorf("promo discount_percent = %v, want 30", it.Promo.DiscountPercent)
			}
		case "restaurant":
			venue++
			var payload struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(it.Restaurant, &payload); err != nil {
				t.Fatalf("decode restaurant payload: %v", err)
			}
			if payload.ID != h.rid.String() || payload.Name != "Del Papa" {
				t.Errorf("restaurant payload = %+v, want the seeded venue", payload)
			}
		default:
			t.Fatalf("unknown kind %q", it.Kind)
		}
	}
	if ev != 1 || promo != 1 || venue != 1 {
		t.Fatalf("kinds seen: event=%d promo=%d restaurant=%d, want 1 each", ev, promo, venue)
	}

	// ?type= narrows the items but never the counts — the tab bar draws itself
	// from the same response.
	for _, tc := range []struct{ kind string }{{"event"}, {"promo"}, {"restaurant"}} {
		got := h.read("?type=" + tc.kind)
		if len(got.Data.Items) != 1 || got.Data.Items[0].Kind != tc.kind {
			t.Fatalf("?type=%s returned %d items %+v", tc.kind, len(got.Data.Items), got.Data.Items)
		}
		if got.Data.Counts.All != 3 {
			t.Errorf("?type=%s counts.all = %d, want 3", tc.kind, got.Data.Counts.All)
		}
	}
	h.expect(http.MethodGet, "/api/v1/favorites/items?type=nonsense", http.StatusUnprocessableEntity)

	// Removing twice is not an error either, and the second call must not
	// resurrect or duplicate anything.
	for i := 0; i < 2; i++ {
		h.expect(http.MethodDelete, "/api/v1/events/"+eventID.String()+"/favorite", http.StatusOK)
		h.expect(http.MethodDelete, "/api/v1/promos/"+promoID.String()+"/favorite", http.StatusOK)
	}
	after := h.read("")
	if after.Data.Counts.Events != 0 || after.Data.Counts.Promos != 0 || after.Data.Counts.Restaurants != 1 {
		t.Fatalf("after unsave counts = %+v, want only the venue left", after.Data.Counts)
	}

	// Unknown ids: saving something that does not exist is a 404, removing it is
	// still a silent 200 (there is nothing to remove).
	missing := uuid.New().String()
	h.expect(http.MethodPut, "/api/v1/events/"+missing+"/favorite", http.StatusNotFound)
	h.expect(http.MethodPut, "/api/v1/promos/"+missing+"/favorite", http.StatusNotFound)
	h.expect(http.MethodDelete, "/api/v1/events/"+missing+"/favorite", http.StatusOK)
	h.expect(http.MethodDelete, "/api/v1/promos/"+missing+"/favorite", http.StatusOK)
	h.expect(http.MethodPut, "/api/v1/events/not-a-uuid/favorite", http.StatusUnprocessableEntity)
}

// The original venue endpoint must keep answering with a bare array of catalog
// items — the new read is additive, and an app that only knows GET /favorites
// must not notice this change at all.
func TestFavorites_LegacyVenueEndpointUnchanged(t *testing.T) {
	h := newContentHarness(t)
	h.expect(http.MethodPut, "/api/v1/favorites/"+h.rid.String(), http.StatusOK)

	body := h.expect(http.MethodGet, "/api/v1/favorites", http.StatusOK)
	var env struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			City string `json:"city"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode /favorites: %v (%s)", err, body)
	}
	if len(env.Data) != 1 {
		t.Fatalf("/favorites returned %d items, want 1 (%s)", len(env.Data), body)
	}
	if env.Data[0].ID != h.rid.String() || env.Data[0].Name != "Del Papa" {
		t.Fatalf("/favorites item = %+v, want the seeded venue", env.Data[0])
	}
	if env.Data[0].City != string(domain.CityAlmaty) {
		t.Fatalf("/favorites city = %q, want %q", env.Data[0].City, domain.CityAlmaty)
	}
}

// ---- stale items -----------------------------------------------------------

// An item can be withdrawn, expire or be deleted while it sits in someone's
// favorites. None of that may break the screen, and none of it may produce a
// card that opens onto a 404 — so the invisible ones are absent while the live
// one is still there.
func TestFavorites_StaleItemsDropOutWithoutBreakingTheList(t *testing.T) {
	h := newContentHarness(t)
	live := h.seedEvent("Живой концерт", h.now.Add(48*time.Hour), domain.EventPublished)
	withdrawn := h.seedEvent("Снятый вечер", h.now.Add(48*time.Hour), domain.EventPublished)
	past := h.seedEvent("Прошедший ужин", h.now.Add(-48*time.Hour), domain.EventPublished)
	deleted := h.seedEvent("Удалённый ужин", h.now.Add(48*time.Hour), domain.EventPublished)

	livePromo := h.seedPromo("Действующая акция", h.now.Add(-time.Hour), h.now.Add(72*time.Hour), domain.PromoPublished)
	expiredPromo := h.seedPromo("Истёкшая акция", h.now.Add(-72*time.Hour), h.now.Add(-time.Hour), domain.PromoPublished)

	for _, id := range []uuid.UUID{live, withdrawn, past, deleted} {
		h.expect(http.MethodPut, "/api/v1/events/"+id.String()+"/favorite", http.StatusOK)
	}
	for _, id := range []uuid.UUID{livePromo, expiredPromo} {
		h.expect(http.MethodPut, "/api/v1/promos/"+id.String()+"/favorite", http.StatusOK)
	}

	ctx := context.Background()
	events := eventrepo.New(h.pool)
	hidden, err := events.GetByID(ctx, withdrawn)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	hidden.Status = domain.EventHidden
	if err := events.Update(ctx, hidden); err != nil {
		t.Fatalf("hide event: %v", err)
	}
	if err := events.Delete(ctx, deleted); err != nil {
		t.Fatalf("delete event: %v", err)
	}

	env := h.read("")
	if env.Data.Counts.Events != 1 || env.Data.Counts.Promos != 1 {
		t.Fatalf("counts = %+v, want exactly the live event and the live promo", env.Data.Counts)
	}
	for _, it := range env.Data.Items {
		if it.Event != nil && it.Event.ID != live.String() {
			t.Errorf("unexpected event in the list: %s (%s)", it.Event.ID, it.Event.Title)
		}
		if it.Promo != nil && it.Promo.ID != livePromo.String() {
			t.Errorf("unexpected promo in the list: %s (%s)", it.Promo.ID, it.Promo.Title)
		}
	}

	// The bookmark itself survived a withdrawal: re-publishing brings the item
	// back with the heart still on, which a delete-on-unpublish would have lost.
	hidden.Status = domain.EventPublished
	if err := events.Update(ctx, hidden); err != nil {
		t.Fatalf("re-publish event: %v", err)
	}
	back := h.read("?type=event")
	if back.Data.Counts.Events != 2 {
		t.Fatalf("after re-publishing, events count = %d, want 2 (%+v)", back.Data.Counts.Events, back.Data.Counts)
	}

	// A deactivated venue takes its content off the screen the same way the
	// public catalog does.
	if _, err := h.pool.Exec(ctx, `UPDATE restaurants SET is_active = false WHERE id = $1`, h.rid); err != nil {
		t.Fatalf("deactivate venue: %v", err)
	}
	empty := h.read("")
	if empty.Data.Counts.All != 0 {
		t.Fatalf("counts at a deactivated venue = %+v, want everything hidden", empty.Data.Counts)
	}
}

// ---- recurring events ------------------------------------------------------

// THE design decision: favoriting one Wednesday of a weekly event saves the
// SERIES, not that date. The guest catalog already collapses a series into one
// card (its nearest upcoming occurrence), so "this Wednesday" is not a thing the
// guest can even see separately — and a per-occurrence bookmark would silently
// empty itself the moment the saved date passed.
//
// This test fails under the other choice: after the saved occurrence is over,
// a per-occurrence favorite returns an empty list here.
func TestFavorites_RecurringEventFollowsTheSeries(t *testing.T) {
	h := newContentHarness(t)
	ctx := context.Background()

	rule := &domain.EventRecurrence{
		ID: uuid.New(), RestaurantID: h.rid,
		Title:            "Cocktail Wednesday",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceWeekly,
		Weekdays:         []domain.ISOWeekday{3},
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         domain.CalendarDate{Year: 2026, Month: time.August, Day: 1},
		IsActive:         true,
	}
	recurrences := recurrencerepo.New(h.pool)
	if err := recurrences.Create(ctx, rule); err != nil {
		t.Fatalf("create recurrence: %v", err)
	}
	first := h.now.Add(24 * time.Hour)   // the occurrence the guest taps
	second := h.now.Add(192 * time.Hour) // a week later
	if n, err := recurrences.InsertOccurrences(ctx, rule, []time.Time{first, second}); err != nil || n != 2 {
		t.Fatalf("insert occurrences = %d, %v; want 2, nil", n, err)
	}
	occurrences := map[time.Time]uuid.UUID{}
	rows, err := h.pool.Query(ctx, `SELECT id, starts_at FROM events WHERE recurrence_id = $1`, rule.ID)
	if err != nil {
		t.Fatalf("read occurrences: %v", err)
	}
	for rows.Next() {
		var id uuid.UUID
		var startsAt time.Time
		if err := rows.Scan(&id, &startsAt); err != nil {
			t.Fatalf("scan occurrence: %v", err)
		}
		occurrences[startsAt.UTC()] = id
	}
	rows.Close()
	firstID, secondID := occurrences[first.UTC()], occurrences[second.UTC()]
	if firstID == uuid.Nil || secondID == uuid.Nil {
		t.Fatalf("expected both occurrences, got %+v", occurrences)
	}

	h.expect(http.MethodPut, "/api/v1/events/"+firstID.String()+"/favorite", http.StatusOK)

	// Before the saved date: the card is that date, flagged as a series.
	env := h.read("?type=event")
	if env.Data.Counts.Events != 1 || len(env.Data.Items) != 1 {
		t.Fatalf("counts = %+v, want one event", env.Data.Counts)
	}
	item := env.Data.Items[0].Event
	if item.ID != firstID.String() {
		t.Fatalf("event id = %s, want the nearest occurrence %s", item.ID, firstID)
	}
	if !item.IsRecurring || item.RecurrenceID == nil || *item.RecurrenceID != rule.ID.String() {
		t.Fatalf("item must be flagged as the series %s, got is_recurring=%v recurrence_id=%v",
			rule.ID, item.IsRecurring, item.RecurrenceID)
	}

	// Saving a SECOND date of the same series is the same bookmark, not a
	// second one.
	h.expect(http.MethodPut, "/api/v1/events/"+secondID.String()+"/favorite", http.StatusOK)
	if again := h.read("?type=event"); again.Data.Counts.Events != 1 {
		t.Fatalf("saving a second occurrence produced %d items, want 1", again.Data.Counts.Events)
	}

	// The saved date passes. The bookmark must roll forward to the next
	// occurrence — under a per-occurrence bookmark this list would be empty.
	h.now = first.Add(3 * time.Hour)
	rolled := h.read("?type=event")
	if rolled.Data.Counts.Events != 1 {
		t.Fatalf("after the saved date passed, events = %d, want 1 (the next occurrence)",
			rolled.Data.Counts.Events)
	}
	if got := rolled.Data.Items[0].Event.ID; got != secondID.String() {
		t.Fatalf("event id = %s, want the NEXT occurrence %s", got, secondID)
	}
	if want := second.Format(time.RFC3339); rolled.Data.Items[0].Event.StartsAt != want {
		t.Fatalf("starts_at = %s, want %s", rolled.Data.Items[0].Event.StartsAt, want)
	}

	// Un-saving works from whichever occurrence the guest currently sees, not
	// only from the one they originally tapped.
	h.expect(http.MethodDelete, "/api/v1/events/"+secondID.String()+"/favorite", http.StatusOK)
	if gone := h.read("?type=event"); gone.Data.Counts.Events != 0 {
		t.Fatalf("after unsaving the series, events = %d, want 0", gone.Data.Counts.Events)
	}

	// And once the whole series is over there is nothing left to show — the
	// bookmark never resurrects a past date.
	h.expect(http.MethodPut, "/api/v1/events/"+secondID.String()+"/favorite", http.StatusOK)
	h.now = second.Add(24 * time.Hour)
	if over := h.read("?type=event"); over.Data.Counts.Events != 0 {
		t.Fatalf("after the whole series is over, events = %d, want 0", over.Data.Counts.Events)
	}
}

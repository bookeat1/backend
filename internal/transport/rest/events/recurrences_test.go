package events

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestParseWallClock(t *testing.T) {
	ok := map[string]int{
		"00:00":  0,
		"09:05":  545,
		"19:00":  1140,
		"23:59":  1439,
		" 19:30": 1170, // stray whitespace from a form field is tolerated
	}
	for in, want := range ok {
		got, err := parseWallClock(in)
		if err != nil {
			t.Fatalf("parseWallClock(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("parseWallClock(%q) = %d, want %d", in, got, want)
		}
	}
	// Anything ambiguous is refused rather than guessed: "19" and "7pm" would
	// each be a different event for somebody.
	for _, in := range []string{"", "19", "7pm", "24:00", "19:60", "-1:00", "19:00:00", "aa:bb"} {
		if _, err := parseWallClock(in); err == nil {
			t.Fatalf("parseWallClock(%q) must be refused", in)
		}
	}
}

// The response must be readable by a cabinet that knows nothing about minutes
// since midnight: wall clock as HH:MM, dates as YYYY-MM-DD, weekdays always an
// array.
func TestRecurrenceResponseShape(t *testing.T) {
	until := domain.CalendarDate{Year: 2026, Month: time.December, Day: 31}
	rec := domain.EventRecurrence{
		ID:               uuid.New(),
		RestaurantID:     uuid.New(),
		Title:            "Cocktail Wednesday",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceWeekly,
		Weekdays:         []domain.ISOWeekday{3, 4},
		StartMinutes:     19*60 + 5,
		DurationMinutes:  180,
		StartsOn:         domain.CalendarDate{Year: 2026, Month: time.August, Day: 17},
		UntilDate:        &until,
		IsActive:         true,
	}
	body := recurrenceResponse(rec)
	if body.StartTime != "19:05" {
		t.Fatalf("start_time = %q, want 19:05", body.StartTime)
	}
	if body.StartsOn != "2026-08-17" || body.UntilDate == nil || *body.UntilDate != "2026-12-31" {
		t.Fatalf("dates wrong: %s / %v", body.StartsOn, body.UntilDate)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `"weekdays":[3,4]`) {
		t.Fatalf("weekdays must serialize as an array of ISO numbers: %s", s)
	}
	if strings.Contains(s, `"tags":null`) {
		t.Fatalf("tags must never serialize as null: %s", s)
	}
	if strings.Contains(s, `"timezone"`) {
		t.Fatalf("an absent zone override must be omitted, not sent as empty: %s", s)
	}
}

// Every rule route is behind authentication. Without an AuthUser on the context
// the handler answers 401 and never reaches the usecase — the usecase is where
// the PermRestaurantManage gate lives, and it must never be handed a caller
// this layer could not identify.
func TestRecurrenceAdminRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// A nil facade is deliberate: reaching it at all would be the bug.
	NewRecurrenceHandler(nil).RegisterAdminRoutes(r.Group("/api/v1"))

	rid := uuid.New().String()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/admin/restaurants/" + rid + "/event-recurrences"},
		{http.MethodGet, "/api/v1/admin/restaurants/" + rid + "/event-recurrences"},
		{http.MethodGet, "/api/v1/admin/event-recurrences/" + rid},
		{http.MethodPut, "/api/v1/admin/event-recurrences/" + rid},
		{http.MethodPost, "/api/v1/admin/event-recurrences/" + rid + "/deactivate"},
		{http.MethodPost, "/api/v1/admin/event-recurrences/" + rid + "/activate"},
		{http.MethodPost, "/api/v1/admin/event-recurrences/" + rid + "/feed/submit"},
		{http.MethodPost, "/api/v1/admin/event-recurrences/" + rid + "/feed/withdraw"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// The platform routes must not be reachable without authentication either — and
// they are mounted on the RequireRole(RoleAdmin) group in bootstrap, so the
// venue-side group must NOT expose them. A rule's approval is a platform
// decision; a route that answered on the venue group would be the whole gate
// gone.
func TestRecurrenceFeedReviewIsNotOnTheVenueGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	venue := gin.New()
	NewRecurrenceHandler(nil).RegisterAdminRoutes(venue.Group("/api/v1"))

	rid := uuid.New().String()
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/admin/event-recurrences/" + rid + "/feed/review"},
		{http.MethodGet, "/api/v1/admin/feed/event-recurrence-queue"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		venue.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s on the venue group = %d, want 404 (it belongs to the superadmin group)", tc.method, tc.path, rec.Code)
		}
	}

	platform := gin.New()
	NewRecurrenceHandler(nil).RegisterPlatformRoutes(platform.Group("/api/v1"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/feed/event-recurrence-queue", nil)
	platform.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("platform queue without auth = %d, want 401", rec.Code)
	}
}

package venuedashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/venuedashboard"
)

type fakeTodayRepo struct {
	out                         domain.VenueToday
	err                         error
	gotVenue                    uuid.UUID
	gotAwaitingLim, gotTodayLim int
	calls                       int
}

func (f *fakeTodayRepo) Today(_ context.Context, rid uuid.UUID, _ time.Time,
	awaitingLimit, todayLimit int) (domain.VenueToday, error) {
	f.gotVenue = rid
	f.gotAwaitingLim, f.gotTodayLim = awaitingLimit, todayLimit
	f.calls++
	return f.out, f.err
}

func todayRouter(repo *fakeTodayRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// The real mount is behind RequireRestaurantManager(..., "id"); this router
	// exercises the handler only, so the authorisation gate is not under test
	// here — it is the same middleware every other venue screen uses.
	NewHandler(nil, uc.NewTodayUseCase(repo)).RegisterScoped(r.Group("/api/v1"))
	return r
}

func get(t *testing.T, r *gin.Engine, url string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return w, body
}

// The screen's contract: two lists, their untruncated totals, and a head count
// that covers the whole day rather than the rows that fit.
func TestTodayEndpointShape(t *testing.T) {
	venue := uuid.New()
	created := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	repo := &fakeTodayRepo{out: domain.VenueToday{
		Awaiting: []domain.VenueTodayBooking{{
			ID: uuid.New(), StartsAt: created.Add(6 * time.Hour), Name: "Алия",
			Phone: "+77010000001", Guests: 2, Status: domain.BookingPending,
			CreatedAt: created, WaitingMinutes: 42,
		}},
		AwaitingTotal: 7,
		Today: []domain.VenueTodayBooking{{
			ID: uuid.New(), StartsAt: created.Add(3 * time.Hour), Name: "Берик",
			Phone: "+77010000002", Guests: 4, Status: domain.BookingConfirmed,
			CreatedAt: created, WaitingMinutes: 42,
		}},
		TodayTotal: 12,
		Guests:     31,
	}}

	w, body := get(t, todayRouter(repo), "/api/v1/restaurants/"+venue.String()+"/dashboard/today")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if repo.gotVenue != venue {
		t.Fatalf("venue = %s, want %s", repo.gotVenue, venue)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data envelope: %s", w.Body.String())
	}
	if data["awaiting_total"] != float64(7) || data["today_total"] != float64(12) {
		t.Fatalf("totals must survive truncation: %v", data)
	}
	if data["guests"] != float64(31) {
		t.Fatalf("guests = %v, want 31 (the whole day, not the listed rows)", data["guests"])
	}
	row := data["awaiting"].([]any)[0].(map[string]any)
	if row["waiting_minutes"] != float64(42) {
		t.Fatalf("waiting_minutes = %v, want 42", row["waiting_minutes"])
	}
	if row["phone"] != "+77010000001" || row["name"] != "Алия" || row["guests"] != float64(2) {
		t.Fatalf("the row must carry who to call back: %v", row)
	}
	if row["status"] != "pending" {
		t.Fatalf("status = %v, want pending", row["status"])
	}
	if _, err := time.Parse(time.RFC3339, row["starts_at"].(string)); err != nil {
		t.Fatalf("starts_at is not RFC3339: %v", row["starts_at"])
	}
}

// Absent limits are the usecase's defaults; a present one is honoured.
func TestTodayEndpointPassesLimitsThrough(t *testing.T) {
	venue := uuid.New()

	repo := &fakeTodayRepo{}
	get(t, todayRouter(repo), "/api/v1/restaurants/"+venue.String()+"/dashboard/today")
	if repo.gotAwaitingLim != 20 || repo.gotTodayLim != 50 {
		t.Fatalf("defaults = %d/%d, want 20/50", repo.gotAwaitingLim, repo.gotTodayLim)
	}

	repo = &fakeTodayRepo{}
	get(t, todayRouter(repo), "/api/v1/restaurants/"+venue.String()+"/dashboard/today?awaiting_limit=5&today_limit=9")
	if repo.gotAwaitingLim != 5 || repo.gotTodayLim != 9 {
		t.Fatalf("limits = %d/%d, want 5/9", repo.gotAwaitingLim, repo.gotTodayLim)
	}
}

// A limit that is not a positive number is a caller mistake. Read as "use the
// default" it would hide a broken client; read as-is it would reach SQL.
func TestTodayEndpointRefusesGarbageLimits(t *testing.T) {
	venue := uuid.New()
	for _, q := range []string{"awaiting_limit=abc", "today_limit=0", "today_limit=-3", "awaiting_limit="} {
		repo := &fakeTodayRepo{}
		w, _ := get(t, todayRouter(repo), "/api/v1/restaurants/"+venue.String()+"/dashboard/today?"+q)
		if q == "awaiting_limit=" {
			// An empty value is "not given", not "given wrong".
			if w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", q, w.Code)
			}
			continue
		}
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want 422", q, w.Code)
		}
		if repo.calls != 0 {
			t.Fatalf("%s: an invalid limit must never reach the read model", q)
		}
	}
}

func TestTodayEndpointRefusesABrokenVenueID(t *testing.T) {
	repo := &fakeTodayRepo{}
	w, _ := get(t, todayRouter(repo), "/api/v1/restaurants/not-a-uuid/dashboard/today")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if repo.calls != 0 {
		t.Fatal("a broken id must never reach the read model")
	}
}

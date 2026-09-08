package notifications

import (
	"context"
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

// migrationRepo is the read half of the settings repository; everything else is
// unused by this handler and panics if it is ever called, which is the point.
type migrationRepo struct {
	domain.RestaurantNotificationSettingsRepository
	rows []domain.TelegramMigrationRow
	err  error
}

func (r *migrationRepo) TelegramMigrationStatus(context.Context) ([]domain.TelegramMigrationRow, error) {
	return r.rows, r.err
}

// The report has to answer exactly one operational question: may the old bot be
// switched off yet? That is `pending == 0`, and the venues that will never get
// there on their own (@username channels) have to be visible as such.
func TestTelegramMigrationReport_CountsAndFlags(t *testing.T) {
	ready := time.Now().Add(-time.Hour)
	failed := time.Now().Add(-2 * time.Hour)
	migrated, pending, channel := uuid.New(), uuid.New(), uuid.New()

	repo := &migrationRepo{rows: []domain.TelegramMigrationRow{
		{RestaurantID: channel, RestaurantName: "Канал", ChatID: "@venue_channel", Enabled: true},
		{RestaurantID: pending, RestaurantName: "Отставший", ChatID: "-100222", Enabled: true, NewBotFailedAt: &failed},
		{RestaurantID: migrated, RestaurantName: "Переехал", ChatID: "-100111", Enabled: true, NewBotReadyAt: &ready},
	}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewTelegramMigrationHandler(repo).RegisterAdminGlobal(r.Group("/api/v1"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/notifications/telegram-migration", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var env struct {
		Data telegramMigrationReport `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	got := env.Data
	if got.Total != 3 || got.Ready != 1 || got.Pending != 2 || got.ManualWork != 1 {
		t.Fatalf("counters = %+v, want total 3 / ready 1 / pending 2 / manual 1", got)
	}
	if len(got.PendingList) != 2 || len(got.ReadyList) != 1 {
		t.Fatalf("lists = pending %d / ready %d, want 2 / 1", len(got.PendingList), len(got.ReadyList))
	}
	for _, v := range got.PendingList {
		if v.RestaurantID == channel && !v.NeedsManualWork {
			t.Fatal("an @username target must be flagged: it has no Start to press")
		}
		if v.RestaurantID == pending && v.NeedsManualWork {
			t.Fatal("a numeric chat id needs no manual work — staff just press Start")
		}
		if v.NewBotReady {
			t.Fatalf("a pending venue is reported as ready: %+v", v)
		}
	}
	if got.ReadyList[0].RestaurantID != migrated {
		t.Fatalf("ready list = %+v, want the migrated venue", got.ReadyList)
	}
}

// Nothing connected yet must serialize as empty arrays, not nulls: the panel
// renders a list, and `null` there reads as "failed to load".
func TestTelegramMigrationReport_EmptyIsArraysNotNull(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewTelegramMigrationHandler(&migrationRepo{}).RegisterAdminGlobal(r.Group("/api/v1"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/notifications/telegram-migration", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if want := `"pending_list":[]`; !strings.Contains(body, want) {
		t.Fatalf("body = %s, want it to contain %s", body, want)
	}
	if want := `"ready_list":[]`; !strings.Contains(body, want) {
		t.Fatalf("body = %s, want it to contain %s", body, want)
	}
}

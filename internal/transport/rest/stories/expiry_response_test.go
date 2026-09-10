package stories

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The cabinet's list must SHOW an expired story rather than hide it, and label
// it as expired. Both halves matter: hiding it is the failure mode this feature
// exists to avoid (the venue would lose the card it wants to extend), and an
// unlabelled expired card is indistinguishable from a live one.
//
// is_expired is computed server-side, so the badge does not depend on how well
// the operator's laptop clock is set.
func TestAdminListMarksExpiredStories(t *testing.T) {
	rid := uuid.New()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)

	expired := domain.Story{
		ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/stale.jpg",
		SortOrder: 0, IsActive: true, ExpiresAt: &past, CreatedAt: time.Now(),
	}
	live := domain.Story{
		ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/fresh.jpg",
		SortOrder: 1, IsActive: true, ExpiresAt: &future, CreatedAt: time.Now(),
	}
	permanent := domain.Story{
		ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/forever.jpg",
		SortOrder: 2, IsActive: true, CreatedAt: time.Now(),
	}

	f := &fakeFacade{rv: []domain.Story{expired, live, permanent}}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()

	w := doAdmin(r, http.MethodGet, "/api/v1/admin/restaurants/"+rid.String()+"/stories", nil, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 3 {
		t.Fatalf("cabinet list len = %d, want 3 (the expired card must not be dropped)", len(env.Data))
	}

	if env.Data[0]["is_expired"] != true {
		t.Errorf("the expired card must be marked: %v", env.Data[0])
	}
	if _, ok := env.Data[0]["expires_at"]; !ok {
		t.Errorf("expires_at must be present so the form can pre-fill it: %v", env.Data[0])
	}
	if env.Data[1]["is_expired"] != false {
		t.Errorf("a card expiring tomorrow is not expired: %v", env.Data[1])
	}
	// A permanent card carries NO expires_at at all — an absent field, never an
	// empty string or a zero timestamp a panel might render as "expired in 1970".
	if _, ok := env.Data[2]["expires_at"]; ok {
		t.Errorf("a story with no expiry must omit expires_at, got %v", env.Data[2]["expires_at"])
	}
	if env.Data[2]["is_expired"] != false {
		t.Errorf("a story with no expiry is never expired: %v", env.Data[2])
	}
}

// The create/update body carries expires_at straight through to the usecase,
// which is where it is parsed and validated. The handler's job is only not to
// drop the field — the bug this pins is the one where a form sends a deadline
// and the API cheerfully stores a permanent story.
func TestCreateAndUpdateForwardExpiresAt(t *testing.T) {
	story := sampleStory()
	f := &fakeFacade{story: story}
	r := adminRouter(f, domain.RoleRestaurant)
	token := uuid.New()
	iso := "2026-08-28T12:00:00Z"

	w := doAdmin(r, http.MethodPost, "/api/v1/admin/restaurants/"+story.RestaurantID.String()+"/stories",
		map[string]any{"image_url": story.ImageURL, "expires_at": iso}, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if f.created == nil || f.created.ExpiresAt == nil || *f.created.ExpiresAt != iso {
		t.Fatalf("create did not forward expires_at: %+v", f.created)
	}

	// An empty string is the "make it permanent again" signal and must reach the
	// usecase as an empty string, NOT be collapsed to "field absent" on the way.
	w = doAdmin(r, http.MethodPut, "/api/v1/admin/stories/"+story.ID.String(),
		map[string]any{"expires_at": ""}, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if f.updated == nil || f.updated.ExpiresAt == nil || *f.updated.ExpiresAt != "" {
		t.Fatalf("update did not forward the clearing empty string: %+v", f.updated)
	}

	// And an edit that does not mention expires_at leaves it nil — "unchanged".
	w = doAdmin(r, http.MethodPut, "/api/v1/admin/stories/"+story.ID.String(),
		map[string]any{"caption": "Устрицы"}, nil, &token)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	if f.updated.ExpiresAt != nil {
		t.Fatalf("an omitted expires_at must arrive as nil, got %q", *f.updated.ExpiresAt)
	}
}

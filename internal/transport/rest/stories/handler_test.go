package stories

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/stories"
)

// fakeFacade: one settable err drives the error→HTTP mapping, canned return
// values drive the happy path (mirrors the reviews handler test). The admin
// methods are exercised by the admin router in admin_handler_test.go.
type fakeFacade struct {
	err error
	rv  []domain.Story

	// admin knobs
	story        *domain.Story   // canned Create/Update result
	adminErr     error           // overrides err for admin methods when set
	reordered    []uuid.UUID     // last ReorderStories argument
	reorderedRID uuid.UUID       // last ReorderStories restaurant id
	created      *uc.CreateInput // last CreateStory input
	updated      *uc.UpdateInput // last UpdateStory input
}

func (f *fakeFacade) List(context.Context, uuid.UUID) ([]domain.Story, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rv, nil
}

func (f *fakeFacade) adminError() error {
	if f.adminErr != nil {
		return f.adminErr
	}
	return f.err
}

func (f *fakeFacade) ListForAdmin(context.Context, uc.Actor, uuid.UUID) ([]domain.Story, error) {
	if err := f.adminError(); err != nil {
		return nil, err
	}
	return f.rv, nil
}

func (f *fakeFacade) CreateStory(_ context.Context, _ uc.Actor, in uc.CreateInput) (*domain.Story, error) {
	f.created = &in
	if err := f.adminError(); err != nil {
		return nil, err
	}
	return f.story, nil
}

func (f *fakeFacade) UpdateStory(_ context.Context, _ uc.Actor, _ uuid.UUID, in uc.UpdateInput) (*domain.Story, error) {
	f.updated = &in
	if err := f.adminError(); err != nil {
		return nil, err
	}
	return f.story, nil
}

func (f *fakeFacade) DeleteStory(context.Context, uc.Actor, uuid.UUID) error {
	return f.adminError()
}

func (f *fakeFacade) ReorderStories(_ context.Context, _ uc.Actor, rid uuid.UUID, orderedIDs []uuid.UUID) error {
	f.reorderedRID = rid
	f.reordered = orderedIDs
	return f.adminError()
}

func newRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(f)
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	return r
}

func do(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

const badUUID = "not-a-uuid"

// TestListHappyAndBadID: a good restaurant id returns 200, a malformed one
// returns 422 (the handler parses :id itself and writes 422 on a parse failure).
func TestListHappyAndBadID(t *testing.T) {
	rid := uuid.New().String()
	r := newRouter(&fakeFacade{})

	cases := []struct {
		name, path string
		want       int
	}{
		{"happy", "/api/v1/restaurants/" + rid + "/stories", http.StatusOK},
		{"bad id", "/api/v1/restaurants/" + badUUID + "/stories", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(r, http.MethodGet, tc.path)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestListShaping asserts the JSON envelope shape: caption is present when set
// and omitted when nil, and the fields match the agreed response contract.
func TestListShaping(t *testing.T) {
	caption := "Летнее меню"
	withCaption := domain.Story{ID: uuid.New(), ImageURL: "https://cdn/a2.jpg", Caption: &caption, SortOrder: 2, IsActive: true}
	noCaption := domain.Story{ID: uuid.New(), ImageURL: "https://cdn/a1.jpg", Caption: nil, SortOrder: 1, IsActive: true}
	r := newRouter(&fakeFacade{rv: []domain.Story{noCaption, withCaption}})

	w := do(r, http.MethodGet, "/api/v1/restaurants/"+uuid.New().String()+"/stories")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body)
	}
	var env struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("len data = %d, want 2", len(env.Data))
	}
	// First card has no caption → the key is omitted entirely.
	if _, ok := env.Data[0]["caption"]; ok {
		t.Errorf("card without caption still carries a caption key: %v", env.Data[0])
	}
	if env.Data[0]["image_url"] != "https://cdn/a1.jpg" || env.Data[0]["sort_order"].(float64) != 1 {
		t.Errorf("card 0 shaping mismatch: %v", env.Data[0])
	}
	// Second card carries its caption.
	if env.Data[1]["caption"] != caption {
		t.Errorf("card 1 caption = %v, want %q", env.Data[1]["caption"], caption)
	}
}

// TestListErrorMapping asserts the domain error → HTTP status mapping the
// response package implements.
func TestListErrorMapping(t *testing.T) {
	path := "/api/v1/restaurants/" + uuid.New().String() + "/stories"
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("%w: restaurant", domain.ErrNotFound), http.StatusNotFound},
		{"validation", fmt.Errorf("%w: bad", domain.ErrValidation), http.StatusUnprocessableEntity},
		{"internal", fmt.Errorf("db exploded"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(&fakeFacade{err: tc.err})
			w := do(r, http.MethodGet, path)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

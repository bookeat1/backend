package homefeeds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeFacade struct {
	cuisines   []domain.Cuisine
	promotions []domain.Promotion
	articles   []domain.Article
	err        error
}

func (f *fakeFacade) Cuisines(context.Context) ([]domain.Cuisine, error) {
	return f.cuisines, f.err
}
func (f *fakeFacade) Promotions(context.Context) ([]domain.Promotion, error) {
	return f.promotions, f.err
}
func (f *fakeFacade) Articles(context.Context) ([]domain.Article, error) {
	return f.articles, f.err
}

func newRouter(f *fakeFacade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	NewHandler(f).RegisterPublic(api)
	return r
}

func get(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCuisinesEndpoint(t *testing.T) {
	url := "https://cdn.book-eat.com/it.png"
	f := &fakeFacade{cuisines: []domain.Cuisine{
		{ID: uuid.New(), Name: "Итальянская", ImageURL: &url, Sort: 1},
	}}
	w := get(newRouter(f), "/api/v1/cuisines")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var env struct {
		Data []cuisineResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].Name != "Итальянская" || env.Data[0].ImageURL == nil {
		t.Errorf("data = %+v", env.Data)
	}
}

func TestPromotionsEndpoint(t *testing.T) {
	rid := uuid.New()
	label := "-30%"
	f := &fakeFacade{promotions: []domain.Promotion{
		{ID: uuid.New(), RestaurantID: &rid, Title: "Скидка", DiscountLabel: &label, Sort: 0},
	}}
	w := get(newRouter(f), "/api/v1/promotions")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var env struct {
		Data []promotionResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].RestaurantID == nil || *env.Data[0].RestaurantID != rid.String() {
		t.Errorf("data = %+v", env.Data)
	}
}

func TestArticlesEndpoint(t *testing.T) {
	f := &fakeFacade{articles: []domain.Article{
		{ID: uuid.New(), Title: "Куда сходить на неделе", Sort: 0},
	}}
	w := get(newRouter(f), "/api/v1/articles")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var env struct {
		Data []articleResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].Title != "Куда сходить на неделе" {
		t.Errorf("data = %+v", env.Data)
	}
}

func TestEmptyListReturnsArrayNotNull(t *testing.T) {
	w := get(newRouter(&fakeFacade{}), "/api/v1/cuisines")
	if body := strings.TrimSpace(w.Body.String()); body != `{"data":[]}` {
		t.Errorf("empty list body = %s, want {\"data\":[]}", body)
	}
}

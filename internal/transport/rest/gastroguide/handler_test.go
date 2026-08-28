package gastroguide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/gastroguide"
)

type fakeFacade struct {
	err         error
	categories  []domain.GuideCategory
	collections []domain.GuideCollection
	detail      *domain.GuideCollectionDetail

	gotInput uc.ListInput
	gotSlug  string
	gotCity  *domain.City
}

func (f *fakeFacade) ListCategories(_ context.Context, city *domain.City) ([]domain.GuideCategory, error) {
	f.gotCity = city
	return f.categories, f.err
}

func (f *fakeFacade) ListCollections(_ context.Context, in uc.ListInput) ([]domain.GuideCollection, int, error) {
	f.gotInput = in
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.collections, len(f.collections), nil
}

func (f *fakeFacade) GetCollection(_ context.Context, slug string) (*domain.GuideCollectionDetail, error) {
	f.gotSlug = slug
	if f.err != nil {
		return nil, f.err
	}
	return f.detail, nil
}

func router(f uc.Facade) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(f).RegisterPublic(r.Group("/api/v1"))
	return r
}

func do(t *testing.T, r *gin.Engine, url string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return w, body
}

// The venue order the usecase returns is the order the guest gets: the handler
// must not sort, group or otherwise "improve" it.
func TestGetCollection_KeepsVenueOrderAndOmitsMissingImages(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	cover := "https://pub-41b6f06fc8e74b6e959cdd6def081e22.r2.dev/guide/kids.jpg"
	f := &fakeFacade{detail: &domain.GuideCollectionDetail{
		GuideCollection: domain.GuideCollection{
			ID: uuid.New(), Slug: "kids", Title: "С детьми",
			CoverImageURL: &cover, VenueCount: 2,
		},
		Venues: []domain.GuideCollectionVenue{
			{RestaurantID: first, Position: 10, Name: "Первый", City: domain.CityAstana},
			{RestaurantID: second, Position: 20, Name: "Второй", City: domain.CityAstana,
				PrimaryImageURL: &cover},
		},
	}}

	w, body := do(t, router(f), "/api/v1/gastroguide/collections/kids")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if f.gotSlug != "kids" {
		t.Fatalf("slug = %q", f.gotSlug)
	}
	data := body["data"].(map[string]any)
	if data["cover_image_url"] != cover {
		t.Fatalf("cover = %v", data["cover_image_url"])
	}
	venues := data["venues"].([]any)
	if len(venues) != 2 {
		t.Fatalf("venues = %d, want 2", len(venues))
	}
	if venues[0].(map[string]any)["restaurant_id"] != first.String() ||
		venues[1].(map[string]any)["restaurant_id"] != second.String() {
		t.Fatalf("venue order changed by the transport: %v", venues)
	}
	// A venue with no picture must carry no picture field at all — never an
	// empty string or an invented placeholder URL.
	if _, present := venues[0].(map[string]any)["primary_image_url"]; present {
		t.Fatal("a venue without an image must omit primary_image_url")
	}
}

// A collection a guest may not see is a 404 with the generic not_found code:
// the same answer as an unknown slug, so an unpublished slug cannot be probed.
func TestGetCollection_NotFound(t *testing.T) {
	w, body := do(t, router(&fakeFacade{err: domain.ErrNotFound}),
		"/api/v1/gastroguide/collections/secret")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if body["code"] != string(domain.CodeNotFound) {
		t.Fatalf("code = %v, want %s", body["code"], domain.CodeNotFound)
	}
}

// An unknown city is refused with a machine-readable code, not answered with a
// silently empty guide.
func TestListCollections_UnknownCityIsCoded(t *testing.T) {
	f := &fakeFacade{}
	w, body := do(t, router(f), "/api/v1/gastroguide/collections?city=Париж")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if body["code"] != string(domain.CodeCityRequired) {
		t.Fatalf("code = %v, want %s", body["code"], domain.CodeCityRequired)
	}
	if f.gotInput.City != nil {
		t.Fatal("a rejected city must not reach the usecase")
	}
}

// No city at all is a legitimate call: the guide is readable before the guest
// has picked a city, unlike the city-scoped feed rail.
func TestListCollections_NoCityIsAllowed(t *testing.T) {
	f := &fakeFacade{collections: []domain.GuideCollection{{ID: uuid.New(), Slug: "kids", Title: "С детьми"}}}
	w, body := do(t, router(f), "/api/v1/gastroguide/collections")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if f.gotInput.City != nil {
		t.Fatalf("city = %v, want nil", f.gotInput.City)
	}
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if _, present := items[0].(map[string]any)["cover_image_url"]; present {
		t.Fatal("a collection without a cover must omit cover_image_url")
	}
}

// Localized text is resolved for the requested language, using the base value
// as the fallback — the same rule the rest of the public API follows.
func TestListCollections_Localized(t *testing.T) {
	f := &fakeFacade{collections: []domain.GuideCollection{{
		ID: uuid.New(), Slug: "kids", Title: "С детьми",
		TitleI18n: domain.I18n{"en": "With kids"},
		Subtitle:  "Проверено родителями",
	}}}
	_, body := do(t, router(f), "/api/v1/gastroguide/collections?lang=en")
	item := body["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if item["title"] != "With kids" {
		t.Fatalf("title = %v, want the en translation", item["title"])
	}
	if item["subtitle"] != "Проверено родителями" {
		t.Fatalf("subtitle = %v, want the ru fallback (no en translation stored)", item["subtitle"])
	}
}

func TestListCategories(t *testing.T) {
	f := &fakeFacade{categories: []domain.GuideCategory{
		{ID: uuid.New(), Slug: "kids", Title: "С детьми", Position: 1},
		{ID: uuid.New(), Slug: "breakfasts", Title: "Завтраки", Position: 2},
	}}
	w, body := do(t, router(f), "/api/v1/gastroguide/categories")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	items := body["data"].(map[string]any)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["slug"] != "kids" {
		t.Fatalf("categories = %v", items)
	}
}

// An article that links no venue still renders: `venues` must be an empty JSON
// array, never null and never a missing key — the client draws a list there and
// would have to guard null in three places otherwise. venue_count is 0 and is
// still present, so the card can say "0 мест" instead of guessing.
func TestGetCollection_EmptyVenueListIsAnArrayNotNull(t *testing.T) {
	f := &fakeFacade{detail: &domain.GuideCollectionDetail{
		GuideCollection: domain.GuideCollection{
			ID: uuid.New(), Slug: "city-guide", Title: "Гид по городу", VenueCount: 0,
		},
	}}

	w, body := do(t, router(f), "/api/v1/gastroguide/collections/city-guide")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	data := body["data"].(map[string]any)
	venues, ok := data["venues"].([]any)
	if !ok {
		t.Fatalf("venues = %#v, want an empty array", data["venues"])
	}
	if len(venues) != 0 {
		t.Fatalf("venues = %v, want empty", venues)
	}
	if data["venue_count"] != float64(0) {
		t.Fatalf("venue_count = %v, want 0", data["venue_count"])
	}
}

// --- articles vs collections (migration 0092) ---

// GET /gastroguide/collections asks the usecase for kind='collection', and
// GET /articles for kind='article'. The kind comes from the ROUTE, so neither
// listing can be talked into returning the other's rows by a query parameter.
func TestListings_AreScopedByKind(t *testing.T) {
	cases := []struct {
		url  string
		want domain.GuideCollectionKind
	}{
		{"/api/v1/gastroguide/collections", domain.GuideKindCollection},
		{"/api/v1/articles", domain.GuideKindArticle},
	}
	for _, tc := range cases {
		f := &fakeFacade{}
		w, _ := do(t, router(f), tc.url)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (%s)", tc.url, w.Code, w.Body.String())
		}
		if f.gotInput.Kind == nil {
			t.Fatalf("%s: kind filter is nil — the listing would return both kinds", tc.url)
		}
		if *f.gotInput.Kind != tc.want {
			t.Fatalf("%s: kind = %q, want %q", tc.url, *f.gotInput.Kind, tc.want)
		}
	}
}

// An article carries no rubric, so ?category= has no meaning on /articles. It is
// IGNORED rather than refused: a client reusing its collections screen with a
// stale rubric in the query string must still get the article feed, not a 422
// for asking a question that does not apply. It must not reach the usecase
// either — a rubric filter would empty the feed.
func TestListArticles_IgnoresTheCategoryFilter(t *testing.T) {
	f := &fakeFacade{}
	w, _ := do(t, router(f), "/api/v1/articles?category=breakfasts")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if f.gotInput.CategorySlug != nil {
		t.Fatalf("category = %v, want nil — an article has no rubric", *f.gotInput.CategorySlug)
	}
}

// The city filter behaves on /articles exactly as it does on the collections
// listing, including the coded refusal of an unknown city.
func TestListArticles_CityFilterBehavesLikeTheCollectionsListing(t *testing.T) {
	f := &fakeFacade{}
	w, _ := do(t, router(f), "/api/v1/articles?city="+url.QueryEscape(string(domain.CityAlmaty)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if f.gotInput.City == nil || *f.gotInput.City != domain.CityAlmaty {
		t.Fatalf("city = %v, want Алматы", f.gotInput.City)
	}

	f2 := &fakeFacade{}
	w2, body := do(t, router(f2), "/api/v1/articles?city="+url.QueryEscape("Париж"))
	if w2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w2.Code)
	}
	if body["code"] != string(domain.CodeCityRequired) {
		t.Fatalf("code = %v, want %s", body["code"], domain.CodeCityRequired)
	}
	if f2.gotInput.City != nil {
		t.Fatal("a rejected city must not reach the usecase")
	}
}

// `kind` is always present on a card, on both listings, so a client can branch
// on what it got. Never omitempty: a missing field would make "article" and "an
// old server" indistinguishable.
func TestListings_ExposeKindOnEveryCard(t *testing.T) {
	cases := []struct {
		url  string
		kind domain.GuideCollectionKind
	}{
		{"/api/v1/gastroguide/collections", domain.GuideKindCollection},
		{"/api/v1/articles", domain.GuideKindArticle},
	}
	for _, tc := range cases {
		f := &fakeFacade{collections: []domain.GuideCollection{
			{ID: uuid.New(), Slug: "s", Title: "T", Kind: tc.kind},
		}}
		_, body := do(t, router(f), tc.url)
		item := body["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
		if item["kind"] != string(tc.kind) {
			t.Fatalf("%s: kind = %v, want %q", tc.url, item["kind"], tc.kind)
		}
	}
}

// BOTH detail routes resolve ANY slug, article or collection. Mobile builds
// already in the wild deep-link articles through /gastroguide/collections/:slug;
// kind-scoping the detail read would 404 every one of those links on the next
// OTA, for rows that did not move anywhere. The response says which kind it is.
func TestDetailRoutes_ResolveEitherKind(t *testing.T) {
	cases := []struct {
		name string
		url  string
		slug string
		kind domain.GuideCollectionKind
	}{
		{"article through the legacy collection route",
			"/api/v1/gastroguide/collections/gde-poest-s-rebenkom-v-almaty",
			"gde-poest-s-rebenkom-v-almaty", domain.GuideKindArticle},
		{"collection through the article route",
			"/api/v1/articles/kazakh-cuisine",
			"kazakh-cuisine", domain.GuideKindCollection},
		{"article through the article route",
			"/api/v1/articles/gde-poest-s-rebenkom-v-almaty",
			"gde-poest-s-rebenkom-v-almaty", domain.GuideKindArticle},
		{"collection through the collection route",
			"/api/v1/gastroguide/collections/kazakh-cuisine",
			"kazakh-cuisine", domain.GuideKindCollection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFacade{detail: &domain.GuideCollectionDetail{
				GuideCollection: domain.GuideCollection{
					ID: uuid.New(), Slug: tc.slug, Title: "Т", Kind: tc.kind,
				},
			}}
			w, body := do(t, router(f), tc.url)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if f.gotSlug != tc.slug {
				t.Fatalf("slug = %q, want %q", f.gotSlug, tc.slug)
			}
			data := body["data"].(map[string]any)
			if data["kind"] != string(tc.kind) {
				t.Fatalf("kind = %v, want %q", data["kind"], tc.kind)
			}
		})
	}
}

package gastroguide

import (
	"context"
	"errors"
	"testing"
	"time"

	"backend-core/internal/domain"
)

// fakeRepo records what the facade asked the read model for. The visibility
// rules themselves live in SQL and are covered by the repository's integration
// tests; what matters here is that the facade passes the clock and the filters
// through unchanged and adds no rule of its own.
type fakeRepo struct {
	gotNow      time.Time
	gotFilter   domain.GuideCollectionFilter
	gotSlug     string
	gotCity     *domain.City
	collections []domain.GuideCollection
	detail      *domain.GuideCollectionDetail
	err         error
}

func (f *fakeRepo) ListCategories(_ context.Context, city *domain.City, now time.Time) ([]domain.GuideCategory, error) {
	f.gotCity, f.gotNow = city, now
	return nil, f.err
}

func (f *fakeRepo) ListPublishedCollections(_ context.Context, flt domain.GuideCollectionFilter, now time.Time) ([]domain.GuideCollection, int, error) {
	f.gotFilter, f.gotNow = flt, now
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.collections, len(f.collections), nil
}

func (f *fakeRepo) GetPublishedCollectionBySlug(_ context.Context, slug string, now time.Time) (*domain.GuideCollectionDetail, error) {
	f.gotSlug, f.gotNow = slug, now
	if f.err != nil {
		return nil, f.err
	}
	return f.detail, nil
}

func TestListCollections_PassesFiltersAndClockThrough(t *testing.T) {
	repo := &fakeRepo{collections: []domain.GuideCollection{{Slug: "kids"}}}
	fixed := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	f := &facade{repo: repo, clock: func() time.Time { return fixed }}

	city := domain.CityAstana
	slug := "breakfasts"
	items, total, err := f.ListCollections(context.Background(), ListInput{
		City: &city, CategorySlug: &slug, Page: 2, PerPage: 5,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(items))
	}
	if !repo.gotNow.Equal(fixed) {
		t.Fatalf("clock = %v, want the facade's own clock %v", repo.gotNow, fixed)
	}
	got := repo.gotFilter
	if got.City == nil || *got.City != city || got.CategorySlug == nil || *got.CategorySlug != slug ||
		got.Page != 2 || got.PerPage != 5 {
		t.Fatalf("filter passed through wrong: %+v", got)
	}
}

func TestGetCollection_EmptySlugIsValidation(t *testing.T) {
	repo := &fakeRepo{}
	f := &facade{repo: repo, clock: time.Now}

	if _, err := f.GetCollection(context.Background(), "   "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if repo.gotSlug != "" {
		t.Fatal("an empty slug must not reach the repository")
	}
}

func TestGetCollection_NotFoundIsPassedThrough(t *testing.T) {
	repo := &fakeRepo{err: domain.ErrNotFound}
	f := &facade{repo: repo, clock: time.Now}

	if _, err := f.GetCollection(context.Background(), "kids"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if repo.gotSlug != "kids" {
		t.Fatalf("slug = %q, want the trimmed input", repo.gotSlug)
	}
}

// Package gastroguide is the application logic of the editorial guide — the
// home screen's "подборки" block ("где поесть с детьми", "лучшие завтраки").
//
// Guest reads only. Everything an editor would need to WRITE a collection
// (create, reorder, publish, attach venues) is deliberately not here: that is a
// cabinet with its own RBAC and its own reordering semantics, and half of it
// would be worse than none.
//
// The facade holds no visibility logic of its own on purpose. Which collection
// is live and which venue may be shown lives in SQL
// (infrastructure/postgres/gastroguide), so no filter passed by a caller can
// widen it; the facade only validates the filters and supplies the clock.
package gastroguide

import (
	"context"
	"fmt"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// Facade exposes the guest-facing reads of the gastroguide.
type Facade interface {
	// ListCategories returns the guide's rubrics that currently hold at least
	// one live collection.
	ListCategories(ctx context.Context, city *domain.City) ([]domain.GuideCategory, error)
	// ListCollections returns live collections, paginated, plus the total.
	ListCollections(ctx context.Context, in ListInput) ([]domain.GuideCollection, int, error)
	// GetCollection returns one live collection with its ordered venues.
	// ErrNotFound covers both "no such slug" and "not published".
	GetCollection(ctx context.Context, slug string) (*domain.GuideCollectionDetail, error)
}

// ListInput carries the optional filters of the collection listing.
type ListInput struct {
	City         *domain.City
	CategorySlug *string
	Page         int
	PerPage      int
}

type facade struct {
	repo  domain.GastroguideRepository
	clock func() time.Time
}

// NewFacade constructs the gastroguide Facade.
func NewFacade(repo domain.GastroguideRepository) Facade {
	return &facade{repo: repo, clock: time.Now}
}

func (f *facade) ListCategories(ctx context.Context, city *domain.City) ([]domain.GuideCategory, error) {
	return f.repo.ListCategories(ctx, city, f.clock())
}

func (f *facade) ListCollections(ctx context.Context, in ListInput) ([]domain.GuideCollection, int, error) {
	return f.repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{
		City:         in.City,
		CategorySlug: in.CategorySlug,
		Page:         in.Page,
		PerPage:      in.PerPage,
	}, f.clock())
}

func (f *facade) GetCollection(ctx context.Context, slug string) (*domain.GuideCollectionDetail, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("%w: slug is required", domain.ErrValidation)
	}
	return f.repo.GetPublishedCollectionBySlug(ctx, slug, f.clock())
}

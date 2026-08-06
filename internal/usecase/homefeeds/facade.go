// Package homefeeds is the application logic for the mobile Home ("Explore")
// screen: the cuisine picker, the promotions strip and the articles block.
// Every operation is a read; curation happens elsewhere.
package homefeeds

import (
	"context"

	"backend-core/internal/domain"
)

// Facade exposes the read-only Home feed lists.
type Facade interface {
	Cuisines(ctx context.Context) ([]domain.Cuisine, error)
	Promotions(ctx context.Context) ([]domain.Promotion, error)
	Articles(ctx context.Context) ([]domain.Article, error)
}

type facade struct {
	cuisines   domain.CuisineRepository
	promotions domain.PromotionRepository
	articles   domain.ArticleRepository
}

// NewFacade constructs the homefeeds Facade.
func NewFacade(
	cuisines domain.CuisineRepository,
	promotions domain.PromotionRepository,
	articles domain.ArticleRepository,
) Facade {
	return &facade{cuisines: cuisines, promotions: promotions, articles: articles}
}

func (f *facade) Cuisines(ctx context.Context) ([]domain.Cuisine, error) {
	return f.cuisines.ListActive(ctx)
}

func (f *facade) Promotions(ctx context.Context) ([]domain.Promotion, error) {
	return f.promotions.ListActive(ctx)
}

func (f *facade) Articles(ctx context.Context) ([]domain.Article, error) {
	return f.articles.ListActive(ctx)
}

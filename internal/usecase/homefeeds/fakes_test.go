package homefeeds

import (
	"context"

	"backend-core/internal/domain"
)

type fakeCuisines struct {
	list []domain.Cuisine
	err  error
}

func (f *fakeCuisines) ListActive(context.Context) ([]domain.Cuisine, error) {
	return f.list, f.err
}

type fakePromotions struct {
	list []domain.Promotion
	err  error
}

func (f *fakePromotions) ListActive(context.Context) ([]domain.Promotion, error) {
	return f.list, f.err
}

type fakeArticles struct {
	list []domain.Article
	err  error
}

func (f *fakeArticles) ListActive(context.Context) ([]domain.Article, error) {
	return f.list, f.err
}

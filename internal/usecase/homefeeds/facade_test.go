package homefeeds

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestFacadeDelegatesToRepositories(t *testing.T) {
	cuisines := &fakeCuisines{list: []domain.Cuisine{{ID: uuid.New(), Name: "Итальянская"}}}
	promotions := &fakePromotions{list: []domain.Promotion{{ID: uuid.New(), Title: "-30%"}}}
	articles := &fakeArticles{list: []domain.Article{{ID: uuid.New(), Title: "Куда сходить"}}}
	f := NewFacade(cuisines, promotions, articles)
	ctx := context.Background()

	gotC, err := f.Cuisines(ctx)
	if err != nil || len(gotC) != 1 || gotC[0].Name != "Итальянская" {
		t.Errorf("Cuisines = %+v, %v", gotC, err)
	}
	gotP, err := f.Promotions(ctx)
	if err != nil || len(gotP) != 1 || gotP[0].Title != "-30%" {
		t.Errorf("Promotions = %+v, %v", gotP, err)
	}
	gotA, err := f.Articles(ctx)
	if err != nil || len(gotA) != 1 || gotA[0].Title != "Куда сходить" {
		t.Errorf("Articles = %+v, %v", gotA, err)
	}
}

func TestFacadePropagatesErrors(t *testing.T) {
	sentinel := errors.New("boom")
	f := NewFacade(
		&fakeCuisines{err: sentinel},
		&fakePromotions{err: sentinel},
		&fakeArticles{err: sentinel},
	)
	ctx := context.Background()

	if _, err := f.Cuisines(ctx); !errors.Is(err, sentinel) {
		t.Errorf("Cuisines err = %v, want sentinel", err)
	}
	if _, err := f.Promotions(ctx); !errors.Is(err, sentinel) {
		t.Errorf("Promotions err = %v, want sentinel", err)
	}
	if _, err := f.Articles(ctx); !errors.Is(err, sentinel) {
		t.Errorf("Articles err = %v, want sentinel", err)
	}
}

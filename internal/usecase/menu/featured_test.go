package menu

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The rail is city-scoped by contract. A missing or unknown city must fail
// loudly with its own code, not quietly widen into a country-wide list: the app
// needs to tell "pick a city" apart from a real failure.
func TestListFeaturedRequiresAKnownCity(t *testing.T) {
	items := newFakeItems()
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	for _, city := range []domain.City{"", "Караганда"} {
		_, err := f.ListFeatured(context.Background(), city, 0)
		if err == nil {
			t.Fatalf("city %q must be rejected", city)
		}
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("city %q: want ErrValidation, got %v", city, err)
		}
		code, ok := domain.CodeOf(err)
		if !ok || code != domain.CodeCityRequired {
			t.Fatalf("city %q: want code %q, got %q (present=%v)", city, domain.CodeCityRequired, code, ok)
		}
	}
}

// Limit is clamped in the usecase so neither an omitted value nor an absurd one
// reaches SQL.
func TestListFeaturedClampsLimit(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"omitted falls back to the default", 0, featuredLimitDefault},
		{"negative falls back to the default", -5, featuredLimitDefault},
		{"in range is passed through", 7, 7},
		{"above the ceiling is capped", 5000, featuredLimitMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := newFakeItems()
			f := NewFacade(items, &fakeCategories{}, &inlineTx{})
			if _, err := f.ListFeatured(context.Background(), "Алматы", tc.in); err != nil {
				t.Fatalf("list featured: %v", err)
			}
			if items.featuredArg.Limit != tc.want {
				t.Fatalf("limit %d: want %d reaching the repository, got %d", tc.in, tc.want, items.featuredArg.Limit)
			}
		})
	}
}

func TestListFeaturedPassesCityThrough(t *testing.T) {
	items := newFakeItems()
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if _, err := f.ListFeatured(context.Background(), "Астана", 3); err != nil {
		t.Fatalf("list featured: %v", err)
	}
	if items.featuredArg.City != "Астана" {
		t.Fatalf("city not passed through: %q", items.featuredArg.City)
	}
}

// The facade delegates the ownership check to SQL rather than reading the item
// first: one statement, and no window between the check and the write.
func TestSetFeaturedRefusesAnItemOfAnotherVenue(t *testing.T) {
	items := newFakeItems()
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	mine, theirs := uuid.New(), uuid.New()
	dish := uuid.New()
	items.store[dish] = &domain.MenuItem{ID: dish, RestaurantID: theirs, Name: "Their plov"}

	err := f.SetFeatured(context.Background(), mine, dish, true)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if items.store[dish].IsFeatured {
		t.Fatal("the dish must not have been promoted")
	}

	if err := f.SetFeatured(context.Background(), theirs, dish, true); err != nil {
		t.Fatalf("the owner must be able to promote its own dish: %v", err)
	}
	if !items.store[dish].IsFeatured {
		t.Fatal("the owner's promotion must stick")
	}
}

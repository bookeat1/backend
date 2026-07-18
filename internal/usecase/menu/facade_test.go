package menu

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestCreateValidatesNameAndPrice(t *testing.T) {
	f := NewFacade(newFakeItems(), &fakeCategories{}, &inlineTx{})
	if _, err := f.Create(context.Background(), uuid.New(), ItemInput{Price: strp("10")}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("missing name err = %v, want ErrValidation", err)
	}
	if _, err := f.Create(context.Background(), uuid.New(), ItemInput{Name: strp("Plov"), Price: strp("abc")}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("bad price err = %v, want ErrValidation", err)
	}
}

func TestCreateSetsItemAndTags(t *testing.T) {
	items := newFakeItems()
	tx := &inlineTx{}
	f := NewFacade(items, &fakeCategories{}, tx)
	rid := uuid.New()
	_, err := f.Create(context.Background(), rid, ItemInput{Name: strp("Plov"), Price: strp("3500.00"), Tags: &[]string{"halal"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if items.created == nil || items.created.RestaurantID != rid || !tx.called {
		t.Error("expected item created with restaurant id inside tx")
	}
	if items.replaceCall != 1 || len(items.tagsFor[items.created.ID]) != 1 {
		t.Errorf("expected tags replaced once, got %d", items.replaceCall)
	}
}

func TestUpdateRejectsCrossRestaurantItem(t *testing.T) {
	items := newFakeItems()
	itemID, ownerRID := uuid.New(), uuid.New()
	items.store[itemID] = &domain.MenuItem{ID: itemID, RestaurantID: ownerRID, Price: "1"}
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	// caller claims a different restaurant → IDOR guard → ErrNotFound
	if _, err := f.Update(context.Background(), uuid.New(), itemID, ItemInput{Name: strp("X")}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-restaurant update err = %v, want ErrNotFound", err)
	}
	if items.updated != nil {
		t.Error("must not update a cross-restaurant item")
	}
}

func TestUpdatePreservesTagsWhenOmitted(t *testing.T) {
	items := newFakeItems()
	itemID, rid := uuid.New(), uuid.New()
	items.store[itemID] = &domain.MenuItem{ID: itemID, RestaurantID: rid, Price: "1"}
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	if _, err := f.Update(context.Background(), rid, itemID, ItemInput{Name: strp("New")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if items.replaceCall != 0 {
		t.Error("tags must not be replaced when Tags is nil")
	}
	if _, err := f.Update(context.Background(), rid, itemID, ItemInput{Tags: &[]string{"a"}}); err != nil {
		t.Fatalf("update tags: %v", err)
	}
	if items.replaceCall != 1 {
		t.Error("tags must be replaced when Tags is provided")
	}
}

func TestCreateDedupesTags(t *testing.T) {
	items := newFakeItems()
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	rid := uuid.New()
	_, err := f.Create(context.Background(), rid, ItemInput{
		Name: strp("Plov"), Price: strp("1"), Tags: &[]string{"halal", "halal", "spicy"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tags := items.tagsFor[items.created.ID]
	if len(tags) != 2 {
		t.Errorf("tags = %d, want 2 after de-dup of [halal, halal, spicy]", len(tags))
	}
}

func TestUpdateCategoryRejectsSelfParent(t *testing.T) {
	id := uuid.New()
	cats := &fakeCategories{}
	f := NewFacade(newFakeItems(), cats, &inlineTx{})
	_, err := f.UpdateCategory(context.Background(), id, CategoryInput{Name: "X", ParentID: &id})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("self-parent err = %v, want ErrValidation", err)
	}
	if cats.updated != nil {
		t.Error("must not persist a self-referential parent")
	}
}

func TestUpdateCategoryRejectsCycle(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	// Existing tree: b's parent is a. Re-parenting a under b closes an a→b→a loop.
	cats := &fakeCategories{list: []domain.MenuCategory{
		{ID: a},
		{ID: b, ParentID: &a},
	}}
	f := NewFacade(newFakeItems(), cats, &inlineTx{})
	_, err := f.UpdateCategory(context.Background(), a, CategoryInput{Name: "A", ParentID: &b})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("cycle err = %v, want ErrValidation", err)
	}
	if cats.updated != nil {
		t.Error("must not persist a parent assignment that creates a cycle")
	}
}

func TestUpdateCategoryAllowsValidReparent(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cats := &fakeCategories{list: []domain.MenuCategory{{ID: a}, {ID: b}}}
	f := NewFacade(newFakeItems(), cats, &inlineTx{})
	if _, err := f.UpdateCategory(context.Background(), a, CategoryInput{Name: "A", ParentID: &b}); err != nil {
		t.Fatalf("valid reparent: %v", err)
	}
	if cats.updated == nil || cats.updated.ParentID == nil || *cats.updated.ParentID != b {
		t.Error("expected a to be re-parented under b")
	}
}

func TestSetAvailableChecksOwnership(t *testing.T) {
	items := newFakeItems()
	itemID, rid := uuid.New(), uuid.New()
	items.store[itemID] = &domain.MenuItem{ID: itemID, RestaurantID: rid, Price: "1", IsAvailable: true}
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	if err := f.SetAvailable(context.Background(), uuid.New(), itemID, false); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("cross-restaurant setavailable err = %v, want ErrNotFound", err)
	}
	if err := f.SetAvailable(context.Background(), rid, itemID, false); err != nil {
		t.Fatalf("setavailable: %v", err)
	}
	if items.availID != itemID || items.avail != false {
		t.Error("expected SetAvailable(false) on the owned item")
	}
}

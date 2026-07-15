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

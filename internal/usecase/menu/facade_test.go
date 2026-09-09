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

// Editing an existing dish must not be able to hide it from the guest either:
// flipping a base row's label to another language is the same mistake as
// creating one, only on a dish guests already see today.
func TestUpdateCannotHideAnExistingDishBehindALanguageLabel(t *testing.T) {
	rid, itemID := uuid.New(), uuid.New()
	ru := "ru"
	items := newFakeItems()
	items.store[itemID] = &domain.MenuItem{
		ID: itemID, RestaurantID: rid, Name: "Плов", Price: "2500.00",
		IsAvailable: true, Language: &ru,
	}
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	en := "en"
	_, err := f.Update(context.Background(), rid, itemID, ItemInput{Language: &en})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeMenuItemLanguageNotBase {
		t.Fatalf("want code %q, got %q (present=%v)", domain.CodeMenuItemLanguageNotBase, code, ok)
	}
	if got := items.store[itemID].Language; got == nil || *got != "ru" {
		t.Fatalf("the refused write must not touch the stored label: %v", got)
	}
}

// ...but a LEGACY row that already carries a non-base label stays editable: the
// 124 imported Kazakh copies are visible in the cabinet, and refusing their
// edits would make those dishes unfixable. Such a row is already out of the
// guest listing, so keeping its label changes nothing for the guest. 'kz' is
// accepted as the same value as 'kk' because the panel echoes back whatever it
// was given and migration 0100 rewrote what is stored.
func TestUpdateStillSavesALegacyTranslationRow(t *testing.T) {
	rid, itemID := uuid.New(), uuid.New()
	kk := "kk"
	items := newFakeItems()
	items.store[itemID] = &domain.MenuItem{
		ID: itemID, RestaurantID: rid, Name: "Палау", Price: "2500.00",
		IsAvailable: true, Language: &kk,
	}
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	newName := "Палау (тауық еті)"
	for _, echoed := range []string{"kk", "kz", "KK-KZ"} {
		lang := echoed
		if _, err := f.Update(context.Background(), rid, itemID, ItemInput{Name: &newName, Language: &lang}); err != nil {
			t.Fatalf("echoed label %q must stay saveable: %v", echoed, err)
		}
	}
	if got := items.store[itemID].Language; got == nil || *got != "kk" {
		t.Fatalf("the label must be stored canonically, got %v", got)
	}
}

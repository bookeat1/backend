package main

import (
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func mkPrice(p int64) *int64 { return &p }

func TestBuildPlanInsertsNewDishes(t *testing.T) {
	parsed := []ParsedItem{
		{Section: "Salads", Name: "Caesar Salad", Price: mkPrice(4500)},
	}
	plan, err := BuildPlan(nil, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToInsert) != 1 || len(plan.ToUpdate) != 0 || plan.Unchanged != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.ToInsert[0].Name != "Caesar Salad" || !plan.ToInsert[0].IsAvailable {
		t.Errorf("insert row = %+v", plan.ToInsert[0])
	}
	if got := plan.Sections; len(got) != 1 || got[0] != "Salads" {
		t.Errorf("Sections = %v", got)
	}
}

func TestBuildPlanUpdatesChangedPrice(t *testing.T) {
	existing := []domain.MenuItem{
		{ID: uuid.New(), Name: "Caesar Salad", Price: "4000.00"},
	}
	parsed := []ParsedItem{
		{Name: "Caesar Salad", Price: mkPrice(4500)},
	}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToUpdate) != 1 || len(plan.ToInsert) != 0 || plan.Unchanged != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.ToUpdate[0].ID != existing[0].ID {
		t.Error("update must preserve the existing row's ID")
	}
	if plan.ToUpdate[0].Price != "4500.00" {
		t.Errorf("Price = %q", plan.ToUpdate[0].Price)
	}
}

func TestBuildPlanMatchesByTrimmedCaseInsensitiveName(t *testing.T) {
	existing := []domain.MenuItem{
		{ID: uuid.New(), Name: "  Caesar Salad ", Price: "4500.00"},
	}
	parsed := []ParsedItem{
		{Name: "CAESAR SALAD", Price: mkPrice(4500)},
	}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// Same price, but Name itself differs after trim/case-fold in the stored
	// column, so it still counts as a (name-normalizing) update, not unchanged,
	// unless the raw Name also matches after ToDomainFields trims it.
	if len(plan.ToInsert) != 0 {
		t.Fatalf("must match the existing row, not insert a duplicate: plan = %+v", plan)
	}
}

func TestBuildPlanLeavesAbsentDishesUntouched(t *testing.T) {
	keepID := uuid.New()
	existing := []domain.MenuItem{
		{ID: keepID, Name: "Old Dish Not In File", Price: "1000.00", IsAvailable: true},
	}
	parsed := []ParsedItem{
		{Name: "New Dish", Price: mkPrice(2000)},
	}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToInsert) != 1 {
		t.Fatalf("expected 1 insert, got %+v", plan.ToInsert)
	}
	if len(plan.ToUpdate) != 0 {
		t.Fatalf("the absent dish must never be queued for update/delete: %+v", plan.ToUpdate)
	}
	// Nothing in the plan structure can even express a delete/deactivate — this
	// test documents the guarantee for anyone tempted to add one.
}

func TestBuildPlanReportsUnchangedWhenNothingDiffers(t *testing.T) {
	desc := "tasty"
	cat := "Salads"
	existing := []domain.MenuItem{
		{ID: uuid.New(), Name: "Caesar Salad", Price: "4500.00", Description: desc, Category: &cat},
	}
	parsed := []ParsedItem{
		{Name: "Caesar Salad", Price: mkPrice(4500), Description: &desc, Section: cat},
	}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.Unchanged != 1 || len(plan.ToUpdate) != 0 || len(plan.ToInsert) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestBuildPlanPairsDuplicateNamesInOrderFIFO(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	existing := []domain.MenuItem{
		{ID: id1, Name: "Cola", Price: "500.00"},
		{ID: id2, Name: "Cola", Price: "600.00"},
	}
	parsed := []ParsedItem{
		{Name: "Cola", Price: mkPrice(550)},
		{Name: "Cola", Price: mkPrice(650)},
		{Name: "Cola", Price: mkPrice(700)}, // 3rd occurrence: no existing row left -> insert
	}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToUpdate) != 2 || len(plan.ToInsert) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.ToUpdate[0].ID != id1 || plan.ToUpdate[0].Price != "550.00" {
		t.Errorf("first update = %+v, want id1/550.00", plan.ToUpdate[0])
	}
	if plan.ToUpdate[1].ID != id2 || plan.ToUpdate[1].Price != "650.00" {
		t.Errorf("second update = %+v, want id2/650.00", plan.ToUpdate[1])
	}
}

func TestBuildPlanSkipsInvalidPriceWithoutBlockingTheRestOfTheFile(t *testing.T) {
	parsed := []ParsedItem{
		{Name: "Good", Price: mkPrice(100)},
		{Name: "Bad"}, // nil price: e.g. a wine list scan with no printed price
	}
	plan, err := BuildPlan(nil, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToInsert) != 1 || plan.ToInsert[0].Name != "Good" {
		t.Fatalf("expected the good item to still be inserted: %+v", plan.ToInsert)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Name != "Bad" {
		t.Fatalf("expected the bad item to be reported as skipped: %+v", plan.Skipped)
	}
}

func TestBuildPlanSkipsNegativePriceOnAnExistingMatch(t *testing.T) {
	existing := []domain.MenuItem{{ID: uuid.New(), Name: "Wine", Price: "1000.00"}}
	neg := int64(-5)
	parsed := []ParsedItem{{Name: "Wine", Price: &neg}}
	plan, err := BuildPlan(existing, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToUpdate) != 0 || len(plan.Skipped) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestBuildPlanCollectsSectionsInFirstSeenOrderWithoutDuplicates(t *testing.T) {
	parsed := []ParsedItem{
		{Name: "A", Price: mkPrice(1), Section: "Drinks"},
		{Name: "B", Price: mkPrice(1), Section: "Salads"},
		{Name: "C", Price: mkPrice(1), Section: "Drinks"},
		{Name: "D", Price: mkPrice(1), Section: ""},
	}
	plan, err := BuildPlan(nil, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := []string{"Drinks", "Salads"}
	if len(plan.Sections) != len(want) || plan.Sections[0] != want[0] || plan.Sections[1] != want[1] {
		t.Errorf("Sections = %v, want %v", plan.Sections, want)
	}
}

func TestMissingCategoriesSkipsExistingCaseInsensitively(t *testing.T) {
	existing := []domain.MenuCategory{{Name: "  Salads "}}
	got := missingCategories(existing, []string{"salads", "Drinks", "DRINKS"})
	if len(got) != 1 || got[0] != "Drinks" {
		t.Errorf("missingCategories = %v, want [Drinks]", got)
	}
}

func TestMissingCategoriesEmptyWhenAllExist(t *testing.T) {
	existing := []domain.MenuCategory{{Name: "Salads"}, {Name: "Drinks"}}
	got := missingCategories(existing, []string{"Salads", "drinks"})
	if len(got) != 0 {
		t.Errorf("missingCategories = %v, want none", got)
	}
}

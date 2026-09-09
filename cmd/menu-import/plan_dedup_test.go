package main

import "testing"

// TestBuildPlanMergesDuplicateNamesWithinTheFileIntoOneInsert guards the
// unique constraint on (restaurant_id, name): two file rows with the exact
// same name that both have no existing DB row to pair with must produce ONE
// insert (last row wins), not two — inserting the same name twice in one run
// would violate the DB constraint (seen for real on the Local Coffee /
// Koktobe / Chaihana Palau / Mongol menu files).
func TestBuildPlanMergesDuplicateNamesWithinTheFileIntoOneInsert(t *testing.T) {
	parsed := []ParsedItem{
		{Name: "Ягодный микс", Price: mkPrice(1000), Section: "Завтраки"},
		{Name: "Ягодный микс", Price: mkPrice(1200), Section: "Обед"},
	}
	plan, err := BuildPlan(nil, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToInsert) != 1 {
		t.Fatalf("expected 1 merged insert, got %d: %+v", len(plan.ToInsert), plan.ToInsert)
	}
	if plan.ToInsert[0].Price != "1200.00" {
		t.Errorf("expected the LAST file row to win, got price %q", plan.ToInsert[0].Price)
	}
	if plan.ToInsert[0].Category == nil || *plan.ToInsert[0].Category != "Обед" {
		t.Errorf("expected the last row's section to win, got %v", plan.ToInsert[0].Category)
	}
}

func TestBuildPlanMergeDoesNotAffectDistinctNames(t *testing.T) {
	parsed := []ParsedItem{
		{Name: "A", Price: mkPrice(100)},
		{Name: "B", Price: mkPrice(200)},
	}
	plan, err := BuildPlan(nil, parsed)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.ToInsert) != 2 {
		t.Fatalf("expected 2 inserts, got %+v", plan.ToInsert)
	}
}

package domain

import "testing"

// card is a minimal stand-in for whatever DiversifyByKey's real callers use
// (a restaurant with its main cuisine, a feed card with its restaurant id).
type card struct {
	id  string
	key string
}

func keys(cards []card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.key
	}
	return out
}

func ids(cards []card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.id
	}
	return out
}

func TestDiversifyByKey(t *testing.T) {
	tests := []struct {
		name     string
		input    []card
		wantKeys []string
	}{
		{name: "empty input", input: nil, wantKeys: []string{}},
		{
			name:     "single item",
			input:    []card{{"a", "italian"}},
			wantKeys: []string{"italian"},
		},
		{
			name:     "two of the same key never needs to move",
			input:    []card{{"a", "italian"}, {"b", "italian"}},
			wantKeys: []string{"italian", "italian"},
		},
		{
			name: "already diverse input is left untouched",
			input: []card{
				{"a", "italian"}, {"b", "kazakh"}, {"c", "italian"}, {"d", "kazakh"},
			},
			wantKeys: []string{"italian", "kazakh", "italian", "kazakh"},
		},
		{
			// The spec's own worked example (criterion 7): 6 italian then 2
			// kazakh — the kazakh cards must land by position 3 and 6, not the
			// tail.
			name: "criterion 7: 6 italian + 2 kazakh pulls kazakh forward, not to the tail",
			input: []card{
				{"i1", "italian"}, {"i2", "italian"}, {"i3", "italian"}, {"i4", "italian"},
				{"i5", "italian"}, {"i6", "italian"}, {"k1", "kazakh"}, {"k2", "kazakh"},
			},
			wantKeys: []string{"italian", "italian", "kazakh", "italian", "italian", "kazakh", "italian", "italian"},
		},
		{
			// No alternative key exists anywhere in the remainder: the rule
			// cannot be satisfied without inventing content, so the run of
			// three+ is placed as-is rather than the function jamming or
			// dropping an item.
			name:     "no alternative key anywhere: places the run as-is",
			input:    []card{{"a", "italian"}, {"b", "italian"}, {"c", "italian"}, {"d", "italian"}},
			wantKeys: []string{"italian", "italian", "italian", "italian"},
		},
		{
			// Cards without a cuisine share the synthetic "none" group like
			// any other key — DiversifyByKey does not special-case it, the
			// caller is expected to pass "none" as keyOf's return value.
			name: "the 'none' group is diversified exactly like any other key",
			input: []card{
				{"a", "none"}, {"b", "none"}, {"c", "none"}, {"d", "italian"},
			},
			wantKeys: []string{"none", "none", "italian", "none"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiversifyByKey(tt.input, func(c card) string { return c.key })
			gotKeys := keys(got)
			if len(gotKeys) != len(tt.wantKeys) {
				t.Fatalf("got %v, want %v", gotKeys, tt.wantKeys)
			}
			for i := range gotKeys {
				if gotKeys[i] != tt.wantKeys[i] {
					t.Fatalf("got %v, want %v", gotKeys, tt.wantKeys)
				}
			}
			// No item invented or dropped: the id set is preserved exactly.
			gotIDs, wantIDs := ids(got), ids(tt.input)
			if len(gotIDs) != len(wantIDs) {
				t.Fatalf("item count changed: got %d, want %d", len(gotIDs), len(wantIDs))
			}
			seen := map[string]bool{}
			for _, id := range gotIDs {
				if seen[id] {
					t.Fatalf("id %q appears twice in output %v", id, gotIDs)
				}
				seen[id] = true
			}
			for _, id := range wantIDs {
				if !seen[id] {
					t.Fatalf("id %q from input missing in output %v", id, gotIDs)
				}
			}
		})
	}
}

// TestDiversifyByKeyNeverThreeInARow is a property check over the criterion-7
// shape scaled up: whatever the input, the output never has three consecutive
// items with the same key when at least one alternative exists anywhere in
// the list at the time a run would otherwise form.
func TestDiversifyByKeyNeverThreeInARow(t *testing.T) {
	input := []card{
		{"1", "european"}, {"2", "european"}, {"3", "european"}, {"4", "european"},
		{"5", "european"}, {"6", "european"}, {"7", "european"}, {"8", "european"},
		{"9", "european"}, {"10", "european"}, {"11", "european"}, {"12", "european"},
		{"13", "seafood"}, {"14", "seafood"}, {"15", "seafood"},
		{"16", "oriental"}, {"17", "kazakh"}, {"18", "georgian"}, {"19", "greek"}, {"20", "vegan"},
	}
	got := DiversifyByKey(input, func(c card) string { return c.key })
	run := 1
	for i := 1; i < len(got); i++ {
		if got[i].key == got[i-1].key {
			run++
		} else {
			run = 1
		}
		if run > 2 {
			t.Fatalf("three or more consecutive %q at position %d: %v", got[i].key, i, keys(got))
		}
	}
}

func TestDiversifyByKeyDeterministic(t *testing.T) {
	input := []card{
		{"a", "italian"}, {"b", "italian"}, {"c", "italian"}, {"d", "kazakh"}, {"e", "italian"},
	}
	got1 := DiversifyByKey(input, func(c card) string { return c.key })
	got2 := DiversifyByKey(input, func(c card) string { return c.key })
	if len(got1) != len(got2) {
		t.Fatalf("length differs between calls")
	}
	for i := range got1 {
		if got1[i] != got2[i] {
			t.Fatalf("output differs between calls at %d: %v vs %v", i, got1, got2)
		}
	}
}

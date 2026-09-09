package restaurants

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// rawItem returns the first item of a page as raw JSON, so a test can assert
// about the PRESENCE of a field, not only its decoded value — "matched_dish is
// absent" and "matched_dish is null" are different promises to the app.
func rawItem(t *testing.T, body map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body["data"], &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	return page.Items[0]
}

// TestSearchPayloadCarriesMatchedDish: a venue that the guest's query found
// through its MENU has to be able to say so on the card. The same field must
// stay out of the plain catalog listing, which has no query and therefore
// nothing to explain.
func TestSearchPayloadCarriesMatchedDish(t *testing.T) {
	id := uuid.New()
	dishID := uuid.New()
	rest := activeVenue(id)
	rest.Name = "Ocean Blue"
	f := &fakeFacade{
		item: domain.RestaurantListItem{
			Restaurant: rest,
			MatchedDish: &domain.MatchedDish{
				ID:       dishID,
				Name:     "Паста карбонара",
				NameI18n: domain.I18n{"en": "Carbonara pasta"},
			},
		},
		agg: &domain.RestaurantAggregate{Restaurant: rest},
	}
	r := newTestRouter(f)

	item := rawItem(t, doGET(t, r, "/api/v1/restaurants/search?q=%D0%BF%D0%B0%D1%81%D1%82%D0%B0"))
	raw, ok := item["matched_dish"]
	if !ok {
		t.Fatalf("search item carries no matched_dish: %v", item)
	}
	var dish struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &dish); err != nil {
		t.Fatalf("decode matched_dish: %v", err)
	}
	if dish.ID != dishID.String() {
		t.Errorf("matched_dish.id = %q, want %q", dish.ID, dishID)
	}
	if dish.Name != "Паста карбонара" {
		t.Errorf("matched_dish.name = %q, want the ru name", dish.Name)
	}

	// The caption follows the request locale, like every other localized text
	// in this payload.
	item = rawItem(t, doGET(t, r, "/api/v1/restaurants/search?q=pasta&lang=en"))
	if err := json.Unmarshal(item["matched_dish"], &dish); err != nil {
		t.Fatalf("decode en matched_dish: %v", err)
	}
	if dish.Name != "Carbonara pasta" {
		t.Errorf("matched_dish.name (en) = %q, want %q", dish.Name, "Carbonara pasta")
	}
}

// TestListPayloadHasNoMatchedDish pins the other half: the frozen catalog
// listing shape does not grow a field. The facade returns the same item, and
// the listing simply never has a MatchedDish to serialize.
func TestListPayloadHasNoMatchedDish(t *testing.T) {
	id := uuid.New()
	rest := activeVenue(id)
	f := &fakeFacade{
		item: domain.RestaurantListItem{Restaurant: rest},
		agg:  &domain.RestaurantAggregate{Restaurant: rest},
	}
	r := newTestRouter(f)

	for _, path := range []string{"/api/v1/restaurants", "/api/v1/restaurants/search?q=x"} {
		item := rawItem(t, doGET(t, r, path))
		if _, ok := item["matched_dish"]; ok {
			t.Errorf("%s serialized matched_dish for a venue that has none", path)
		}
	}
}

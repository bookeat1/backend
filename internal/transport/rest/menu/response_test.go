package menu

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The formatted `price` stays exactly as the mobile app reads it today, and the
// NUMERIC amount travels beside it. Without price_minor a client that wants to
// show «добавить · 8 990 ₸» has to parse the label back into tiyn, which is
// inventing money.
func TestMenuItemCarriesBothTheLabelAndTheNumericPrice(t *testing.T) {
	for _, tc := range []struct {
		price string
		minor int64
	}{
		{"8990.00", 899000},
		{"0.10", 10},
		{"1234.56", 123456},
		{"4500", 450000},
	} {
		m := domain.MenuItem{ID: uuid.New(), RestaurantID: uuid.New(), Name: "Блюдо", Price: tc.price}
		raw, err := json.Marshal(itemToResponse(&m))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got struct {
			Price      string `json:"price"`
			PriceMinor *int64 `json:"price_minor"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Price != tc.price {
			t.Fatalf("the formatted price must be untouched: want %q, got %q", tc.price, got.Price)
		}
		if got.PriceMinor == nil {
			t.Fatalf("price %q: price_minor is missing", tc.price)
		}
		if *got.PriceMinor != tc.minor {
			t.Fatalf("price %q: want %d tiyn, got %d", tc.price, tc.minor, *got.PriceMinor)
		}
		// The label and the number must describe the SAME money, through the
		// one converter the pre-order flow charges with.
		want, err := domain.PriceStringToMinor(got.Price)
		if err != nil {
			t.Fatalf("price %q: %v", tc.price, err)
		}
		if *got.PriceMinor != want {
			t.Fatalf("price %q: label says %d tiyn, payload says %d", tc.price, want, *got.PriceMinor)
		}
	}
}

// A price the exact converter refuses becomes null, never 0 — «мы не можем
// назвать сумму» is honest, «блюдо бесплатное» is a lie about money.
func TestUnconvertiblePriceIsNullNotZero(t *testing.T) {
	m := domain.MenuItem{ID: uuid.New(), RestaurantID: uuid.New(), Name: "Блюдо", Price: "по запросу"}
	raw, err := json.Marshal(itemToResponse(&m))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, present := got["price_minor"]; !present || v != nil {
		t.Fatalf("want an explicit null price_minor, got %#v (present=%v)", v, present)
	}
}

// The venue's own mark is exposed separately from is_featured, which is the
// platform's cross-venue rail. Collapsing the two would let a venue put itself
// on the main screen by curating its own storefront.
func TestTopPickIsReportedSeparatelyFromIsFeatured(t *testing.T) {
	slot := 2
	m := domain.MenuItem{
		ID: uuid.New(), RestaurantID: uuid.New(), Name: "Блюдо", Price: "1000.00",
		IsFeatured: false, TopPickPosition: &slot,
	}
	var got struct {
		IsFeatured      bool `json:"is_featured"`
		IsTopPick       bool `json:"is_top_pick"`
		TopPickPosition *int `json:"top_pick_position"`
	}
	raw, _ := json.Marshal(itemToResponse(&m))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IsFeatured {
		t.Fatal("marking a top pick must not make the dish a platform pick")
	}
	if !got.IsTopPick || got.TopPickPosition == nil || *got.TopPickPosition != 2 {
		t.Fatalf("want is_top_pick=true at slot 2, got %+v", got)
	}

	unmarked := domain.MenuItem{ID: uuid.New(), RestaurantID: uuid.New(), Name: "Другое", Price: "1000.00"}
	raw, _ = json.Marshal(itemToResponse(&unmarked))
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IsTopPick || got.TopPickPosition != nil {
		t.Fatalf("an unmarked dish must report no slot, got %+v", got)
	}
}

package domain

import "testing"

func TestCityValid(t *testing.T) {
	cases := map[City]bool{"Астана": true, "Алматы": true, "almaty": false, "": false}
	for c, want := range cases {
		if got := c.Valid(); got != want {
			t.Errorf("City(%q).Valid() = %v, want %v", c, got, want)
		}
	}
}

func TestPriceCategoryValid(t *testing.T) {
	cases := map[PriceCategory]bool{"₸": true, "₸₸": true, "₸₸₸": true, "$": false, "": false}
	for p, want := range cases {
		if got := p.Valid(); got != want {
			t.Errorf("PriceCategory(%q).Valid() = %v, want %v", p, got, want)
		}
	}
}

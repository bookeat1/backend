package domain

import (
	"errors"
	"testing"
)

func TestValidPrice(t *testing.T) {
	cases := map[string]bool{"4500": true, "4500.00": true, "0": true, "12.5": true,
		"": false, "12.345": false, "-5": false, "abc": false, "1,000": false}
	for in, want := range cases {
		if got := ValidPrice(in); got != want {
			t.Errorf("ValidPrice(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPriceStringToMinor(t *testing.T) {
	ok := map[string]int64{
		"4500":    450000,
		"4500.00": 450000,
		"4500.5":  450050,
		"4500.50": 450050,
		"0":       0,
		"0.10":    10, // exact: 10 tiyn, not 9 (no float rounding)
		"0.01":    1,
		"12.99":   1299,
	}
	for in, want := range ok {
		got, err := PriceStringToMinor(in)
		if err != nil {
			t.Errorf("PriceStringToMinor(%q) unexpected err: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("PriceStringToMinor(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"", "-5", "12.345", "abc", "1,000", "4500.", ".5"}
	for _, in := range bad {
		if _, err := PriceStringToMinor(in); !errors.Is(err, ErrValidation) {
			t.Errorf("PriceStringToMinor(%q) err = %v, want ErrValidation", in, err)
		}
	}
}

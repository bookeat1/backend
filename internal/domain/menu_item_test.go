package domain

import "testing"

func TestValidPrice(t *testing.T) {
	cases := map[string]bool{"4500": true, "4500.00": true, "0": true, "12.5": true,
		"": false, "12.345": false, "-5": false, "abc": false, "1,000": false}
	for in, want := range cases {
		if got := ValidPrice(in); got != want {
			t.Errorf("ValidPrice(%q) = %v, want %v", in, got, want)
		}
	}
}

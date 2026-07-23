package geo

import "testing"

func TestValidCountryCode(t *testing.T) {
	valid := []string{"KZ", "US", "RU", "DE", "AE"}
	for _, c := range valid {
		if !ValidCountryCode(c) {
			t.Errorf("ValidCountryCode(%q) = false, want true", c)
		}
	}

	invalid := []string{"kz", "ZZ", "XX", "KAZ", "1Z", "", "K", "K Z"}
	for _, c := range invalid {
		if ValidCountryCode(c) {
			t.Errorf("ValidCountryCode(%q) = true, want false", c)
		}
	}
}

package domain

import (
	"strings"
	"testing"
)

// TestValidAttributionSource is spec marathon-qr-attribution-20260921 §4
// criterion 5/9: the shape a channel tag must have to be stored at all,
// mirrored by the CHECK constraints added in migration 0115.
func TestValidAttributionSource(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"tshirt", "tshirt", true},
		{"box", "box", true},
		{"digits, dash and underscore", "box-promo_2", true},
		{"single char", "a", true},
		{"exactly 32 chars", strings.Repeat("a", 32), true},
		{"empty", "", false},
		{"33 chars", strings.Repeat("a", 33), false},
		{"uppercase", "TSHIRT", false},
		{"space", "t shirt", false},
		{"cyrillic", "футболка", false},
		{"leading slash", "/tshirt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidAttributionSource(tt.in); got != tt.want {
				t.Errorf("ValidAttributionSource(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeAttributionSource: a valid tag round-trips as-is, everything
// else (including an absent value) reports ok=false so the caller stores
// NULL — never an error, spec criterion 9 ("Отказа быть не должно").
func TestSanitizeAttributionSource(t *testing.T) {
	if got, ok := SanitizeAttributionSource("tshirt"); !ok || got != "tshirt" {
		t.Errorf("SanitizeAttributionSource(tshirt) = (%q, %v), want (tshirt, true)", got, ok)
	}
	if got, ok := SanitizeAttributionSource(""); ok || got != "" {
		t.Errorf("SanitizeAttributionSource(\"\") = (%q, %v), want (\"\", false)", got, ok)
	}
	if got, ok := SanitizeAttributionSource("НЕ ВАЛИДНО"); ok || got != "" {
		t.Errorf("SanitizeAttributionSource(invalid) = (%q, %v), want (\"\", false)", got, ok)
	}
}

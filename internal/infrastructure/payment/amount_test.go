package payment

import (
	"errors"
	"testing"

	"backend-core/internal/domain"
)

func TestFormatMinor(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{99, "0.99"},
		{100, "1.00"},
		{10350, "103.50"},
		{1000000, "10000.00"},
		{-2550, "-25.50"},
	}
	for _, tc := range tests {
		if got := FormatMinor(tc.minor); got != tc.want {
			t.Errorf("FormatMinor(%d) = %q, want %q", tc.minor, got, tc.want)
		}
	}
}

func TestParseMinor(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"integer", "103", 10300, false},
		{"two decimals", "103.50", 10350, false},
		{"one decimal", "103.5", 10350, false},
		{"comma separator", "103,50", 10350, false},
		{"zero", "0", 0, false},
		{"padded", "  159.00 ", 15900, false},
		{"negative", "-25.50", -2550, false},
		{"trailing zeros beyond two decimals", "10.5000", 1050, false},
		{"real precision beyond the currency", "10.501", 0, true},
		{"empty", "", 0, true},
		{"not a number", "abc", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMinor(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMinor(%q) = %d, want error", tc.in, got)
				}
				if !errors.Is(err, domain.ErrValidation) {
					t.Errorf("error %v does not wrap ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMinor(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseMinor(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestAmountRoundTrip is the property that matters for money: whatever we send
// an acquirer must come back as the same integer number of tiyn.
func TestAmountRoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, 7, 99, 100, 350, 10350, 999999999} {
		got, err := ParseMinor(FormatMinor(minor))
		if err != nil {
			t.Fatalf("round trip %d: %v", minor, err)
		}
		if got != minor {
			t.Errorf("round trip %d -> %q -> %d", minor, FormatMinor(minor), got)
		}
	}
}

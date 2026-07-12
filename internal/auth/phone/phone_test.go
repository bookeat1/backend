package phone

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"8 707 123 4567":  "+77071234567",
		"+7 707 123 4567": "+77071234567",
		"77071234567":     "+77071234567",
		"7071234567":      "+77071234567",
		"":                "",
		"+1 202 555 0100": "+12025550100",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

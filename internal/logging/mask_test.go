package logging

import "testing"

func TestMaskPhone(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+77075552233", "+7707***2233"},
		{"+7 707 555 22 33", "+7 70***2 33"}, // formatting is preserved, only the middle is blanked
		{"", ""},
		{"12345", "***"},    // too short for a safe head+tail split
		{"+7707555", "***"}, // == headLen+tailLen, still masked in full
	}
	for _, c := range cases {
		if got := MaskPhone(c.in); got != c.want {
			t.Errorf("MaskPhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"damir@gmail.com", "d***@gmail.com"},
		{"a@b.com", "a***@b.com"},
		{"", "***"},
		{"not-an-email", "***"},
		{"@nolocal.com", "***"},
	}
	for _, c := range cases {
		if got := MaskEmail(c.in); got != c.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Never regress into logging a card number or CVV as if it were a phone/email
// — MaskPhone/MaskEmail must not be usable in a way that plausibly looks safe
// for card data by leaking more than 4 digits.
func TestMaskPhoneNeverLeaksMoreThanTailOnCardLikeInput(t *testing.T) {
	pan := "4111111111111111" // 16 digits, card-shaped input reaching this helper by mistake
	got := MaskPhone(pan)
	if len(got) >= len(pan) {
		t.Fatalf("MaskPhone(%q) = %q did not shrink a long numeric input", pan, got)
	}
}

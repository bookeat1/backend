package domain

import "testing"

// The alias table is the backward-compatibility contract with installed store
// builds and with the imported data: 'kz' is what the old menu import and older
// app builds say, 'kk' is what everything else says.
func TestNormalizeLocale(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ru", "ru"},
		{"kk", "kk"},
		{"en", "en"},
		{"kz", "kk"},
		{"KZ", "kk"},
		{" kz ", "kk"},
		{"kk-KZ", "kk"},
		{"kz-KZ", "kk"},
		{"ru_RU", "ru"},
		{"kaz", "kk"},
		{"qaz", "kk"},
		{"", ""},
		{"fr", ""},
		{"zh", ""},
		{"-", ""},
	}
	for _, tc := range cases {
		if got := NormalizeLocale(tc.in); got != tc.want {
			t.Errorf("NormalizeLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Whatever comes out of NormalizeLocale must be servable: a code that passes
// normalization but is not in SupportedLocales would resolve to a translation
// key nobody ever writes.
func TestNormalizeLocaleOnlyEverProducesSupportedCodes(t *testing.T) {
	for _, in := range []string{"ru", "kk", "en", "kz", "kaz", "qaz", "rus", "eng", "KK-kz"} {
		got := NormalizeLocale(in)
		if got == "" || !IsSupportedLocale(got) {
			t.Errorf("NormalizeLocale(%q) = %q, which is not a supported locale", in, got)
		}
	}
}

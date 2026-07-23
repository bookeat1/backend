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

func TestCities(t *testing.T) {
	got := Cities()
	if len(got) != 2 || got[0] != CityAstana || got[1] != CityAlmaty {
		t.Fatalf("Cities() = %v, want [Астана Алматы]", got)
	}
	for _, c := range got {
		if !c.Valid() {
			t.Errorf("Cities() returned invalid city %q", c)
		}
	}
}

func TestI18nResolve(t *testing.T) {
	full := I18n{"ru": "Ресторан", "kk": "Мейрамхана", "en": "Restaurant"}
	partial := I18n{"ru": "Ресторан"} // no kk/en translation
	empty := I18n{"kk": ""}           // present key but empty value

	cases := []struct {
		name string
		i    I18n
		lang string
		base string
		want string
	}{
		{"no lang requested falls back to base", full, "", "base", "base"},
		{"nil map falls back to base", nil, "kk", "base", "base"},
		{"exact translation present", full, "kk", "base", "Мейрамхана"},
		{"missing translation falls back to base", partial, "en", "base", "base"},
		{"empty-string translation falls back to base", empty, "kk", "base", "base"},
		{"ru translation present", full, "ru", "base", "Ресторан"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.i.Resolve(tc.lang, tc.base); got != tc.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", tc.lang, tc.base, got, tc.want)
			}
		})
	}
}

func TestIsSupportedLocale(t *testing.T) {
	cases := map[string]bool{"ru": true, "kk": true, "en": true, "fr": false, "": false}
	for lang, want := range cases {
		if got := IsSupportedLocale(lang); got != want {
			t.Errorf("IsSupportedLocale(%q) = %v, want %v", lang, got, want)
		}
	}
}

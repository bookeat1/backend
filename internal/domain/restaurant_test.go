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

// TestI18nWithLocale pins the three rules the venue-rename fix leans on: other
// languages survive, a NULL column grows a map, and an empty value removes the
// entry instead of storing a translation that reads as missing.
func TestI18nWithLocale(t *testing.T) {
	base := I18n{"ru": "Старое", "kk": "Ескі"}

	got := base.WithLocale(LocaleRU, "Новое")
	if got["ru"] != "Новое" || got["kk"] != "Ескі" {
		t.Errorf("WithLocale = %v, want ru replaced and kk kept", got)
	}
	if base["ru"] != "Старое" {
		t.Errorf("the receiver was mutated: %v", base)
	}

	if got := I18n(nil).WithLocale(LocaleRU, "Новое"); got["ru"] != "Новое" {
		t.Errorf("WithLocale on a nil map = %v, want it created", got)
	}
	if got := I18n(nil).WithLocale(LocaleRU, ""); got != nil {
		t.Errorf("clearing on a nil map = %v, want nil (column stays NULL)", got)
	}

	cleared := base.WithLocale(LocaleRU, "")
	if _, ok := cleared["ru"]; ok {
		t.Errorf("cleared = %v, want the ru entry removed", cleared)
	}
	if cleared["kk"] != "Ескі" {
		t.Errorf("cleared = %v, want kk untouched", cleared)
	}
	if only := (I18n{"ru": "Старое"}).WithLocale(LocaleRU, ""); only != nil {
		t.Errorf("clearing the only entry = %v, want nil rather than an empty map", only)
	}
}

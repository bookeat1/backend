package domain

import (
	"errors"
	"testing"
)

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
		// ko and zh became servable on 2026-09-02. The script subtag is
		// dropped like any other: Simplified is the only Chinese we store, so
		// zh-Hant readers get it rather than falling all the way back to ru.
		{"ko", "ko"},
		{"ko-KR", "ko"},
		{"KO", "ko"},
		{"kor", "ko"},
		{"zh", "zh"},
		{"zh-CN", "zh"},
		{"zh-Hans", "zh"},
		{"zh-Hans-CN", "zh"},
		{"zh-Hant", "zh"},
		{"zh_TW", "zh"},
		{"zho", "zh"},
		{"chi", "zh"},
		{"", ""},
		{"fr", ""},
		{"ja", ""},
		{"cn", ""}, // a country code, not a language: not accepted, unlike the historical kz
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
	for _, in := range []string{
		"ru", "kk", "en", "kz", "kaz", "qaz", "rus", "eng", "KK-kz",
		"ko", "ko-KR", "kor", "zh", "zh-Hans", "zh-Hant", "zho", "chi",
	} {
		got := NormalizeLocale(in)
		if got == "" || !IsSupportedLocale(got) {
			t.Errorf("NormalizeLocale(%q) = %q, which is not a supported locale", in, got)
		}
	}
}

// --- I18nPatch: the wire shape of every translation write ---

// The whole point of the patch: what the request did not mention survives it.
// A cabinet editing Kazakh must not have to resend English to keep it.
func TestI18nPatchApplyToKeepsUnmentionedLanguages(t *testing.T) {
	stored := I18n{"ru": "Терраса", "kk": "Террасса", "en": "Terrace"}
	got := I18nPatch{"kk": sp("Шатыр террасасы")}.ApplyTo(stored)

	if got["kk"] != "Шатыр террасасы" {
		t.Errorf("kk = %q, want the new translation", got["kk"])
	}
	if got["en"] != "Terrace" {
		t.Errorf("en = %q, want the untouched translation kept", got["en"])
	}
	if stored["kk"] != "Террасса" {
		t.Errorf("the stored map was mutated: kk = %q", stored["kk"])
	}
}

// null (and a blank string, which reads as missing anyway) is how a language is
// REMOVED — the state a full-replace map could not tell from "not mentioned".
func TestI18nPatchApplyToRemovesOnNullAndBlank(t *testing.T) {
	stored := I18n{"ru": "Терраса", "kk": "Шатыр", "en": "Terrace"}
	got := I18nPatch{"en": nil, "kk": sp("   ")}.ApplyTo(stored)

	if _, ok := got["en"]; ok {
		t.Error("a null value must remove the language")
	}
	if _, ok := got["kk"]; ok {
		t.Error("a blank value must remove the language too")
	}
	if got["ru"] != "Терраса" {
		t.Errorf("ru = %q, want it untouched", got["ru"])
	}
}

// ru never travels through the map: it is the plain column, and ApplyTo leaves
// that decision to the caller (which is what keeps the two from disagreeing).
func TestI18nPatchApplyToIgnoresRussian(t *testing.T) {
	got := I18nPatch{"ru": sp("Из карты")}.ApplyTo(I18n{"ru": "Из колонки"})
	if got["ru"] != "Из колонки" {
		t.Errorf("ru = %q, want the stored column value untouched by ApplyTo", got["ru"])
	}
}

// A map emptied by a patch comes back nil so the column goes to NULL rather
// than holding an empty object nobody can read anything out of.
func TestI18nPatchApplyToEmptiedMapIsNil(t *testing.T) {
	if got := (I18nPatch{"kk": nil}).ApplyTo(I18n{"kk": "Шатыр"}); got != nil {
		t.Errorf("got %#v, want nil so the column becomes NULL", got)
	}
}

func TestI18nPatchValidate(t *testing.T) {
	cases := []struct {
		name    string
		patch   I18nPatch
		wantErr bool
	}{
		{"supported languages", I18nPatch{"kk": sp("а"), "en": sp("b")}, false},
		{"alias and region tag", I18nPatch{"kz": sp("а"), "en-US": sp("b")}, false},
		{"russian value is allowed", I18nPatch{"ru": sp("Текст")}, false},
		{"empty patch", I18nPatch{}, false},
		{"nil patch", nil, false},
		{"korean is a language now", I18nPatch{"ko": sp("한국어")}, false},
		{"chinese is a language now", I18nPatch{"zh": sp("中文")}, false},
		{"unsupported language", I18nPatch{"fr": sp("bonjour")}, true},
		{"two spellings of one language", I18nPatch{"kk": sp("а"), "kk-KZ": sp("б")}, true},
		{"deleting russian", I18nPatch{"ru": nil}, true},
		{"blanking russian", I18nPatch{"ru": sp("  ")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.patch.Validate("venue_i18n")
			if tc.wantErr && err == nil {
				t.Fatal("want a validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr && !errors.Is(err, ErrValidation) {
				t.Fatalf("want ErrValidation (→ 422), got %v", err)
			}
		})
	}
}

// A patch spelled the old way ('kz', 'kk-KZ') must land under the canonical
// key, or the write would create a language the reads never look for.
func TestI18nPatchApplyToNormalizesKeys(t *testing.T) {
	got := I18nPatch{"kz": sp("Шатыр"), "en-US": sp("Terrace")}.ApplyTo(nil)
	if got["kk"] != "Шатыр" {
		t.Errorf(`got["kk"] = %q, want the alias normalized`, got["kk"])
	}
	if got["en"] != "Terrace" {
		t.Errorf(`got["en"] = %q, want the region subtag dropped`, got["en"])
	}
}

func TestI18nPatchRussian(t *testing.T) {
	if v, ok := (I18nPatch{"kk": sp("Шатыр")}).Russian(); ok {
		t.Errorf("no ru key, got %q", v)
	}
	if v, ok := (I18nPatch{"ru": sp("Терраса")}).Russian(); !ok || v != "Терраса" {
		t.Errorf("Russian() = %q, %v; want the ru value", v, ok)
	}
}

// ApplyTranslations is the one write path: merge, then make i18n["ru"] equal
// the column again.
func TestApplyTranslations(t *testing.T) {
	stored := I18n{"ru": "Старое", "kk": "Ескі", "en": "Old"}

	got := ApplyTranslations(stored, I18nPatch{"kk": sp("Жаңа")}, "Новое")
	if got["ru"] != "Новое" {
		t.Errorf(`ru = %q, want it re-synced with the column`, got["ru"])
	}
	if got["kk"] != "Жаңа" {
		t.Errorf(`kk = %q, want the patched translation`, got["kk"])
	}
	if got["en"] != "Old" {
		t.Errorf(`en = %q, want the untouched translation kept`, got["en"])
	}

	// The column always wins over a ru key in the patch: a body carrying two
	// Russian values is a confused client, and the column is the answer.
	got = ApplyTranslations(stored, I18nPatch{"ru": sp("Из карты")}, "Из колонки")
	if got["ru"] != "Из колонки" {
		t.Errorf(`ru = %q, want the column to win`, got["ru"])
	}

	// An empty column means "there is no Russian text": ru is dropped, the
	// other languages stay (we were told the Russian is gone, not that the
	// Kazakh is wrong).
	got = ApplyTranslations(stored, nil, "")
	if _, ok := got["ru"]; ok {
		t.Error("an empty column must drop the ru entry")
	}
	if got["kk"] != "Ескі" {
		t.Errorf("kk = %q, want it kept", got["kk"])
	}
}

// Two rows that render the same in every language ARE the same content, even
// when one carries the redundant ru key and the other (written before the
// invariant was enforced) does not.
func TestI18nRenderEqual(t *testing.T) {
	if !I18nRenderEqual("Терраса", I18n{"ru": "Терраса", "kk": "Шатыр"}, "Терраса", I18n{"kk": "Шатыр"}) {
		t.Error("a redundant ru key must not read as a content change")
	}
	if I18nRenderEqual("Терраса", I18n{"ru": "Другое"}, "Терраса", nil) {
		t.Error("a ru entry that contradicts its column IS a difference")
	}
	if I18nRenderEqual("Терраса", I18n{"kk": "Шатыр"}, "Терраса", nil) {
		t.Error("an added translation is a content change")
	}
	if I18nRenderEqual("Терраса", nil, "Зал", nil) {
		t.Error("a changed column is a content change")
	}
}

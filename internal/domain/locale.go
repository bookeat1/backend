package domain

import (
	"fmt"
	"strings"
)

// localeAliases maps the language codes that DO occur in the wild — old app
// builds, imported spreadsheets, the legacy Supabase menu — onto the canonical
// codes of SupportedLocales.
//
// "kz" is the important one and the reason this file exists: menu_items.language
// was imported with 'kz', while the whole application (SupportedLocales, every
// *_i18n map key) speaks 'kk'. The two never met, so a guest asking for Kazakh
// got an empty menu. Data is normalized by migration 0100; this map is what
// keeps a REQUEST spelled the old way working, because store builds that send
// ?lang=kz stay installed for months.
//
// Note that 'kz' is not a language code at all (it is the ISO country code of
// Kazakhstan) — it is accepted, never produced.
var localeAliases = map[string]string{
	"kz":  LocaleKK,
	"kaz": LocaleKK,
	"qaz": LocaleKK,
	"rus": LocaleRU,
	"eng": LocaleEN,
}

// NormalizeLocale reduces a caller- or import-supplied language tag to one of
// SupportedLocales, or returns "" when it cannot. It lowercases, drops the
// region subtag ("kk-KZ", "ru_RU" → "kk", "ru") and applies localeAliases.
//
// It never invents a locale: an unknown tag comes back as "" so the caller can
// decide what "unknown" means for it (reqlocale answers ru; a write keeps the
// raw value rather than losing it).
func NormalizeLocale(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if a, ok := localeAliases[l]; ok {
		l = a
	}
	if !IsSupportedLocale(l) {
		return ""
	}
	return l
}

// I18nPatch is a PARTIAL update of an I18n map — the wire shape of every
// `<field>_i18n` object an admin write accepts:
//
//	{"kk": "Жайлы орын", "en": null}
//
// Three states per language, and the middle one is the reason this type exists
// instead of a plain map[string]string:
//
//	key absent            → that language is left exactly as it is stored;
//	key with a string     → that language is written;
//	key with a null/blank → that language is REMOVED.
//
// A cabinet that edits Kazakh must not have to resend English to keep it, and
// a full-replace map cannot tell "I did not touch English" from "delete
// English". Blank counts as a removal because I18n.Resolve already reads an
// empty translation as missing: keeping the key would store a value that reads
// as absent and still shows up in every payload.
type I18nPatch map[string]*string

// Russian reports the ru text carried by the patch, if any.
//
// ru is NOT applied to the map by ApplyTo: the plain column IS the Russian text
// (see LocaleRU and I18n.WithLocale), so a patch that names ru is asking to
// change the COLUMN, and the caller routes it there — which then syncs the map
// key back. That keeps the one invariant the whole scheme rests on: the column
// and i18n["ru"] never disagree.
func (p I18nPatch) Russian() (string, bool) {
	for k, v := range p {
		if NormalizeLocale(k) != LocaleRU {
			continue
		}
		if v == nil {
			return "", true
		}
		return *v, true
	}
	return "", false
}

// Validate rejects a patch that could not be applied honestly. field names the
// JSON field in the message so an operator knows which input was refused.
//
// Refused: a language outside SupportedLocales (a translation nothing can ever
// read back — see the ko/zh rows the old import left behind), two keys that
// normalize to the same language ("kk" and "kk-KZ" in one object, where the
// winner would depend on Go's map order), and deleting ru (the Russian text is
// the column; clear it by sending an empty base field, not by deleting a key).
func (p I18nPatch) Validate(field string) error {
	seen := make(map[string]string, len(p))
	for k, v := range p {
		l := NormalizeLocale(k)
		if l == "" {
			return fmt.Errorf("%w: %s: unsupported language %q", ErrValidation, field, k)
		}
		if prev, dup := seen[l]; dup {
			return fmt.Errorf("%w: %s: keys %q and %q both mean %q", ErrValidation, field, prev, k, l)
		}
		seen[l] = k
		if l == LocaleRU && (v == nil || strings.TrimSpace(*v) == "") {
			return fmt.Errorf(
				"%w: %s: the Russian text lives in the plain field, not in this map; clear it by sending an empty %s",
				ErrValidation, field, strings.TrimSuffix(field, "_i18n"))
		}
	}
	return nil
}

// ApplyTo merges the patch onto the stored map and returns the result, leaving
// base untouched (it is usually a map read straight out of the database and
// shared with the caller's aggregate).
//
// ru is skipped — see Russian. A result with no entries comes back nil so the
// column goes back to NULL instead of holding `{}`.
//
// Callers must Validate first: an unsupported key is silently ignored here
// rather than stored, because writing a language nothing reads is the one
// outcome worse than refusing the request.
func (p I18nPatch) ApplyTo(base I18n) I18n {
	if len(p) == 0 {
		return base
	}
	out := make(I18n, len(base)+len(p))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range p {
		l := NormalizeLocale(k)
		if l == "" || l == LocaleRU {
			continue
		}
		if v == nil || strings.TrimSpace(*v) == "" {
			delete(out, l)
			continue
		}
		out[l] = *v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ApplyTranslations is the ONE write path for a localized text field: it merges
// the patch onto the stored map and then re-establishes the invariant the whole
// scheme rests on — i18n["ru"] is the plain column, byte for byte.
//
// column is the Russian text as it will be stored. It always wins over a `ru`
// key inside the patch: a body that carries two different Russian values is a
// confused client, and the answer to "which one is Russian" is fixed. (Where
// the plain field can be ABSENT from the request — the restaurants PATCH — the
// caller promotes the patch's `ru` into the column first, and by the time this
// runs the two agree.)
//
// An empty column drops the ru entry rather than storing "": I18n.Resolve reads
// an empty translation as missing anyway, and a map left with nothing in it
// comes back nil so the column goes to NULL.
func ApplyTranslations(base I18n, patch I18nPatch, column string) I18n {
	return patch.ApplyTo(base).WithLocale(LocaleRU, column)
}

// I18nRenderEqual reports whether two localized fields would read IDENTICALLY
// to a guest in every language: same base column, same translations, with the
// redundant `ru` key normalized away.
//
// The normalization is what makes this different from comparing the two maps.
// `ru` duplicates the column by invariant (ApplyTranslations), so a row written
// before that invariant was enforced carries no `ru` key while an equivalent
// row written after carries one — and a plain map comparison would call them
// different content. They resolve to the same text for every locale, so they
// are the same content, and "did the words change" (feed re-moderation, series
// inheritance) must answer no.
//
// A `ru` entry that does NOT match its column is kept, deliberately: that is a
// broken invariant, and hiding it here would make an edit that fixes it look
// like a no-op.
func I18nRenderEqual(aBase string, a I18n, bBase string, b I18n) bool {
	if aBase != bBase {
		return false
	}
	na, nb := withoutRedundantRU(a, aBase), withoutRedundantRU(b, bBase)
	if len(na) != len(nb) {
		return false
	}
	for k, v := range na {
		if nb[k] != v {
			return false
		}
	}
	return true
}

// withoutRedundantRU returns i without its ru entry when that entry merely
// repeats base. The map is not copied unless something is actually dropped.
func withoutRedundantRU(i I18n, base string) I18n {
	if v, ok := i[LocaleRU]; !ok || v != base {
		return i
	}
	out := make(I18n, len(i)-1)
	for k, v := range i {
		if k != LocaleRU {
			out[k] = v
		}
	}
	return out
}

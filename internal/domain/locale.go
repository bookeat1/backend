package domain

import "strings"

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

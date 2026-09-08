// Package geo has small, dependency-light geography helpers: today just ISO
// 3166-1 alpha-2 country code validation for the guest profile's "country"
// field (tourist/local analytics).
package geo

import "golang.org/x/text/language"

// ValidCountryCode reports whether code is a real, currently assigned ISO
// 3166-1 alpha-2 country code (e.g. "KZ", "US"). It rejects lowercase input,
// malformed strings, and reserved/unassigned codes (e.g. "ZZ", "XX") — this
// uses golang.org/x/text's CLDR-backed region data instead of a hand-maintained
// list, so it never drifts as ISO adds/retires codes.
func ValidCountryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	r, err := language.ParseRegion(code)
	if err != nil {
		return false
	}
	// ParseRegion is case-insensitive and normalizes to canonical case; require
	// the input to already be canonical uppercase so callers don't silently
	// persist "kz" instead of "KZ".
	if r.String() != code {
		return false
	}
	return r.IsCountry()
}

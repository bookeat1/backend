package domain

import "regexp"

// AttributionSourceMaxLen is the bookings.attribution_source /
// users.attribution_source column width (migration 0115).
const AttributionSourceMaxLen = 32

// attributionSourceRe mirrors the CHECK constraints added in migration 0115
// (bookings_attribution_source_format, users_attribution_source_format) and
// the spec's own contract (marathon-qr-attribution-20260921 §4 criterion 5,
// §5): lowercase ASCII letters, digits, underscore and hyphen, 1-32 chars.
// Deliberately case-sensitive lowercase-only — "tshirt" and "TSHIRT" reading
// as two different channels in a report is a worse failure mode than a
// dashboard operator being asked to paste the value in lower case once.
var attributionSourceRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ValidAttributionSource reports whether s is an acceptable channel tag as
// typed (no normalization — unlike promo codes, a channel tag has no
// "spellings" to fold together, it either matches the printed QR string or
// it is not a channel BookEat knows).
func ValidAttributionSource(s string) bool {
	return attributionSourceRe.MatchString(s)
}

// SanitizeAttributionSource returns (s, true) for a valid tag, or ("", false)
// for anything else — including the empty string, which means "no tag was
// sent" rather than an invalid one. Every caller (POST /bookings today, the
// account first-touch endpoint later) must go through this: an invalid or
// oversized value is silently dropped (spec §4 criterion 9 — "Отказа быть не
// должно"), never rejected and never truncated to fit the column.
func SanitizeAttributionSource(s string) (string, bool) {
	if !ValidAttributionSource(s) {
		return "", false
	}
	return s, true
}

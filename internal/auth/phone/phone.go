// Package phone normalizes user-typed phone numbers to E.164, matching the
// frontend's normalizePhone (default country code +7 for the KZ/RU market).
package phone

import "strings"

// Normalize returns raw as E.164 ("+7..."), or "" when raw has no digits.
func Normalize(raw string) string {
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if d == "" {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "+") {
		return "+" + d
	}
	switch {
	case len(d) == 11 && d[0] == '8':
		return "+7" + d[1:]
	case len(d) == 11 && d[0] == '7':
		return "+" + d
	case len(d) == 10:
		return "+7" + d
	default:
		return "+" + d
	}
}

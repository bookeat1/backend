package logging

import "strings"

// MaskPhone masks the middle of a phone number, keeping the leading "+" plus
// country/operator code (5 characters) and the last 4 digits, e.g.
// "+77075552233" -> "+7707***2233". A value too short to have a safe,
// reconstructable middle section is masked in full ("***") so a short,
// unexpected value never leaks more than that.
func MaskPhone(phone string) string {
	if phone == "" {
		return ""
	}
	const headLen = 5 // e.g. "+7707"
	const tailLen = 4 // trailing digits kept, e.g. "2233"
	runes := []rune(phone)
	if len(runes) <= headLen+tailLen {
		return "***"
	}
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	return head + "***" + tail
}

// MaskEmail masks the local part of an email address, keeping only its first
// character, e.g. "damir@gmail.com" -> "d***@gmail.com". A value with no '@'
// (not a well-formed email) is masked in full.
func MaskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	local, domain := email[:at], email[at:]
	return local[:1] + "***" + domain
}

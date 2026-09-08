package domain

import (
	"context"
	"strings"
	"time"
)

// ParseStorePlatform normalizes a caller-supplied platform tag into the SUBSET
// of DevicePlatform that has an app store behind it.
//
// It reuses DevicePlatform (see notification.go) rather than declaring a second
// string enum over the same two words: one spelling of "ios" in the domain, not
// two that can drift apart.
//
// PlatformWeb is deliberately refused: a web build has no store to send anybody
// to and no version that a store review could gate, so an update policy for it
// would be a row nothing could ever act on.
//
// Case and surrounding whitespace are forgiven (a client that sends "iOS" means
// iOS); anything else comes back with ok=false so the caller decides what an
// unknown platform means for it, exactly like NormalizeLocale.
func ParseStorePlatform(s string) (DevicePlatform, bool) {
	switch DevicePlatform(strings.ToLower(strings.TrimSpace(s))) {
	case PlatformIOS:
		return PlatformIOS, true
	case PlatformAndroid:
		return PlatformAndroid, true
	default:
		return "", false
	}
}

// AppUpdateAction is what the server tells a launching app to do.
//
// The MODE is decided by the server on purpose: an over-the-air update cannot
// carry a native change, so "ask nicely" and "do not let them continue" must be
// switchable without shipping a new build to the stores.
type AppUpdateAction string

const (
	// AppUpdateNone — the build is fine, show nothing. This is also the answer
	// to every uncertainty: an unknown platform row, an unparsable client
	// version, thresholds that were never configured.
	AppUpdateNone AppUpdateAction = "none"
	// AppUpdateRecommended — a soft, dismissible prompt.
	AppUpdateRecommended AppUpdateAction = "recommended"
	// AppUpdateRequired — a blocking screen: this build is below the minimum
	// the server still supports.
	AppUpdateRequired AppUpdateAction = "required"
)

// appVersionParts is how many numeric components an app version may carry.
// Three is the marketing version everyone writes (1.5.1); the fourth exists so
// a vendor-style "1.5.1.2" parses instead of being rejected as garbage and
// silently downgraded to "do nothing".
const appVersionParts = 4

// appVersionMaxLen bounds the input before any parsing work happens. The field
// arrives from an unauthenticated request, so it gets a length gate first.
const appVersionMaxLen = 64

// AppVersion is a marketing version parsed into ordered numeric components,
// zero-padded to a fixed width. Padding is what makes 1.5 and 1.5.0 the SAME
// version and 1.5.1 strictly greater than both — the comparison this whole
// feature turns on, and the one a string comparison gets wrong twice over
// ("1.5" > "1.10" and "1.5.1" < "1.5" lexicographically).
type AppVersion [appVersionParts]int

// ParseAppVersion reads a dotted numeric version.
//
// Accepted: 1 to 4 dot-separated groups of ASCII digits, optionally prefixed
// with "v", optionally followed by a semver pre-release/build suffix
// ("1.6.0-rc.1", "1.6.0+42") which is PARSED OFF AND IGNORED — this codebase
// never ships a pre-release to a store, and ordering "1.6.0-rc.1" below
// "1.6.0" would only add a rule with no data behind it.
//
// Refused (ok=false): an empty string, a missing or non-numeric group
// ("1..2", "1.", "1.5.x", "abc"), more than four groups, a group of more than
// nine digits (it would not fit an int32 and is not a version anybody ships),
// and anything longer than appVersionMaxLen.
//
// A refusal is NOT an error type: garbage from a client is an expected input
// here and must end in "do nothing", never in a 500 and never in a forced
// update. See MobileAppPolicy.Decide.
func ParseAppVersion(s string) (AppVersion, bool) {
	var v AppVersion
	s = strings.TrimSpace(s)
	if s == "" || len(s) > appVersionMaxLen {
		return v, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	// Cut the semver pre-release / build metadata; the numeric core must
	// survive on its own.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return v, false
	}
	groups := strings.Split(s, ".")
	if len(groups) > appVersionParts {
		return v, false
	}
	for i, g := range groups {
		if g == "" || len(g) > 9 {
			return v, false
		}
		n := 0
		for _, r := range g {
			if r < '0' || r > '9' {
				return v, false
			}
			n = n*10 + int(r-'0')
		}
		v[i] = n
	}
	return v, true
}

// Compare returns -1, 0 or +1 comparing v against other component by component.
func (v AppVersion) Compare(other AppVersion) int {
	for i := 0; i < appVersionParts; i++ {
		switch {
		case v[i] < other[i]:
			return -1
		case v[i] > other[i]:
			return 1
		}
	}
	return 0
}

// Less reports whether v is strictly older than other.
func (v AppVersion) Less(other AppVersion) bool { return v.Compare(other) < 0 }

// MobileAppPolicy is one platform's update policy — one row of
// mobile_app_policies (migration 0103), edited from the admin panel and read by
// every app launch.
//
// Both thresholds are stored as the STRINGS an operator typed, not as parsed
// numbers: what a human put into the panel is what an incident review has to be
// able to read back. An unparsable threshold means "no threshold" (see Decide),
// which is why a typo can only ever fail open.
type MobileAppPolicy struct {
	Platform DevicePlatform
	// MinSupportedVersion — anything strictly below it gets AppUpdateRequired.
	// Empty = nothing is forced.
	MinSupportedVersion string
	// MinRecommendedVersion — anything strictly below it (and at or above
	// MinSupportedVersion) gets AppUpdateRecommended. Empty = nothing is
	// suggested.
	MinRecommendedVersion string
	// StoreURL is where the "Update" button sends the guest, per platform.
	StoreURL string
	// The four texts. Base column = Russian, *I18n = the other languages, the
	// same invariant every other localized field in this schema keeps (see
	// ApplyTranslations). They are served as full {ru,kk,en} objects so the
	// wording can be changed without a store release — that is the whole point
	// of keeping them on the server.
	RecommendedTitle       string
	RecommendedTitleI18n   I18n
	RecommendedMessage     string
	RecommendedMessageI18n I18n
	RequiredTitle          string
	RequiredTitleI18n      I18n
	RequiredMessage        string
	RequiredMessageI18n    I18n
	UpdatedAt              time.Time
}

// Decide answers what a client on clientVersion must be told.
//
// Every uncertain input resolves to AppUpdateNone. That direction is deliberate
// and is the safety property of this feature: a parsing bug, an empty setting
// or a client sending nonsense can only ever fail to prompt an update, never
// lock a paying guest out of a working app.
//
// Required is checked BEFORE recommended, so a misconfiguration where
// MinSupportedVersion is above MinRecommendedVersion still blocks rather than
// merely suggesting. (The write path refuses that combination outright —
// usecase/appversion — but the read path must not depend on that.)
func (p MobileAppPolicy) Decide(clientVersion string) AppUpdateAction {
	v, ok := ParseAppVersion(clientVersion)
	if !ok {
		return AppUpdateNone
	}
	if floor, ok := ParseAppVersion(p.MinSupportedVersion); ok && v.Less(floor) {
		return AppUpdateRequired
	}
	if floor, ok := ParseAppVersion(p.MinRecommendedVersion); ok && v.Less(floor) {
		return AppUpdateRecommended
	}
	return AppUpdateNone
}

// TitleFor returns the title of the given action as a full {ru,kk,en} object,
// every language filled (a missing translation falls back to the Russian base,
// the same rule I18n.Resolve applies). AppUpdateNone carries no text at all and
// comes back nil.
//
// The client picks the language itself, which is why this is a whole map and
// not one resolved string: the answer must not depend on a request header, or
// it could not be cached by URL alone.
func (p MobileAppPolicy) TitleFor(a AppUpdateAction) I18n {
	switch a {
	case AppUpdateRecommended:
		return fullI18n(p.RecommendedTitle, p.RecommendedTitleI18n)
	case AppUpdateRequired:
		return fullI18n(p.RequiredTitle, p.RequiredTitleI18n)
	default:
		return nil
	}
}

// MessageFor is TitleFor for the body text.
func (p MobileAppPolicy) MessageFor(a AppUpdateAction) I18n {
	switch a {
	case AppUpdateRecommended:
		return fullI18n(p.RecommendedMessage, p.RecommendedMessageI18n)
	case AppUpdateRequired:
		return fullI18n(p.RequiredMessage, p.RequiredMessageI18n)
	default:
		return nil
	}
}

// fullI18n expands a (base, translations) pair into a map with an entry for
// EVERY supported locale, falling back to base. Nil when there is no base and
// no translation at all — an object of three empty strings would tell a client
// "there is a text" when there is none.
func fullI18n(base string, i I18n) I18n {
	out := make(I18n, len(SupportedLocales))
	empty := true
	for _, l := range SupportedLocales {
		v := i.Resolve(l, base)
		out[l] = v
		if v != "" {
			empty = false
		}
	}
	if empty {
		return nil
	}
	return out
}

// MobileAppPolicyRepository persists the per-platform update policy.
type MobileAppPolicyRepository interface {
	// Get returns one platform's policy, or ErrNotFound when the row is
	// absent. A missing row is a legitimate state (the feature is simply not
	// configured for that platform) and the caller answers AppUpdateNone.
	Get(ctx context.Context, platform DevicePlatform) (*MobileAppPolicy, error)
	// List returns every configured platform, ordered by platform, for the
	// admin screen.
	List(ctx context.Context) ([]MobileAppPolicy, error)
	// Upsert writes the whole row for one platform, inserting it when absent.
	// The caller has already merged its patch onto the stored value.
	Upsert(ctx context.Context, p *MobileAppPolicy) error
}

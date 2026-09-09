package domain

import "testing"

// TestParseAppVersionAcceptsWhatStoresActuallyShip covers the shapes a real
// build can send. "1.5" and "1.5.0" MUST parse to the same value: the iOS train
// is written as "1.5" in app.config.js while a client library may normalize it
// to "1.5.0", and a gate that told those two apart would force half the
// installs of the same build.
func TestParseAppVersionAcceptsWhatStoresActuallyShip(t *testing.T) {
	cases := []struct {
		in   string
		want AppVersion
	}{
		{"1", AppVersion{1, 0, 0, 0}},
		{"1.5", AppVersion{1, 5, 0, 0}},
		{"1.5.0", AppVersion{1, 5, 0, 0}},
		{"1.5.1", AppVersion{1, 5, 1, 0}},
		{"1.10.0", AppVersion{1, 10, 0, 0}},
		{"1.5.1.2", AppVersion{1, 5, 1, 2}},
		{"  1.5.1  ", AppVersion{1, 5, 1, 0}},
		{"v1.5.1", AppVersion{1, 5, 1, 0}},
		{"1.6.0-rc.1", AppVersion{1, 6, 0, 0}},
		{"1.6.0+42", AppVersion{1, 6, 0, 0}},
		{"01.05", AppVersion{1, 5, 0, 0}},
		{"0.0.0", AppVersion{0, 0, 0, 0}},
	}
	for _, tc := range cases {
		got, ok := ParseAppVersion(tc.in)
		if !ok {
			t.Errorf("ParseAppVersion(%q) refused a version a store can ship", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAppVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseAppVersionRefusesGarbage. Every one of these must come back ok=false
// so the caller can answer "do nothing" — see TestDecideNeverActsOnGarbage,
// which is the property that actually matters to a guest.
func TestParseAppVersionRefusesGarbage(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"v",
		"abc",
		"1.5.x",
		"1..2",
		"1.",
		".1",
		"1.5.1.2.3",
		"-1.5",
		"1,5",
		"1.5 OR 1=1",
		"1234567890.0",
		"<script>alert(1)</script>",
		"1.٥", // Arabic-Indic digit: not ASCII, must not be read as 5
		"999999999999999999999999999999999999999999999999999999999999999999999",
	}
	for _, in := range cases {
		if v, ok := ParseAppVersion(in); ok {
			t.Errorf("ParseAppVersion(%q) accepted garbage as %v", in, v)
		}
	}
}

// TestAppVersionCompareIsNumericNotLexicographic pins the two comparisons a
// string compare gets wrong, which is the entire reason this type exists.
func TestAppVersionCompareIsNumericNotLexicographic(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The task's two named cases.
		{"1.5", "1.5.1", -1},  // string compare says "1.5" < "1.5.1" too, but only by luck of the prefix
		{"1.5.1", "1.5", 1},   // ...and here it would have to agree; the real trap is below
		{"1.10", "1.9", 1},    // string compare says "1.10" < "1.9". It is wrong.
		{"1.9", "1.10", -1},   //
		{"1.5", "1.5.0", 0},   // padding: the same version written two ways
		{"1.5.0", "1.5", 0},   //
		{"2.0", "1.99.99", 1}, //
		{"1.0.0", "1.0.1", -1},
		{"1.5.1", "1.5.1", 0},
	}
	for _, tc := range cases {
		a, ok := ParseAppVersion(tc.a)
		if !ok {
			t.Fatalf("ParseAppVersion(%q) refused", tc.a)
		}
		b, ok := ParseAppVersion(tc.b)
		if !ok {
			t.Fatalf("ParseAppVersion(%q) refused", tc.b)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%q.Compare(%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := a.Less(b); got != (tc.want < 0) {
			t.Errorf("%q.Less(%q) = %v, want %v", tc.a, tc.b, got, tc.want < 0)
		}
	}
}

// policy is a fully configured policy: force below 1.5, suggest below 1.7.
func policy() MobileAppPolicy {
	return MobileAppPolicy{
		Platform:              PlatformIOS,
		MinSupportedVersion:   "1.5",
		MinRecommendedVersion: "1.7",
		StoreURL:              "https://apps.apple.com/app/id1",
		RecommendedTitle:      "Доступно обновление",
		RecommendedTitleI18n:  I18n{"ru": "Доступно обновление", "en": "Update available"},
		RecommendedMessage:    "Обновите приложение",
		RequiredTitle:         "Нужно обновить",
		RequiredTitleI18n:     I18n{"ru": "Нужно обновить", "kk": "Жаңарту қажет", "en": "Update required"},
		RequiredMessage:       "Эта версия больше не поддерживается",
	}
}

// TestDecideThreeOutcomes walks the whole ladder around both thresholds,
// including the boundaries: a version EQUAL to a threshold is on the good side
// of it ("minimum supported" means supported).
func TestDecideThreeOutcomes(t *testing.T) {
	p := policy()
	cases := []struct {
		version string
		want    AppUpdateAction
	}{
		{"1.0", AppUpdateRequired},
		{"1.4.9", AppUpdateRequired},
		{"1.4.99", AppUpdateRequired},
		{"1.5", AppUpdateRecommended},   // exactly the forced floor: supported
		{"1.5.0", AppUpdateRecommended}, // same version, written differently
		{"1.5.1", AppUpdateRecommended},
		{"1.6.9", AppUpdateRecommended},
		{"1.7", AppUpdateNone}, // exactly the recommended floor: nothing to say
		{"1.7.1", AppUpdateNone},
		{"1.10", AppUpdateNone}, // the lexicographic trap, end to end
		{"2.0", AppUpdateNone},
	}
	for _, tc := range cases {
		if got := p.Decide(tc.version); got != tc.want {
			t.Errorf("Decide(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// TestDecideNeverActsOnGarbage is the safety property of the whole feature: a
// client sending nonsense, nothing, or an injection attempt is left alone. It
// is never forced to update on the strength of a string nobody could parse.
func TestDecideNeverActsOnGarbage(t *testing.T) {
	p := policy()
	for _, v := range []string{"", "   ", "abc", "1.5.x", "null", "undefined", "1..2", "-1"} {
		if got := p.Decide(v); got != AppUpdateNone {
			t.Errorf("Decide(%q) = %q, want %q — garbage must never force an update", v, got, AppUpdateNone)
		}
	}
}

// TestDecideWithNoThresholdsConfigured: an unconfigured policy (which is what
// migration 0103 seeds) tells nobody anything, whatever they send.
func TestDecideWithNoThresholdsConfigured(t *testing.T) {
	p := MobileAppPolicy{Platform: PlatformAndroid, StoreURL: "https://play.google.com/store/apps/details?id=x"}
	for _, v := range []string{"0.1", "1.5", "99.0"} {
		if got := p.Decide(v); got != AppUpdateNone {
			t.Errorf("Decide(%q) with no thresholds = %q, want %q", v, got, AppUpdateNone)
		}
	}
}

// TestDecideIgnoresAnUnparsableThreshold. A typo in the panel degrades to "no
// threshold" instead of blocking everyone or blocking nobody at random. The
// write path refuses to store such a value at all (usecase/appversion), so this
// is the second line of defence, for a row written before that check existed.
func TestDecideIgnoresAnUnparsableThreshold(t *testing.T) {
	p := policy()
	p.MinSupportedVersion = "полтора"
	if got := p.Decide("1.0"); got != AppUpdateRecommended {
		t.Errorf("Decide with an unparsable forced floor = %q, want %q (the recommended floor still applies)",
			got, AppUpdateRecommended)
	}
	p.MinRecommendedVersion = "тоже мусор"
	if got := p.Decide("1.0"); got != AppUpdateNone {
		t.Errorf("Decide with both floors unparsable = %q, want %q", got, AppUpdateNone)
	}
}

// TestDecideChecksRequiredFirst: if somebody manages to store a forced floor
// ABOVE the recommended one, the answer is still the blocking one. Merely
// suggesting an update to a build we no longer support would be the wrong way
// round.
func TestDecideChecksRequiredFirst(t *testing.T) {
	p := policy()
	p.MinSupportedVersion, p.MinRecommendedVersion = "1.7", "1.5"
	if got := p.Decide("1.6"); got != AppUpdateRequired {
		t.Errorf("Decide = %q, want %q", got, AppUpdateRequired)
	}
}

// TestTextsComeBackInEveryLanguage. The client picks the language, so the
// server must hand it all three — filling a missing translation from the
// Russian base rather than shipping an empty string a guest would see as a
// blank modal.
func TestTextsComeBackInEveryLanguage(t *testing.T) {
	p := policy()

	title := p.TitleFor(AppUpdateRecommended)
	for _, l := range SupportedLocales {
		if title[l] == "" {
			t.Errorf("recommended title has no %s text: %v", l, title)
		}
	}
	if title["kk"] != "Доступно обновление" {
		t.Errorf("a missing kk translation must fall back to the Russian base, got %q", title["kk"])
	}
	if title["en"] != "Update available" {
		t.Errorf("en title = %q, want the stored translation", title["en"])
	}

	if got := p.TitleFor(AppUpdateRequired)["kk"]; got != "Жаңарту қажет" {
		t.Errorf("required kk title = %q, want the stored translation", got)
	}
	if got := p.MessageFor(AppUpdateRequired)["en"]; got != "Эта версия больше не поддерживается" {
		t.Errorf("a message with no en translation must fall back to Russian, got %q", got)
	}
}

// TestNoTextsForTheNoneAction: "do nothing" carries no wording at all, so a
// client cannot accidentally render a prompt for it.
func TestNoTextsForTheNoneAction(t *testing.T) {
	p := policy()
	if got := p.TitleFor(AppUpdateNone); got != nil {
		t.Errorf("TitleFor(none) = %v, want nil", got)
	}
	if got := p.MessageFor(AppUpdateNone); got != nil {
		t.Errorf("MessageFor(none) = %v, want nil", got)
	}
}

// TestEmptyWordingIsNilNotThreeEmptyStrings: an object of three empty strings
// would tell a client "there is a text" when there is none.
func TestEmptyWordingIsNilNotThreeEmptyStrings(t *testing.T) {
	p := MobileAppPolicy{MinSupportedVersion: "2.0"}
	if got := p.TitleFor(AppUpdateRequired); got != nil {
		t.Errorf("TitleFor with no wording stored = %v, want nil", got)
	}
}

// TestParseStorePlatform: the two stores, forgiving about case, refusing
// everything else — including web, which has no store to send anyone to.
func TestParseStorePlatform(t *testing.T) {
	for _, in := range []string{"ios", "iOS", "IOS", " ios "} {
		if got, ok := ParseStorePlatform(in); !ok || got != PlatformIOS {
			t.Errorf("ParseStorePlatform(%q) = %q, %v; want ios, true", in, got, ok)
		}
	}
	for _, in := range []string{"android", "Android", "ANDROID"} {
		if got, ok := ParseStorePlatform(in); !ok || got != PlatformAndroid {
			t.Errorf("ParseStorePlatform(%q) = %q, %v; want android, true", in, got, ok)
		}
	}
	for _, in := range []string{"", "web", "windows", "ios;android", "и"} {
		if got, ok := ParseStorePlatform(in); ok {
			t.Errorf("ParseStorePlatform(%q) accepted an unknown platform as %q", in, got)
		}
	}
}

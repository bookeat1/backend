package domain

import (
	"errors"
	"testing"
	"time"
)

// TestNormalizeVenueTimezoneRefusesWhatMoneyCannotUse pins the write-side rule.
// Every rejected value below is one a human or an importer really produces, and
// every one of them would be accepted by the naive check (time.LoadLocation
// alone) that this replaces.
func TestNormalizeVenueTimezoneRefusesWhatMoneyCannotUse(t *testing.T) {
	bad := []struct {
		name string
		why  string
	}{
		{"", "an empty name loads as UTC without an error, so nothing downstream would ever complain"},
		{"   ", "whitespace trims to empty, same trap"},
		{"Local", "resolves to the SERVER's zone: the venue's payout day would follow the deployment host"},
		{"KZT", "a currency-shaped abbreviation, not a zone"},
		{"+06", "an offset is not a zone: it cannot express a future rule change"},
		{"Mars/Olympus", "well-formed but unknown to tzdata"},
		{"Asia/Almatyy", "a typo one letter away from the right zone"},
	}
	for _, c := range bad {
		got, err := NormalizeVenueTimezone(c.name)
		if err == nil {
			t.Fatalf("timezone %q was accepted (%s); normalized to %q", c.name, c.why, got)
		}
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("timezone %q: error does not wrap ErrValidation: %v", c.name, err)
		}
		if code, ok := CodeOf(err); !ok || code != CodeVenueTimezoneInvalid {
			t.Fatalf("timezone %q: code = %q (ok=%v), want %q", c.name, code, ok, CodeVenueTimezoneInvalid)
		}
	}

	good := map[string]string{
		"Asia/Almaty":       "Asia/Almaty",
		"UTC":               "UTC",
		"  Europe/Lisbon":   "Europe/Lisbon", // trimmed, not rejected
		"America/Sao_Paulo": "America/Sao_Paulo",
	}
	for in, want := range good {
		got, err := NormalizeVenueTimezone(in)
		if err != nil {
			t.Fatalf("timezone %q was rejected: %v", in, err)
		}
		if got != want {
			t.Fatalf("timezone %q normalized to %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeVenueTimezoneRefusesAbbreviationsThatDoLoad is the subtle half.
// "EST" and friends exist in tzdata, so time.LoadLocation accepts them — and a
// venue stored that way sits on standard time all year: the zone reports the
// same offset in January and in July, so half the year every opening hour,
// every deadline and every payout day is an hour out, silently.
func TestNormalizeVenueTimezoneRefusesAbbreviationsThatDoLoad(t *testing.T) {
	for _, name := range []string{"EST", "MST", "EST5EDT", "CET"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue // this host's tzdata has no such entry; nothing to guard
		}
		if _, err := NormalizeVenueTimezone(name); err == nil {
			t.Fatalf("timezone %q was accepted even though it is a legacy abbreviation entry (%s)", name, loc)
		}
	}

	// The reason, made observable: a fixed-offset entry does not move across a
	// DST transition, while the proper IANA zone for the same place does.
	est, err := time.LoadLocation("EST")
	if err != nil {
		t.Skip("no EST entry in this host's tzdata")
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no America/New_York entry in this host's tzdata")
	}
	winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	_, estWinter := winter.In(est).Zone()
	_, estSummer := summer.In(est).Zone()
	_, nyWinter := winter.In(newYork).Zone()
	_, nySummer := summer.In(newYork).Zone()
	if estWinter != estSummer {
		t.Fatalf("assumption broken: EST is expected to be fixed-offset, got %d and %d", estWinter, estSummer)
	}
	if nyWinter == nySummer {
		t.Fatalf("assumption broken: America/New_York is expected to observe DST, got %d both times", nyWinter)
	}
	if estSummer == nySummer {
		t.Fatalf("assumption broken: EST and America/New_York should differ in summer")
	}
}

// TestLoadVenueLocationDoesNotFallBack is the read-side rule: a caller about to
// decide a payout period or a deposit gets an error, never a substituted zone.
// A silent substitution is what made the DST hours bug invisible — the wrong
// answer looked exactly like a right one.
func TestLoadVenueLocationDoesNotFallBack(t *testing.T) {
	for _, name := range []string{"", "KZT", "Local", "EST"} {
		if name == "EST" {
			if _, err := time.LoadLocation("EST"); err != nil {
				continue
			}
		}
		loc, err := LoadVenueLocation(name)
		if err == nil {
			t.Fatalf("LoadVenueLocation(%q) returned %s instead of refusing", name, loc)
		}
		if loc != nil {
			t.Fatalf("LoadVenueLocation(%q) returned both an error and a location %s", name, loc)
		}
	}

	loc, err := LoadVenueLocation("Europe/Lisbon")
	if err != nil {
		t.Fatalf("LoadVenueLocation(Europe/Lisbon): %v", err)
	}
	// A real zone, not a fixed offset: Lisbon is UTC+0 in winter and UTC+1 in
	// summer, and a venue's wall clock has to follow that.
	_, winter := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	_, summer := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(loc).Zone()
	if winter == summer {
		t.Fatalf("Europe/Lisbon reported the same offset in winter and summer (%d): tzdata on this host looks wrong", winter)
	}
}

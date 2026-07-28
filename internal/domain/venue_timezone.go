package domain

import (
	"fmt"
	"strings"
	"time"
)

// A venue's timezone is a MONEY boundary, not a display preference. It decides
// which calendar day a payout settles (usecase/payouts.DailyRunner), which
// local date a booking instant falls on for a paid special day
// (ScheduleOverrideRepository.GetForBookingInstant), and which wall-clock hours
// the venue is sold at (OpeningWindow). A wrong zone moves money to the wrong
// day and trading hours to the wrong hour, and — as the DST bug of 2026-07-27
// showed — it does so invisibly, because the storefront and the engine read the
// same wrong value and agree with each other.
//
// So the zone is validated where it is WRITTEN, once, in the domain, and every
// reader that has money or an obligation riding on it refuses to guess when the
// stored value is unusable.

// venueTimezoneUTC is the one abbreviation-shaped name a venue may legitimately
// carry. Everything else must be an IANA "Area/Location" name.
const venueTimezoneUTC = "UTC"

// NormalizeVenueTimezone validates an IANA timezone name for a venue and
// returns it trimmed, ready to store.
//
// What it refuses, and why each refusal is not pedantry:
//
//   - empty / whitespace — time.LoadLocation("") silently returns UTC, so an
//     empty string does not fail anywhere downstream: it quietly settles a
//     Kazakh venue on UTC days. A venue with no zone of its own must store NULL
//     (no override, platform fallback applies), never "".
//   - "Local" — loads fine and resolves to the SERVER's zone. That makes a
//     venue's payout day an accident of where the container runs, and it would
//     change under the venue's feet on a redeploy to another host.
//   - an abbreviation-shaped name ("EST", "MET", "CST6CDT", …) — some of these
//     do exist in tzdata and load without error, which is exactly what makes
//     them dangerous: they are fixed-offset or legacy compatibility entries, so
//     a venue stored as "EST" keeps standard time straight through the summer
//     and loses an hour of trading for half the year. The name a picker offers
//     is always "Area/Location"; "UTC" is the single deliberate exception.
//   - anything time.LoadLocation rejects ("KZT", "+06", "Mars/Olympus") — the
//     name is not a zone at all on this host.
func NormalizeVenueTimezone(name string) (string, error) {
	tz := strings.TrimSpace(name)
	if tz == "" {
		return "", WithCode(CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: timezone must not be empty; leave it unset to use the platform default", ErrValidation))
	}
	if tz == "Local" {
		return "", WithCode(CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: timezone %q means the server's own zone, which is not a property of the venue", ErrValidation, tz))
	}
	if tz != venueTimezoneUTC && !strings.Contains(tz, "/") {
		return "", WithCode(CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: timezone %q is not an IANA name; use the \"Area/Location\" form, e.g. \"Asia/Almaty\"", ErrValidation, tz))
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "", WithCode(CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: unknown timezone %q", ErrValidation, tz))
	}
	return tz, nil
}

// LoadVenueLocation resolves a stored venue timezone to a *time.Location under
// the same rules NormalizeVenueTimezone enforces on write.
//
// It returns an error rather than a fallback ON PURPOSE. A caller that is about
// to decide a payout period, a deposit or a deadline must be able to tell "this
// venue has no zone of its own" (empty — the platform default legitimately
// applies) from "this venue's stored zone is unusable" (a data fault). Papering
// the second case over with a default is how an hour, and then a day, goes
// missing without anybody being told.
func LoadVenueLocation(name string) (*time.Location, error) {
	tz, err := NormalizeVenueTimezone(name)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Unreachable through NormalizeVenueTimezone, kept so a future change to
		// the validation cannot turn into a nil location.
		return nil, WithCode(CodeVenueTimezoneInvalid,
			fmt.Errorf("%w: unknown timezone %q", ErrValidation, tz))
	}
	return loc, nil
}

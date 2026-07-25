package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrWorkingHoursUnparsed means the free-text opening hours of the OLD system
// could not be understood well enough to be turned into a weekly schedule.
//
// The parser deliberately REFUSES instead of guessing: a venue whose stored
// hours are wrong takes bookings while it is closed, which is strictly worse
// than a venue with no hours at all (the latter simply shows no slots and is
// visible in the fill report, so a human can enter the hours by hand).
var ErrWorkingHoursUnparsed = errors.New("opening hours text not understood")

// DayHours is one weekday of a parsed schedule. Times are "HH:MM" in the
// venue's local timezone, empty when the day is closed.
//
// A close time that is not after the open time means the venue works past
// midnight (18:00–06:00); it is stored verbatim, exactly as
// usecase/bookings.openingWindow expects — that function rolls such a close
// time into the next day itself. 24:00 is normalised to "00:00" for the same
// reason: it then reads as "not after open" and rolls over, and unlike "24:00"
// it also passes the admin panel's HH:MM validation, so a venue can later edit
// the imported row without first having to fix it.
type DayHours struct {
	IsOpen    bool
	OpenTime  string
	CloseTime string
}

// ParsedWorkingHours is a full week derived from one legacy free-text string.
type ParsedWorkingHours struct {
	// Days is indexed by time.Weekday (0 = Sunday), the same convention the
	// restaurant_working_hours.day_of_week column and availability use.
	Days [7]DayHours
	// AssumedClosed lists the weekdays the text never mentioned. They are
	// stored as closed — the safe direction (a missed booking beats a booking
	// while shut) — but they are a judgement call, so the caller reports them
	// for human review instead of hiding them.
	AssumedClosed []time.Weekday
}

// ToWorkingHours renders the parsed week as persistence-shaped rows, one per
// weekday. IDs are left zero: the repository assigns them.
func (p ParsedWorkingHours) ToWorkingHours(restaurantID uuid.UUID) []WorkingHours {
	out := make([]WorkingHours, 0, len(p.Days))
	for dow, d := range p.Days {
		row := WorkingHours{RestaurantID: restaurantID, DayOfWeek: dow, IsOpen: d.IsOpen}
		if d.IsOpen {
			open, close_ := d.OpenTime, d.CloseTime
			row.OpenTime, row.CloseTime = &open, &close_
		}
		out = append(out, row)
	}
	return out
}

// ---- parsing ---------------------------------------------------------------

// Day abbreviations in the order the Russian week is written (Пн first). The
// index is NOT time.Weekday — see ruDayToWeekday.
var ruDayTokens = [7]string{"пн", "вт", "ср", "чт", "пт", "сб", "вс"}

// ruDayToWeekday maps the Russian week position (0 = Пн) to time.Weekday
// (0 = Sunday), which is what the database column stores.
func ruDayToWeekday(ru int) time.Weekday { return time.Weekday((ru + 1) % 7) }

const (
	dayToken   = `(?:пн|вт|ср|чт|пт|сб|вс)`
	dayItem    = dayToken + `(?:\s*-\s*` + dayToken + `)?`
	daySpec    = dayItem + `(?:\s*,\s*` + dayItem + `)*`
	closedWord = `(?:выходной|выходные)`
	clock      = `(\d{1,2}):(\d{2})`
)

// segmentRe matches ONE "<days> <value>" segment of a normalised string, where
// value is either a time range or "выходной". The separator between the days
// and the value may be a colon, a space, or nothing; the range may be written
// either as "10:00-23:00" or as "с 18:00 до 06:00".
var segmentRe = regexp.MustCompile(
	`(ежедневно|` + daySpec + `)\s*:?\s*` +
		`(?:(` + closedWord + `)|(?:с\s+)?` + clock + `\s*(?:-|до)\s*` + clock + `)`)

// betweenSegmentsRe is what is allowed to sit BETWEEN two matched segments (and
// before the first / after the last). Anything else — a stray word, a lone
// "до 02:00", a dash that belongs to nothing — makes the whole string a refusal
// rather than a partial, half-understood schedule.
var betweenSegmentsRe = regexp.MustCompile(`^[\s,.]*$`)

var spaceRe = regexp.MustCompile(`\s+`)

// punctReplacer folds the punctuation variants the live data actually contains
// into one canonical form: every dash-like rune becomes "-", every bullet /
// semicolon becomes ",", every exotic space becomes a plain space.
var punctReplacer = strings.NewReplacer(
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-",
	"―", "-", "−", "-", "﹘", "-", "－", "-",
	"·", ",", "•", ",", ";", ",",
	"\u00a0", " ", "\u202f", " ", "\u2009", " ", "\ufeff", "",
)

// latinHomoglyphs repairs Latin letters typed inside Cyrillic words — the live
// data contains "Вc" with a Latin "c" twice. Nothing in this grammar is
// legitimately Latin (the vocabulary is day abbreviations, "ежедневно",
// "выходной", "с", "до" and digits), so folding the look-alikes is a
// normalisation, not a guess.
var latinHomoglyphs = strings.NewReplacer(
	"a", "а", "b", "в", "c", "с", "e", "е", "h", "н", "k", "к",
	"m", "м", "o", "о", "p", "р", "t", "т", "x", "х", "y", "у",
)

// normalizeHoursText lower-cases, folds punctuation and homoglyphs and collapses
// whitespace, so the grammar above has exactly one shape to match.
func normalizeHoursText(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = punctReplacer.Replace(s)
	s = latinHomoglyphs.Replace(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// ParseWorkingHoursText turns the old system's free-text opening hours into a
// full week. It returns an error wrapping ErrWorkingHoursUnparsed — with a
// human-readable reason for the fill report — whenever the text is empty, is
// not fully consumed by the grammar, contradicts itself, or carries an
// impossible clock value.
func ParseWorkingHoursText(raw string) (ParsedWorkingHours, error) {
	text := normalizeHoursText(raw)
	if text == "" {
		return ParsedWorkingHours{}, fmt.Errorf("%w: text is empty", ErrWorkingHoursUnparsed)
	}

	matches := segmentRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return ParsedWorkingHours{}, fmt.Errorf("%w: no <days> <hours> segment found in %q",
			ErrWorkingHoursUnparsed, text)
	}
	// Every character outside the matched segments must be pure separator.
	// This is the "refuse rather than guess" guarantee: an unrecognised piece
	// of the string can never be silently dropped.
	prev := 0
	for _, m := range matches {
		if gap := text[prev:m[0]]; !betweenSegmentsRe.MatchString(gap) {
			return ParsedWorkingHours{}, fmt.Errorf("%w: unrecognised fragment %q in %q",
				ErrWorkingHoursUnparsed, strings.TrimSpace(gap), text)
		}
		prev = m[1]
	}
	if tail := text[prev:]; !betweenSegmentsRe.MatchString(tail) {
		return ParsedWorkingHours{}, fmt.Errorf("%w: unrecognised fragment %q in %q",
			ErrWorkingHoursUnparsed, strings.TrimSpace(tail), text)
	}

	var (
		out      ParsedWorkingHours
		assigned [7]bool
	)
	for _, m := range matches {
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return text[m[2*i]:m[2*i+1]]
		}
		days, err := parseDaySpec(group(1))
		if err != nil {
			return ParsedWorkingHours{}, err
		}
		var value DayHours
		if group(2) == "" { // not "выходной" -> a time range
			value, err = parseHoursRange(group(3), group(4), group(5), group(6))
			if err != nil {
				return ParsedWorkingHours{}, err
			}
		}
		for _, dow := range days {
			// Two segments naming the same day must agree. A contradiction
			// ("Пн-Вс 12:00-01:00, Пт-Сб до 02:00") is exactly the case where
			// guessing a winner would silently invent hours.
			if assigned[dow] && out.Days[dow] != value {
				return ParsedWorkingHours{}, fmt.Errorf(
					"%w: weekday %d is given two different schedules in %q",
					ErrWorkingHoursUnparsed, int(dow), text)
			}
			assigned[dow] = true
			out.Days[dow] = value
		}
	}

	for dow := range out.Days {
		if !assigned[dow] {
			out.AssumedClosed = append(out.AssumedClosed, time.Weekday(dow))
		}
	}
	return out, nil
}

// parseDaySpec expands "ежедневно", a day list, a day range, or a mix of both
// ("пн-чт, вс") into time.Weekday values. Ranges wrap around the Russian week,
// so "вс-чт" is Вс,Пн,Вт,Ср,Чт.
func parseDaySpec(spec string) ([]time.Weekday, error) {
	spec = strings.TrimSpace(spec)
	if spec == "ежедневно" {
		out := make([]time.Weekday, 0, 7)
		for ru := 0; ru < 7; ru++ {
			out = append(out, ruDayToWeekday(ru))
		}
		return out, nil
	}
	var out []time.Weekday
	for _, item := range strings.Split(spec, ",") {
		bounds := strings.Split(item, "-")
		from, err := ruDayIndex(bounds[0])
		if err != nil {
			return nil, err
		}
		if len(bounds) == 1 {
			out = append(out, ruDayToWeekday(from))
			continue
		}
		to, err := ruDayIndex(bounds[1])
		if err != nil {
			return nil, err
		}
		for i := from; ; i = (i + 1) % 7 {
			out = append(out, ruDayToWeekday(i))
			if i == to {
				break
			}
		}
	}
	return out, nil
}

func ruDayIndex(token string) (int, error) {
	token = strings.TrimSpace(token)
	for i, d := range ruDayTokens {
		if token == d {
			return i, nil
		}
	}
	// Unreachable through segmentRe (the grammar only matches known tokens);
	// kept so parseDaySpec is safe to call on its own.
	return 0, fmt.Errorf("%w: unknown weekday %q", ErrWorkingHoursUnparsed, token)
}

// parseHoursRange validates one "HH:MM - HH:MM" pair.
//
//   - 24:00 is accepted as an END of day only and normalised to "00:00" (see
//     DayHours).
//   - open == close is REFUSED: "00:00-00:00" could mean round-the-clock or
//     closed, and both readings are a guess.
//   - close < open is fine and kept verbatim — that is the past-midnight case
//     the availability code rolls over by itself.
func parseHoursRange(openH, openM, closeH, closeM string) (DayHours, error) {
	open, err := clockValue(openH, openM, false)
	if err != nil {
		return DayHours{}, err
	}
	close_, err := clockValue(closeH, closeM, true)
	if err != nil {
		return DayHours{}, err
	}
	if open == close_ {
		return DayHours{}, fmt.Errorf("%w: open and close time are equal (%s) — round-the-clock or closed cannot be told apart",
			ErrWorkingHoursUnparsed, open)
	}
	return DayHours{IsOpen: true, OpenTime: open, CloseTime: close_}, nil
}

func clockValue(hh, mm string, isEnd bool) (string, error) {
	h, err := strconv.Atoi(hh)
	if err != nil {
		return "", fmt.Errorf("%w: bad hour %q", ErrWorkingHoursUnparsed, hh)
	}
	m, err := strconv.Atoi(mm)
	if err != nil {
		return "", fmt.Errorf("%w: bad minute %q", ErrWorkingHoursUnparsed, mm)
	}
	if m > 59 {
		return "", fmt.Errorf("%w: bad minute %02d", ErrWorkingHoursUnparsed, m)
	}
	if h == 24 && isEnd && m == 0 {
		return "00:00", nil // end of day; rolls over in openingWindow
	}
	if h > 23 {
		return "", fmt.Errorf("%w: bad hour %02d", ErrWorkingHoursUnparsed, h)
	}
	return fmt.Sprintf("%02d:%02d", h, m), nil
}

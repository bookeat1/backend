package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// week is a compact expected schedule: one entry per weekday, "" meaning
// closed, "10:00-23:00" meaning open. Indexed by time.Weekday (0 = Sunday) —
// the same convention the database column uses.
type week [7]string

func (w week) String() string { return strings.Join(w[:], " | ") }

func actual(p domain.ParsedWorkingHours) week {
	var got week
	for i, d := range p.Days {
		if d.IsOpen {
			got[i] = d.OpenTime + "-" + d.CloseTime
		}
	}
	return got
}

// TestParseWorkingHoursTextRealFormats covers, verbatim, the strings that exist
// in the live legacy database (read-only dump taken 2026-07-25) plus the shapes
// they are built from. If a format here stops parsing, venues stop being
// bookable — so every sample is pinned exactly as stored, spaces included.
func TestParseWorkingHoursTextRealFormats(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		want          week
		assumedClosed []time.Weekday
	}{
		{
			name: "en dash, whole week",
			in:   "Пн–Вс 10:00–23:00",
			want: week{"10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-23:00"},
		},
		{
			name: "hyphen with colon",
			in:   "Пн-Вс: 07:00-22:00",
			want: week{"07:00-22:00", "07:00-22:00", "07:00-22:00", "07:00-22:00", "07:00-22:00", "07:00-22:00", "07:00-22:00"},
		},
		{
			name: "ежедневно, с/до wording, padded, past midnight",
			in:   " Ежедневно с 18:00 до 06:00  ",
			want: week{"18:00-06:00", "18:00-06:00", "18:00-06:00", "18:00-06:00", "18:00-06:00", "18:00-06:00", "18:00-06:00"},
		},
		{
			name: "range plus list in one spec, two segments",
			in:   "Пн–Чт, Вс: 12:00–02:00 Пт–Сб: 12:00–03:00  ",
			// Вс=0, Пн..Чт=1..4 -> 12:00-02:00; Пт=5, Сб=6 -> 12:00-03:00
			want: week{"12:00-02:00", "12:00-02:00", "12:00-02:00", "12:00-02:00", "12:00-02:00", "12:00-03:00", "12:00-03:00"},
		},
		{
			name: "range wrapping over Sunday",
			in:   "Вс-Чт: 11:00-00:00 Пт - Сб: 11:00-01:00 ",
			want: week{"11:00-00:00", "11:00-00:00", "11:00-00:00", "11:00-00:00", "11:00-00:00", "11:00-01:00", "11:00-01:00"},
		},
		{
			name: "day list with em dash times, two unmentioned days",
			in:   "Пн, Чт, Вс 11:00 — 22:00  Пт - Сб 11:00 — 01:00",
			// Вт (2) and Ср (3) are never mentioned -> stored closed and flagged.
			want:          week{"11:00-22:00", "11:00-22:00", "", "", "11:00-22:00", "11:00-01:00", "11:00-01:00"},
			assumedClosed: []time.Weekday{time.Tuesday, time.Wednesday},
		},
		{
			name: "24:00 end of day is normalised to 00:00",
			in:   "Пн-Вс: 09:00-24:00",
			want: week{"09:00-00:00", "09:00-00:00", "09:00-00:00", "09:00-00:00", "09:00-00:00", "09:00-00:00", "09:00-00:00"},
		},
		{
			name: "closed days, mixed ranges, four comma-separated segments",
			in:   "Пн — Вт Выходной, Ср — Чт: 17:00–03:00, Пт — Сб: 17:00–05:00, Вс: 17:00–03:00",
			want: week{"17:00-03:00", "", "", "17:00-03:00", "17:00-03:00", "17:00-05:00", "17:00-05:00"},
		},
		{
			name: "middle dot separator between segments",
			in:   "пн–чт 10:00–23:00 · пт–вс 10:00–00:00",
			want: week{"10:00-00:00", "10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-23:00", "10:00-00:00", "10:00-00:00"},
		},
		{
			name: "latin homoglyph in Вc",
			in:   "Пн — Чт: 12:00–01:00, Пт — Сб: 12:00–03:00, Вc: 12:00–01:00",
			want: week{"12:00-01:00", "12:00-01:00", "12:00-01:00", "12:00-01:00", "12:00-01:00", "12:00-03:00", "12:00-03:00"},
		},
		{
			name: "half-hour close time",
			in:   "Пн-Вс: 08:00-23:30",
			want: week{"08:00-23:30", "08:00-23:30", "08:00-23:30", "08:00-23:30", "08:00-23:30", "08:00-23:30", "08:00-23:30"},
		},
		{
			name:          "bare day list, four unmentioned days",
			in:            "Чт, Пт, Сб 19:00-24:00",
			want:          week{"", "", "", "", "19:00-00:00", "19:00-00:00", "19:00-00:00"},
			assumedClosed: []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParseWorkingHoursText(tc.in)
			if err != nil {
				t.Fatalf("ParseWorkingHoursText(%q) = error %v, want a schedule", tc.in, err)
			}
			if a := actual(got); a != tc.want {
				t.Fatalf("schedule mismatch\n got: %s\nwant: %s", a, tc.want)
			}
			if len(got.AssumedClosed) != len(tc.assumedClosed) {
				t.Fatalf("assumed-closed days = %v, want %v", got.AssumedClosed, tc.assumedClosed)
			}
			for i := range tc.assumedClosed {
				if got.AssumedClosed[i] != tc.assumedClosed[i] {
					t.Fatalf("assumed-closed days = %v, want %v", got.AssumedClosed, tc.assumedClosed)
				}
			}
		})
	}
}

// TestParseWorkingHoursTextRefuses is the anti-guessing guard. Every string here
// MUST be refused: storing a wrong window makes a venue take bookings while it
// is shut, which is worse than storing nothing.
func TestParseWorkingHoursTextRefuses(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "blank", in: "   "},
		{name: "free prose", in: "Работаем когда захотим"},
		{
			// Real production string: the second half has no opening time and
			// contradicts the first half. Both readings are guesses.
			name: "contradicting half-specified segment",
			in:   "Пн-Вс 12:00–01:00, Пт–Сб до 02:00",
		},
		{name: "trailing unrecognised words", in: "Пн-Вс 10:00-23:00 кроме праздников"},
		{name: "leading unrecognised words", in: "летом Пн-Вс 10:00-23:00"},
		{name: "same day two different schedules", in: "Пн-Вт 10:00-23:00, Вт 11:00-22:00"},
		{name: "open equals close cannot be told from round-the-clock", in: "Пн-Вс 00:00-00:00"},
		{name: "impossible hour", in: "Пн-Вс 25:00-26:00"},
		{name: "impossible minute", in: "Пн-Вс 10:70-23:00"},
		{name: "24:00 as an opening time", in: "Пн-Вс 24:00-06:00"},
		{name: "no times at all", in: "Пн-Вс"},
		{name: "hours without days", in: "10:00-23:00"},
		{name: "round the clock wording is not supported", in: "Ежедневно круглосуточно"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParseWorkingHoursText(tc.in)
			if err == nil {
				t.Fatalf("ParseWorkingHoursText(%q) parsed to %v, want a refusal", tc.in, actual(got))
			}
			if !errors.Is(err, domain.ErrWorkingHoursUnparsed) {
				t.Fatalf("error = %v, want it to wrap ErrWorkingHoursUnparsed", err)
			}
			if !strings.Contains(err.Error(), ":") {
				t.Fatalf("refusal %q carries no reason for the fill report", err)
			}
		})
	}
}

func TestParsedWorkingHoursToRows(t *testing.T) {
	rid := uuid.New()
	p, err := domain.ParseWorkingHoursText("Пн — Вт Выходной, Ср — Вс: 17:00–03:00")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rows := p.ToWorkingHours(rid)
	if len(rows) != 7 {
		t.Fatalf("got %d rows, want one per weekday", len(rows))
	}
	for dow, r := range rows {
		if r.RestaurantID != rid || r.DayOfWeek != dow {
			t.Fatalf("row %d = %+v, want restaurant %s and day_of_week %d", dow, r, rid, dow)
		}
		// A closed day must be stored as a real closed row (is_open=false with
		// NULL times), not skipped: a missing row and a closed row read the same
		// in availability today, but only the stored row survives a later
		// partial edit and shows up in the admin panel.
		if r.IsOpen {
			if r.OpenTime == nil || r.CloseTime == nil {
				t.Fatalf("row %d is open but has nil times", dow)
			}
		} else if r.OpenTime != nil || r.CloseTime != nil {
			t.Fatalf("row %d is closed but carries times", dow)
		}
	}
	if rows[time.Monday].IsOpen || rows[time.Tuesday].IsOpen {
		t.Fatalf("Пн/Вт are Выходной, they must be stored closed: %+v %+v", rows[1], rows[2])
	}
	if !rows[time.Sunday].IsOpen || *rows[time.Sunday].OpenTime != "17:00" {
		t.Fatalf("Вс must be open 17:00: %+v", rows[0])
	}
}

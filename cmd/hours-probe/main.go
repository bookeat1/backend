// Command hours-probe runs the legacy opening-hours parser
// (domain.ParseWorkingHoursText) over a list of venues WITHOUT touching any
// database, so the result of a fill can be reviewed before the sync writes
// anything — and so the venues that must be filled in by hand in the admin panel
// can be listed at any time.
//
// Input: one venue per line on stdin, "<id>|<free text>" (exactly the shape of
//
//	psql -tAc "select id, coalesce(opening_hours,'') from restaurants"
//
// Output: one line per venue (OK / REFUSED, with the parsed week or the reason)
// and a summary. Exit code 0 always — a refusal is data, not a tool failure.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"backend-core/internal/domain"
)

func main() {
	var parsed, refused, assumed int
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		id, text, _ := strings.Cut(line, "|")

		week, err := domain.ParseWorkingHoursText(text)
		if err != nil {
			refused++
			fmt.Printf("REFUSED %s  %q\n        %v\n", id, text, err)
			continue
		}
		parsed++
		fmt.Printf("OK      %s  %q\n        %s\n", id, text, formatWeek(week))
		if len(week.AssumedClosed) > 0 {
			assumed++
			fmt.Printf("        note: not mentioned, stored closed: %s\n", formatDays(week.AssumedClosed))
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(1)
	}
	fmt.Printf("\ntotal=%d parsed=%d refused=%d (of parsed, %d have weekdays the text never mentioned)\n",
		parsed+refused, parsed, refused, assumed)
}

// weekdayNames is indexed by time.Weekday (0 = Sunday), like the stored rows.
var weekdayNames = [7]string{"Вс", "Пн", "Вт", "Ср", "Чт", "Пт", "Сб"}

func formatWeek(w domain.ParsedWorkingHours) string {
	parts := make([]string, 0, 7)
	for dow, d := range w.Days {
		if d.IsOpen {
			parts = append(parts, fmt.Sprintf("%s %s-%s", weekdayNames[dow], d.OpenTime, d.CloseTime))
			continue
		}
		parts = append(parts, weekdayNames[dow]+" закрыт")
	}
	return strings.Join(parts, ", ")
}

func formatDays[T ~int](days []T) string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, weekdayNames[int(d)%7])
	}
	return strings.Join(out, ",")
}

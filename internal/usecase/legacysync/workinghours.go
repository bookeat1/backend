package legacysync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// The old system never had a working-hours table: a venue's opening times live
// there as ONE free-text line (restaurants.opening_hours, e.g. "Пн–Вс 10:00–23:00").
// The new backend needs a row per weekday — usecase/bookings.openingWindow
// produces no slot at all for a weekday that has no row, so a synced venue with
// no working hours can never be booked.
//
// This pass closes that gap: it reads the already-synced free text from the NEW
// restaurants table (so it is a plain backfill that also works for venues whose
// restaurant row was synced long ago and no longer moves the cursor), parses it
// with domain.ParseWorkingHoursText and writes the seven rows.
//
// Two rules make it safe to re-run forever:
//
//  1. The venue's own edit ALWAYS wins. Ownership is tracked in
//     legacy_working_hours_import: for every venue we touched we store what we
//     imported (source_text) and a fingerprint of the seven rows we wrote. We
//     overwrite only when the rows still match that fingerprint exactly — i.e.
//     nobody has edited them since. Rows that exist without an import record
//     (entered by hand in the admin panel) are never touched, and once a venue
//     is marked venue_owned it stays untouchable.
//  2. Nothing is written while the legacy text is unchanged: the candidate
//     query only returns venues whose opening_hours differs from the text of
//     their last import attempt — including refused ones, which are therefore
//     reported once and retried only if the venue rewrites its legacy string.

// Import statuses persisted in legacy_working_hours_import.status.
const (
	// HoursImportFilled: the seven rows were written by this sync and, unless
	// edited, may be refreshed by it later.
	HoursImportFilled = "filled"
	// HoursImportVenueOwned: hours exist that this sync did not write (or no
	// longer matches). Hands off, permanently.
	HoursImportVenueOwned = "venue_owned"
	// HoursImportRefused: the legacy text could not be parsed. Reported for
	// manual entry; retried only when the legacy text changes.
	HoursImportRefused = "refused"
)

// WorkingHoursCandidate is a venue whose legacy free text has not yet been
// reconciled with what this sync last imported.
type WorkingHoursCandidate struct {
	RestaurantID uuid.UUID
	Name         string
	SourceText   string // restaurants.opening_hours, verbatim
}

// WorkingHoursImport is the venue's last import attempt, as stored.
type WorkingHoursImport struct {
	SourceText  string
	Status      string
	Fingerprint string // set only for HoursImportFilled
}

// WorkingHoursState is everything the ownership decision needs, read INSIDE the
// fill transaction so the check and the write cannot straddle an admin edit.
type WorkingHoursState struct {
	SourceText string // fresh restaurants.opening_hours
	Rows       []domain.WorkingHours
	Import     *WorkingHoursImport // nil when this venue was never attempted
}

// WorkingHoursResult counts one pass, for the log line and the operator report.
type WorkingHoursResult struct {
	Considered    int // venues whose legacy text differs from the last attempt
	Filled        int // seven rows written
	Unchanged     int // nothing to do (text already imported)
	VenueOwned    int // left alone: the venue's own hours win
	Refused       int // legacy text not understood (incl. NoText)
	NoText        int // of Refused: the venue has no legacy hours at all
	AssumedClosed int // of Filled: at least one weekday the text never mentioned
}

func (r WorkingHoursResult) empty() bool { return r == WorkingHoursResult{} }

// workingHoursFingerprint is a content hash of a venue's weekly rows: it changes
// whenever any weekday's open/close/is_open changes, and ignores row ids and
// timestamps (the admin panel replaces rows wholesale, so ids are not stable).
// It is what tells "still exactly what the sync wrote" from "a human edited it".
func workingHoursFingerprint(rows []domain.WorkingHours) string {
	byDay := make([]string, 7)
	for i := range byDay {
		byDay[i] = "-" // absent weekday, distinct from a stored closed day
	}
	for _, r := range rows {
		if r.DayOfWeek < 0 || r.DayOfWeek > 6 {
			continue
		}
		byDay[r.DayOfWeek] = strconv.FormatBool(r.IsOpen) + "|" + clockKey(r.OpenTime) + "|" + clockKey(r.CloseTime)
	}
	sum := sha256.Sum256([]byte(strings.Join(byDay, ";")))
	return hex.EncodeToString(sum[:])
}

// clockKey normalises a stored time for the fingerprint. Postgres hands a `time`
// column back as "HH:MM:SS"; comparing on "HH:MM" keeps a plain round-trip
// through the database from looking like a human edit.
func clockKey(s *string) string {
	if s == nil {
		return ""
	}
	v := strings.TrimSpace(*s)
	if len(v) > 5 {
		v = v[:5]
	}
	return v
}

// hoursDecision is what the pass may do to one venue.
type hoursDecision int

const (
	// hoursWrite: the sync owns this venue's hours (or there are none) — write.
	hoursWrite hoursDecision = iota
	// hoursKeepVenue: a human owns these hours — never touch them.
	hoursKeepVenue
	// hoursNothingToDo: this exact legacy text was already handled.
	hoursNothingToDo
)

// decideWorkingHours is the whole "never overwrite a venue's own edit" rule, as
// a pure function so it can be tested without a database.
func decideWorkingHours(st WorkingHoursState) (hoursDecision, string) {
	if st.Import != nil && st.Import.SourceText == st.SourceText {
		return hoursNothingToDo, "legacy text unchanged since the last attempt"
	}
	if st.Import != nil && st.Import.Status == HoursImportVenueOwned {
		// Once a venue has taken ownership it keeps it, even if the legacy text
		// later changes: the old system is not where they edit any more.
		return hoursKeepVenue, "venue owns its working hours"
	}
	if len(st.Rows) == 0 {
		return hoursWrite, "venue has no working hours yet"
	}
	if st.Import == nil {
		return hoursKeepVenue, "hours exist that this sync never wrote"
	}
	if st.Import.Status != HoursImportFilled || st.Import.Fingerprint == "" {
		return hoursKeepVenue, "hours exist but the last import wrote none"
	}
	if st.Import.Fingerprint != workingHoursFingerprint(st.Rows) {
		return hoursKeepVenue, "hours were edited after the last import"
	}
	return hoursWrite, "hours are still exactly what the sync last wrote"
}

// syncWorkingHours runs one pass over the venues whose legacy text changed (or
// was never attempted). Each venue is handled in its own transaction: one venue
// failing must not roll back the venues already filled.
func (w *Worker) syncWorkingHours(ctx context.Context) (WorkingHoursResult, error) {
	var res WorkingHoursResult
	candidates, err := w.sink.WorkingHoursCandidates(ctx, w.cfg.BatchSize)
	if err != nil {
		return res, err
	}
	res.Considered = len(candidates)

	var firstErr error
	for _, c := range candidates {
		err := w.tx.WithinTx(ctx, func(ctx context.Context) error {
			return w.fillVenueWorkingHours(ctx, c, &res)
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("working hours for restaurant %s: %w", c.RestaurantID, err)
			}
			w.log.Error("legacy sync working hours failed",
				slog.String("restaurant_id", c.RestaurantID.String()),
				slog.String("error", err.Error()))
		}
	}
	return res, firstErr
}

// fillVenueWorkingHours handles ONE venue inside a transaction: re-read the
// state, decide, then write. Refusals and hands-off decisions are persisted too
// so the next pass does not reconsider (and re-log) the same unchanged text.
func (w *Worker) fillVenueWorkingHours(ctx context.Context, c WorkingHoursCandidate, res *WorkingHoursResult) error {
	st, err := w.sink.LoadWorkingHours(ctx, c.RestaurantID)
	if err != nil {
		return err
	}

	switch d, reason := decideWorkingHours(st); d {
	case hoursNothingToDo:
		res.Unchanged++
		return nil
	case hoursKeepVenue:
		res.VenueOwned++
		w.log.Info("legacy sync kept the venue's own working hours",
			slog.String("restaurant_id", c.RestaurantID.String()),
			slog.String("restaurant", c.Name),
			slog.String("reason", reason))
		return w.sink.RecordWorkingHoursImport(ctx, c.RestaurantID, st.SourceText, HoursImportVenueOwned, reason)
	}

	parsed, err := domain.ParseWorkingHoursText(st.SourceText)
	if err != nil {
		res.Refused++
		if strings.TrimSpace(st.SourceText) == "" {
			res.NoText++
		}
		// Loud on purpose: this is the list a human has to fill in by hand in
		// the admin panel. The venue stays unbookable until then.
		w.log.Warn("legacy sync could not parse opening hours",
			slog.String("restaurant_id", c.RestaurantID.String()),
			slog.String("restaurant", c.Name),
			slog.String("opening_hours", st.SourceText),
			slog.String("reason", err.Error()))
		return w.sink.RecordWorkingHoursImport(ctx, c.RestaurantID, st.SourceText, HoursImportRefused, err.Error())
	}

	rows := parsed.ToWorkingHours(c.RestaurantID)
	if err := w.sink.ReplaceSyncedWorkingHours(ctx, c.RestaurantID, rows,
		st.SourceText, workingHoursFingerprint(rows)); err != nil {
		return err
	}
	res.Filled++
	if len(parsed.AssumedClosed) > 0 {
		res.AssumedClosed++
		// Not an error, but a judgement call worth a human's eye: the text
		// simply never mentioned these weekdays, so they were stored closed.
		w.log.Warn("legacy sync stored unmentioned weekdays as closed",
			slog.String("restaurant_id", c.RestaurantID.String()),
			slog.String("restaurant", c.Name),
			slog.String("opening_hours", st.SourceText),
			slog.String("assumed_closed", weekdayList(parsed.AssumedClosed)))
	}
	return nil
}

func weekdayList(days []time.Weekday) string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, d.String())
	}
	return strings.Join(out, ",")
}

// logWorkingHours emits the one-line pass summary, in the same shape as the
// per-entity tick line.
func (w *Worker) logWorkingHours(res WorkingHoursResult) {
	if res.empty() {
		return
	}
	w.log.Info(logging.EventLegacySyncTick,
		slog.String("entity", EntityWorkingHours),
		slog.Int("considered", res.Considered),
		slog.Int("filled", res.Filled),
		slog.Int("unchanged", res.Unchanged),
		slog.Int("venue_owned", res.VenueOwned),
		slog.Int("refused", res.Refused),
		slog.Int("no_text", res.NoText),
		slog.Int("assumed_closed", res.AssumedClosed))
}

package legacysync

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/logger"
)

// ---- fakes -----------------------------------------------------------------

// hoursSink implements only the working-hours part of Sink; the embedded
// interface is nil, so a pass that calls anything else panics loudly instead of
// silently doing something unrelated.
type hoursSink struct {
	Sink
	state      map[uuid.UUID]WorkingHoursState
	candidates []WorkingHoursCandidate

	written  map[uuid.UUID][]domain.WorkingHours
	imported map[uuid.UUID]WorkingHoursImport
	reasons  map[uuid.UUID]string
}

func newHoursSink() *hoursSink {
	return &hoursSink{
		state:    map[uuid.UUID]WorkingHoursState{},
		written:  map[uuid.UUID][]domain.WorkingHours{},
		imported: map[uuid.UUID]WorkingHoursImport{},
		reasons:  map[uuid.UUID]string{},
	}
}

func (s *hoursSink) WorkingHoursCandidates(context.Context, int) ([]WorkingHoursCandidate, error) {
	return s.candidates, nil
}

func (s *hoursSink) LoadWorkingHours(_ context.Context, rid uuid.UUID) (WorkingHoursState, error) {
	return s.state[rid], nil
}

func (s *hoursSink) ReplaceSyncedWorkingHours(_ context.Context, rid uuid.UUID, rows []domain.WorkingHours, sourceText, fingerprint string) error {
	s.written[rid] = rows
	s.imported[rid] = WorkingHoursImport{SourceText: sourceText, Status: HoursImportFilled, Fingerprint: fingerprint}
	return nil
}

func (s *hoursSink) RecordWorkingHoursImport(_ context.Context, rid uuid.UUID, sourceText, status, reason string) error {
	s.imported[rid] = WorkingHoursImport{SourceText: sourceText, Status: status}
	s.reasons[rid] = reason
	return nil
}

// passthroughTx runs fn with no real transaction — the ownership rule under test
// lives in the usecase, not in the database.
type passthroughTx struct{}

func (passthroughTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (passthroughTx) Detach(ctx context.Context) context.Context { return ctx }

func hoursWorker(sink Sink) *Worker {
	return NewWorker(nil, sink, passthroughTx{}, Config{BatchSize: 100}, logger.New("error", "text"))
}

// syncedRows is what the sync itself would have written for text — used to set
// up "untouched since the import" states.
func syncedRows(t *testing.T, rid uuid.UUID, text string) []domain.WorkingHours {
	t.Helper()
	p, err := domain.ParseWorkingHoursText(text)
	if err != nil {
		t.Fatalf("fixture %q does not parse: %v", text, err)
	}
	return p.ToWorkingHours(rid)
}

// ---- the ownership rule ----------------------------------------------------

// TestWorkingHoursNeverOverwritesVenueEdits is the guard on the rule that the
// venue's own edit always wins. If the rule is removed, these cases start
// writing rows and fail.
func TestWorkingHoursNeverOverwritesVenueEdits(t *testing.T) {
	const oldText = "Пн-Вс: 10:00-23:00"
	const newText = "Пн-Вс: 09:00-22:00"

	t.Run("hours entered by hand (no import record) are untouchable", func(t *testing.T) {
		rid := uuid.New()
		sink := newHoursSink()
		sink.candidates = []WorkingHoursCandidate{{RestaurantID: rid, Name: "Bar", SourceText: newText}}
		sink.state[rid] = WorkingHoursState{SourceText: newText, Rows: syncedRows(t, rid, oldText)}

		res, err := hoursWorker(sink).syncWorkingHours(context.Background())
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		if len(sink.written) != 0 {
			t.Fatalf("the sync overwrote hours it never wrote: %+v", sink.written)
		}
		if res.VenueOwned != 1 || res.Filled != 0 {
			t.Fatalf("result = %+v, want exactly one venue_owned", res)
		}
		if got := sink.imported[rid].Status; got != HoursImportVenueOwned {
			t.Fatalf("import status = %q, want %q", got, HoursImportVenueOwned)
		}
	})

	t.Run("hours edited after our import are untouchable", func(t *testing.T) {
		rid := uuid.New()
		rows := syncedRows(t, rid, oldText)
		edited := append([]domain.WorkingHours(nil), rows...)
		edited[3].IsOpen = false // the venue closed Wednesdays in the admin panel

		sink := newHoursSink()
		sink.candidates = []WorkingHoursCandidate{{RestaurantID: rid, SourceText: newText}}
		sink.state[rid] = WorkingHoursState{
			SourceText: newText,
			Rows:       edited,
			Import: &WorkingHoursImport{
				SourceText: oldText, Status: HoursImportFilled,
				Fingerprint: workingHoursFingerprint(rows), // hash of what we wrote, before the edit
			},
		}

		res, err := hoursWorker(sink).syncWorkingHours(context.Background())
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		if len(sink.written) != 0 {
			t.Fatalf("the sync overwrote an edited week: %+v", sink.written)
		}
		if res.VenueOwned != 1 {
			t.Fatalf("result = %+v, want one venue_owned", res)
		}
	})

	t.Run("a venue that once took ownership stays owned even when the legacy text changes", func(t *testing.T) {
		rid := uuid.New()
		sink := newHoursSink()
		sink.candidates = []WorkingHoursCandidate{{RestaurantID: rid, SourceText: newText}}
		sink.state[rid] = WorkingHoursState{
			SourceText: newText,
			Rows:       syncedRows(t, rid, oldText),
			Import:     &WorkingHoursImport{SourceText: oldText, Status: HoursImportVenueOwned},
		}

		if _, err := hoursWorker(sink).syncWorkingHours(context.Background()); err != nil {
			t.Fatalf("pass: %v", err)
		}
		if len(sink.written) != 0 {
			t.Fatalf("the sync took a venue-owned week back: %+v", sink.written)
		}
	})

	t.Run("untouched rows the sync itself wrote are refreshed", func(t *testing.T) {
		rid := uuid.New()
		rows := syncedRows(t, rid, oldText)
		sink := newHoursSink()
		sink.candidates = []WorkingHoursCandidate{{RestaurantID: rid, SourceText: newText}}
		sink.state[rid] = WorkingHoursState{
			SourceText: newText,
			Rows:       rows,
			Import: &WorkingHoursImport{
				SourceText: oldText, Status: HoursImportFilled,
				Fingerprint: workingHoursFingerprint(rows),
			},
		}

		res, err := hoursWorker(sink).syncWorkingHours(context.Background())
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		if res.Filled != 1 {
			t.Fatalf("result = %+v, want one filled", res)
		}
		got := sink.written[rid]
		if len(got) != 7 || got[1].OpenTime == nil || *got[1].OpenTime != "09:00" {
			t.Fatalf("refreshed week = %+v, want the new 09:00 opening", got)
		}
	})
}

func TestWorkingHoursFillsAndIsIdempotent(t *testing.T) {
	rid := uuid.New()
	const text = "Пн — Вт Выходной, Ср — Чт: 17:00–03:00, Пт — Сб: 17:00–05:00, Вс: 17:00–03:00"

	sink := newHoursSink()
	sink.candidates = []WorkingHoursCandidate{{RestaurantID: rid, Name: "Night bar", SourceText: text}}
	sink.state[rid] = WorkingHoursState{SourceText: text}

	res, err := hoursWorker(sink).syncWorkingHours(context.Background())
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if res.Filled != 1 || res.VenueOwned != 0 || res.Refused != 0 {
		t.Fatalf("result = %+v, want one filled", res)
	}
	rows := sink.written[rid]
	if len(rows) != 7 {
		t.Fatalf("wrote %d rows, want one per weekday", len(rows))
	}
	// Monday/Tuesday are "Выходной": stored as a real closed row, not skipped.
	if rows[1].IsOpen || rows[2].IsOpen {
		t.Fatalf("Пн/Вт must be stored closed: %+v %+v", rows[1], rows[2])
	}
	// Past-midnight window kept verbatim — availability rolls it over itself.
	if *rows[5].OpenTime != "17:00" || *rows[5].CloseTime != "05:00" {
		t.Fatalf("Friday = %s-%s, want 17:00-05:00", *rows[5].OpenTime, *rows[5].CloseTime)
	}

	// Second pass over the SAME text: the state now carries our import record,
	// so nothing is written again.
	sink.state[rid] = WorkingHoursState{
		SourceText: text,
		Rows:       rows,
		Import: &WorkingHoursImport{SourceText: text, Status: HoursImportFilled,
			Fingerprint: workingHoursFingerprint(rows)},
	}
	sink.written = map[uuid.UUID][]domain.WorkingHours{}
	res2, err := hoursWorker(sink).syncWorkingHours(context.Background())
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(sink.written) != 0 || res2.Unchanged != 1 {
		t.Fatalf("re-run wrote %+v, result %+v — the pass is not idempotent", sink.written, res2)
	}
}

func TestWorkingHoursRefusalIsRecordedAndCounted(t *testing.T) {
	bad := uuid.New()
	empty := uuid.New()

	sink := newHoursSink()
	sink.candidates = []WorkingHoursCandidate{
		{RestaurantID: bad, Name: "Ambiguous", SourceText: "Пн-Вс 12:00–01:00, Пт–Сб до 02:00"},
		{RestaurantID: empty, Name: "No hours", SourceText: ""},
	}
	sink.state[bad] = WorkingHoursState{SourceText: "Пн-Вс 12:00–01:00, Пт–Сб до 02:00"}
	sink.state[empty] = WorkingHoursState{SourceText: ""}

	res, err := hoursWorker(sink).syncWorkingHours(context.Background())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(sink.written) != 0 {
		t.Fatalf("an unparseable venue got hours anyway: %+v", sink.written)
	}
	if res.Refused != 2 || res.NoText != 1 {
		t.Fatalf("result = %+v, want 2 refused of which 1 without any text", res)
	}
	for _, rid := range []uuid.UUID{bad, empty} {
		if sink.imported[rid].Status != HoursImportRefused {
			t.Fatalf("venue %s: import status = %q, want refused", rid, sink.imported[rid].Status)
		}
		if !strings.Contains(sink.reasons[rid], "not understood") {
			t.Fatalf("venue %s: reason %q does not say why", rid, sink.reasons[rid])
		}
	}
}

func TestWorkingHoursFingerprintDetectsEveryFieldChange(t *testing.T) {
	rid := uuid.New()
	base := syncedRows(t, rid, "Пн-Вс: 10:00-23:00")
	fp := workingHoursFingerprint(base)

	// A round-trip through Postgres renders varchar times as HH:MM:SS on some
	// paths; that must NOT read as an edit.
	roundTripped := append([]domain.WorkingHours(nil), base...)
	for i := range roundTripped {
		if roundTripped[i].OpenTime != nil {
			v := *roundTripped[i].OpenTime + ":00"
			c := *roundTripped[i].CloseTime + ":00"
			roundTripped[i].OpenTime, roundTripped[i].CloseTime = &v, &c
		}
	}
	if workingHoursFingerprint(roundTripped) != fp {
		t.Fatalf("HH:MM:SS round-trip changed the fingerprint — every sync would think the venue edited its hours")
	}

	mutations := map[string]func([]domain.WorkingHours){
		"closed a day":      func(r []domain.WorkingHours) { r[2].IsOpen = false },
		"moved opening":     func(r []domain.WorkingHours) { v := "11:00"; r[2].OpenTime = &v },
		"moved closing":     func(r []domain.WorkingHours) { v := "22:00"; r[2].CloseTime = &v },
		"dropped a weekday": func(r []domain.WorkingHours) { copy(r[2:], r[3:]) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			rows := make([]domain.WorkingHours, len(base))
			copy(rows, base)
			if name == "dropped a weekday" {
				rows = rows[:len(rows)-1]
			}
			mutate(rows)
			if workingHoursFingerprint(rows) == fp {
				t.Fatalf("%s did not change the fingerprint — an edit would be silently overwritten", name)
			}
		})
	}
}

func TestDecideWorkingHours(t *testing.T) {
	rid := uuid.New()
	rows := syncedRows(t, rid, "Пн-Вс: 10:00-23:00")
	fp := workingHoursFingerprint(rows)

	cases := []struct {
		name string
		st   WorkingHoursState
		want hoursDecision
	}{
		{
			name: "never attempted, no hours",
			st:   WorkingHoursState{SourceText: "x"},
			want: hoursWrite,
		},
		{
			name: "same text as the last attempt",
			st:   WorkingHoursState{SourceText: "x", Import: &WorkingHoursImport{SourceText: "x", Status: HoursImportRefused}},
			want: hoursNothingToDo,
		},
		{
			name: "refused before, text changed, still no hours",
			st:   WorkingHoursState{SourceText: "y", Import: &WorkingHoursImport{SourceText: "x", Status: HoursImportRefused}},
			want: hoursWrite,
		},
		{
			name: "our rows, untouched",
			st: WorkingHoursState{SourceText: "y", Rows: rows,
				Import: &WorkingHoursImport{SourceText: "x", Status: HoursImportFilled, Fingerprint: fp}},
			want: hoursWrite,
		},
		{
			name: "our rows, edited",
			st: WorkingHoursState{SourceText: "y", Rows: rows,
				Import: &WorkingHoursImport{SourceText: "x", Status: HoursImportFilled, Fingerprint: "deadbeef"}},
			want: hoursKeepVenue,
		},
		{
			name: "rows with no import record",
			st:   WorkingHoursState{SourceText: "y", Rows: rows},
			want: hoursKeepVenue,
		},
		{
			name: "refused before but hours appeared meanwhile",
			st: WorkingHoursState{SourceText: "y", Rows: rows,
				Import: &WorkingHoursImport{SourceText: "x", Status: HoursImportRefused}},
			want: hoursKeepVenue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := decideWorkingHours(tc.st)
			if got != tc.want {
				t.Fatalf("decision = %v (%s), want %v", got, reason, tc.want)
			}
			if reason == "" {
				t.Fatal("decision carries no reason for the log")
			}
		})
	}
}

// Clearing the hours in the admin panel is a decision, not an absence: a venue
// that wiped what the sync had filled must not have it quietly restored the
// next time someone edits the legacy text.
func TestDecideWorkingHours_ClearedByVenueIsNotRefilled(t *testing.T) {
	st := WorkingHoursState{
		SourceText: "Пн-Вс: 10:00-22:00",
		Rows:       nil,
		Import: &WorkingHoursImport{
			SourceText:  "Пн-Вс: 09:00-21:00", // legacy text has since changed
			Status:      HoursImportFilled,
			Fingerprint: "whatever-we-wrote-before",
		},
	}
	if got, why := decideWorkingHours(st); got != hoursKeepVenue {
		t.Fatalf("decision = %v (%s), want keep-venue: an empty week the venue chose is still the venue's", got, why)
	}
}

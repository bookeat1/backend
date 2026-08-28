package domain

// Shared content of a recurring series, and the per-date override that may
// deviate from it.
//
// The problem this solves: until now every date of «Афиша Greek Party» was a
// full copy of the editorial content, so changing one word meant editing
// eighteen rows by hand. The rule (EventRecurrence) already carries the same
// content columns as its occurrences — it is the template the generator copies
// — so the series content lives THERE, and the occurrences are kept in sync
// with it (EventRecurrenceRepository.SyncOccurrenceContent).
//
// Why the occurrences still carry their own copy of the words instead of
// reading them through a join: every read path in this codebase (the guest
// listing, the collapse query, the detail page, the home feed read model, the
// ticket and booking flows) already selects from `events`. Resolving the
// content at read time would mean touching all of them at once, and a single
// forgotten query would show a guest last month's poster. The copy is written
// by exactly one statement, in one place, which is the trade this design picks:
// one careful writer instead of a dozen careful readers.
//
// A single date may still deviate — «в эту субботу другой гость» — and that is
// what EventContentField and Event.ContentOverrides express.

// EventContentField names one editorially meaningful piece of an event's
// content. It is the unit of inheritance: a field listed in
// Event.ContentOverrides belongs to that DATE and is never rewritten by its
// series; a field that is absent is INHERITED and kept equal to the series.
//
// This is what distinguishes "the field was left empty on purpose" from "the
// field is not filled in, take the series value" — a distinction NULL alone
// cannot make, because a NULL cover already means "no cover" and an empty
// description already means "no description" for the thousands of one-off
// events that have no series at all.
//
// The vocabulary is deliberately coarser than the column list: title and
// title_i18n are ONE editorial decision (the same sentence in several
// languages), and so are description and description_i18n. Nobody overrides the
// Kazakh title of a date while inheriting its Russian one.
type EventContentField string

const (
	// EventContentTitle covers title + title_i18n.
	EventContentTitle EventContentField = "title"
	// EventContentDescription covers description + description_i18n.
	EventContentDescription EventContentField = "description"
	// EventContentVenue covers the free-text room inside the venue.
	EventContentVenue EventContentField = "venue"
	// EventContentCover covers the poster (cover_image_url).
	EventContentCover EventContentField = "cover_image_url"
	// EventContentTags covers the «Афиша» chips.
	EventContentTags EventContentField = "tags"
)

// EventContentFields is the whole vocabulary, in a stable order. The database
// CHECK in migration 0097 lists exactly these values; adding one means adding
// it in both places (and teaching SyncOccurrenceContent to carry it).
//
// What is deliberately NOT here: status, the schedule, the ticketing terms and
// the capacity. A single date that a venue hid, moved, sold out or priced
// differently must never be resurrected by an edit to the series text — and a
// ticket already sold against that date was sold under those terms.
var EventContentFields = []EventContentField{
	EventContentTitle,
	EventContentDescription,
	EventContentVenue,
	EventContentCover,
	EventContentTags,
}

// Valid reports whether f names a known content field.
func (f EventContentField) Valid() bool {
	for _, known := range EventContentFields {
		if f == known {
			return true
		}
	}
	return false
}

// EventContent is the editorial content shared by a series: exactly the fields
// EventContentFields names, in the types both `events` and `event_recurrences`
// store them in.
type EventContent struct {
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	Venue           string
	CoverImageURL   *string
	Tags            []string
}

// Content returns the rule's own content — the series value every occurrence
// inherits unless it overrides the field.
func (r EventRecurrence) Content() EventContent {
	return EventContent{
		Title:           r.Title,
		TitleI18n:       r.TitleI18n,
		Description:     r.Description,
		DescriptionI18n: r.DescriptionI18n,
		Venue:           r.Venue,
		CoverImageURL:   r.CoverImageURL,
		Tags:            r.Tags,
	}
}

// Content returns this occurrence's own content, whatever it currently is —
// inherited or overridden. Comparing it to the rule's Content is how the
// override markers are derived.
func (e Event) Content() EventContent {
	return EventContent{
		Title:           e.Title,
		TitleI18n:       e.TitleI18n,
		Description:     e.Description,
		DescriptionI18n: e.DescriptionI18n,
		Venue:           e.Venue,
		CoverImageURL:   e.CoverImageURL,
		Tags:            e.Tags,
	}
}

// OverridesContent reports whether this date owns the given field, i.e. whether
// the series must leave it alone.
func (e Event) OverridesContent(f EventContentField) bool {
	for _, o := range e.ContentOverrides {
		if o == f {
			return true
		}
	}
	return false
}

// EventContentDiff lists the fields in which want differs from base, in the
// EventContentFields order.
//
// This one function is the whole override policy, and it is intentionally
// derived rather than declared by the client: the cabinet sends a date's
// content as a full replace (it always has), and a field that comes back EQUAL
// to the series is inheritance restored — re-typing the series text on a date
// hands the date back to the series instead of silently freezing a copy of
// today's wording.
func EventContentDiff(base, want EventContent) []EventContentField {
	out := make([]EventContentField, 0, len(EventContentFields))
	if want.Title != base.Title || !i18nMapEqual(want.TitleI18n, base.TitleI18n) {
		out = append(out, EventContentTitle)
	}
	if want.Description != base.Description || !i18nMapEqual(want.DescriptionI18n, base.DescriptionI18n) {
		out = append(out, EventContentDescription)
	}
	if want.Venue != base.Venue {
		out = append(out, EventContentVenue)
	}
	if !optStringEqual(want.CoverImageURL, base.CoverImageURL) {
		out = append(out, EventContentCover)
	}
	if !stringSliceEqual(want.Tags, base.Tags) {
		out = append(out, EventContentTags)
	}
	return out
}

// ApplyEventContent copies the listed fields of c onto e, leaving every other
// field of the event untouched. It is what "reset this date back to the series"
// does; it does NOT touch e.ContentOverrides, because the caller re-derives
// those from the resulting content (see EventContentDiff).
func ApplyEventContent(e *Event, c EventContent, fields []EventContentField) {
	for _, f := range fields {
		switch f {
		case EventContentTitle:
			e.Title, e.TitleI18n = c.Title, c.TitleI18n
		case EventContentDescription:
			e.Description, e.DescriptionI18n = c.Description, c.DescriptionI18n
		case EventContentVenue:
			e.Venue = c.Venue
		case EventContentCover:
			e.CoverImageURL = c.CoverImageURL
		case EventContentTags:
			e.Tags = c.Tags
		}
	}
}

// i18nMapEqual compares two localized maps by content: nil and empty read the
// same to a guest, so they must never count as a difference (the same rule the
// events usecase applies when deciding whether an edit needs re-moderation).
func i18nMapEqual(a, b I18n) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// optStringEqual compares two optional strings; absent equals absent only.
func optStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// stringSliceEqual compares chip lists by content AND order — the order is what
// the card draws, so a reordered list is a real edit. nil and empty are equal.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

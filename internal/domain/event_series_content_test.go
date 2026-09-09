package domain

import "testing"

func sp(s string) *string { return &s }

func seriesContent() EventContent {
	return EventContent{
		Title:           "Greek Party",
		TitleI18n:       I18n{"en": "Greek Party"},
		Description:     "Сиртаки и узо",
		DescriptionI18n: I18n{"en": "Sirtaki and ouzo"},
		Venue:           "терраса",
		CoverImageURL:   sp("https://cdn.example/greek.jpg"),
		Tags:            []string{"Живая музыка"},
	}
}

func names(fields []EventContentField) string {
	out := ""
	for _, f := range fields {
		out += string(f) + ";"
	}
	return out
}

func TestEventContentDiffFindsNothingWhenIdentical(t *testing.T) {
	if got := EventContentDiff(seriesContent(), seriesContent()); len(got) != 0 {
		t.Fatalf("identical content owns nothing, got %s", names(got))
	}
}

// nil and an empty map read the same to a guest, so they must not register as
// an edit — otherwise a client that omits an empty i18n map would claim the
// field on every save.
func TestEventContentDiffTreatsNilAndEmptyI18nAsEqual(t *testing.T) {
	base := seriesContent()
	base.TitleI18n = nil
	base.DescriptionI18n = nil
	want := seriesContent()
	want.TitleI18n = I18n{}
	want.DescriptionI18n = I18n{}
	if got := EventContentDiff(base, want); len(got) != 0 {
		t.Fatalf("nil and empty i18n must be equal, got %s", names(got))
	}
}

// The i18n map is part of the same editorial decision as the base string: a
// changed translation claims "title", not some separate field.
func TestEventContentDiffCountsI18nAsItsField(t *testing.T) {
	want := seriesContent()
	want.TitleI18n = I18n{"en": "Greek Night"}
	got := EventContentDiff(seriesContent(), want)
	if len(got) != 1 || got[0] != EventContentTitle {
		t.Fatalf("a changed translation belongs to its own field, got %s", names(got))
	}
}

// The distinction the marker column exists for: an emptied field is OWNED, not
// unfilled.
func TestEventContentDiffTreatsEmptiedFieldsAsOwned(t *testing.T) {
	want := seriesContent()
	want.Description, want.DescriptionI18n = "", nil
	want.CoverImageURL = nil
	want.Tags = nil
	got := EventContentDiff(seriesContent(), want)
	if names(got) != "description;cover_image_url;tags;" {
		t.Fatalf("emptied fields must be owned by the date, got %s", names(got))
	}
}

// Reordering the chips is a real edit: the order is what the card draws.
func TestEventContentDiffSeesReorderedTags(t *testing.T) {
	base := seriesContent()
	base.Tags = []string{"Живая музыка", "Греция"}
	want := base
	want.Tags = []string{"Греция", "Живая музыка"}
	got := EventContentDiff(base, want)
	if len(got) != 1 || got[0] != EventContentTags {
		t.Fatalf("a reordered chip list is an edit, got %s", names(got))
	}
}

func TestApplyEventContentTouchesOnlyTheNamedFields(t *testing.T) {
	e := Event{
		Title:         "Greek Party с Никосом",
		Description:   "Гость — Никос",
		Venue:         "свой зал",
		CoverImageURL: sp("https://cdn.example/nikos.jpg"),
		Tags:          []string{"Своё"},
		Status:        EventPublished,
	}
	ApplyEventContent(&e, seriesContent(), []EventContentField{EventContentCover})

	if *e.CoverImageURL != "https://cdn.example/greek.jpg" {
		t.Fatalf("the cover must come from the series, got %q", *e.CoverImageURL)
	}
	if e.Title != "Greek Party с Никосом" || e.Description != "Гость — Никос" || e.Venue != "свой зал" {
		t.Fatalf("a partial reset must leave the other fields alone: %+v", e)
	}
	if e.Status != EventPublished {
		t.Fatalf("content is not status: %q", e.Status)
	}
}

func TestEventContentFieldValidity(t *testing.T) {
	for _, f := range EventContentFields {
		if !f.Valid() {
			t.Fatalf("%q must be a known field", f)
		}
	}
	for _, f := range []EventContentField{"", "cover", "status", "starts_at", "capacity"} {
		if f.Valid() {
			t.Fatalf("%q must not be a content field", f)
		}
	}
}

func TestOverridesContent(t *testing.T) {
	e := Event{ContentOverrides: []EventContentField{EventContentCover}}
	if !e.OverridesContent(EventContentCover) {
		t.Fatal("the cover is this date's own")
	}
	if e.OverridesContent(EventContentTitle) {
		t.Fatal("the title is inherited")
	}
}

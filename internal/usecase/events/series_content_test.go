package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeSeries is the seriesContentReader port: one rule, addressed by id.
type fakeSeries struct {
	rules map[uuid.UUID]*domain.EventRecurrence
	calls int
}

func (f *fakeSeries) GetByID(_ context.Context, id uuid.UUID) (*domain.EventRecurrence, error) {
	f.calls++
	rec, ok := f.rules[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *rec
	return &cp, nil
}

func strp(s string) *string { return &s }

// seriesFixture builds one venue, one rule and one occurrence that currently
// inherits everything from it — the state every generated date starts in.
func seriesFixture(t *testing.T) (*facadeFixture, *domain.Event) {
	t.Helper()
	rid := uuid.New()
	actorID := uuid.New()
	ruleID := uuid.New()
	rule := &domain.EventRecurrence{
		ID:            ruleID,
		RestaurantID:  rid,
		Title:         "Greek Party",
		Description:   "Сиртаки и узо",
		Venue:         "терраса",
		CoverImageURL: strp("https://cdn.example/greek.jpg"),
		Tags:          []string{"Живая музыка"},
	}
	ev := &domain.Event{
		ID:            uuid.New(),
		RestaurantID:  &rid,
		RecurrenceID:  &ruleID,
		Title:         rule.Title,
		Description:   rule.Description,
		Venue:         rule.Venue,
		CoverImageURL: rule.CoverImageURL,
		Tags:          rule.Tags,
		Status:        domain.EventPublished,
		StartsAt:      time.Now().Add(48 * time.Hour),
		EndsAt:        time.Now().Add(51 * time.Hour),
	}
	repo := newFakeRepo()
	repo.byID[ev.ID] = ev
	series := &fakeSeries{rules: map[uuid.UUID]*domain.EventRecurrence{ruleID: rule}}
	feed := &fakeFeed{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), feed,
		WithSeriesContent(series))
	return &facadeFixture{
		facade: f, repo: repo, series: series, feed: feed,
		actor: Actor{UserID: actorID, Role: domain.RoleRestaurant},
		rule:  rule,
	}, ev
}

type facadeFixture struct {
	facade Facade
	repo   *fakeEventRepo
	series *fakeSeries
	feed   *fakeFeed
	actor  Actor
	rule   *domain.EventRecurrence
}

// updateFrom builds the cabinet's full-replace payload out of the event as it
// currently is, so a test only has to state what it CHANGES.
func updateFrom(e domain.Event) UpdateInput {
	return UpdateInput{
		Title:           e.Title,
		TitleI18n:       e.TitleI18n,
		Description:     e.Description,
		DescriptionI18n: e.DescriptionI18n,
		StartsAt:        e.StartsAt,
		EndsAt:          e.EndsAt,
		Venue:           e.Venue,
		CoverImageURL:   e.CoverImageURL,
		Status:          e.Status,
		Tags:            e.Tags,
	}
}

func overrideNames(fields []domain.EventContentField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, string(f))
	}
	return out
}

// A date that re-saves exactly what the series says owns NOTHING: the cabinet
// sends a full replace on every edit (here: only the status changes), and
// treating that as eighteen overrides would freeze the whole series on its
// first save.
func TestUpdate_ResavingSeriesContentKeepsInheritance(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.Status = domain.EventHidden

	got, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(got.ContentOverrides) != 0 {
		t.Fatalf("a date that changed no content must own nothing, got %v", overrideNames(got.ContentOverrides))
	}
}

// Changing ONE field on ONE date marks that field — and only that field — as
// this date's own. The rest of the row keeps following the series.
func TestUpdate_MarksOnlyTheChangedField(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.CoverImageURL = strp("https://cdn.example/this-saturday.jpg")

	got, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if names := overrideNames(got.ContentOverrides); len(names) != 1 || names[0] != "cover_image_url" {
		t.Fatalf("only the cover must become this date's own, got %v", names)
	}
}

// An EMPTY value set on purpose is an override, not "unfilled". This is the
// distinction the whole marker column exists for: without it, clearing a date's
// description would read as "inherit" and the series text would come straight
// back on the next series edit.
func TestUpdate_DeliberatelyEmptyIsAnOverride(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.Description = ""
	in.CoverImageURL = nil

	got, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	names := overrideNames(got.ContentOverrides)
	if len(names) != 2 || names[0] != "description" || names[1] != "cover_image_url" {
		t.Fatalf("an emptied field must be owned by the date, got %v", names)
	}
}

// A one-off event has no series to inherit from, so it never carries markers —
// and the facade must not go looking for a rule that does not exist.
func TestUpdate_OneOffEventOwnsNothing(t *testing.T) {
	fx, ev := seriesFixture(t)
	ev.RecurrenceID = nil
	in := updateFrom(*ev)
	in.Title = "Другое событие"

	got, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(got.ContentOverrides) != 0 {
		t.Fatalf("a one-off event must carry no overrides, got %v", overrideNames(got.ContentOverrides))
	}
	if fx.series.calls != 0 {
		t.Fatalf("no series lookup must happen for a one-off event, got %d", fx.series.calls)
	}
}

// Reset hands the date back to the series: the series content is copied on and
// the markers disappear, so the next series edit reaches this date again.
func TestResetSeriesContent_RestoresInheritance(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.Title = "Greek Party с Никосом"
	in.CoverImageURL = strp("https://cdn.example/nikos.jpg")
	if _, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID, nil)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(got.ContentOverrides) != 0 {
		t.Fatalf("reset must clear every marker, got %v", overrideNames(got.ContentOverrides))
	}
	if got.Title != fx.rule.Title {
		t.Fatalf("title must be back to the series value, got %q", got.Title)
	}
	if got.CoverImageURL == nil || *got.CoverImageURL != *fx.rule.CoverImageURL {
		t.Fatalf("cover must be back to the series value, got %v", got.CoverImageURL)
	}
}

// A partial reset is a real use case ("верни афишу, текст оставь мой"), and it
// must leave the fields it was not asked about exactly as they are.
func TestResetSeriesContent_PartialKeepsTheOtherOverride(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.Title = "Greek Party с Никосом"
	in.CoverImageURL = strp("https://cdn.example/nikos.jpg")
	if _, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID,
		[]domain.EventContentField{domain.EventContentCover})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if names := overrideNames(got.ContentOverrides); len(names) != 1 || names[0] != "title" {
		t.Fatalf("only the cover was reset, the title must stay this date's own, got %v", names)
	}
	if got.Title != "Greek Party с Никосом" {
		t.Fatalf("a partial reset must not touch the title, got %q", got.Title)
	}
	if *got.CoverImageURL != *fx.rule.CoverImageURL {
		t.Fatalf("the cover must be back to the series value, got %q", *got.CoverImageURL)
	}
}

func TestResetSeriesContent_RejectsAnEventWithNoSeries(t *testing.T) {
	fx, ev := seriesFixture(t)
	ev.RecurrenceID = nil

	_, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID, nil)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an event outside a series has nothing to reset to, got %v", err)
	}
}

func TestResetSeriesContent_RejectsUnknownField(t *testing.T) {
	fx, ev := seriesFixture(t)

	_, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID,
		[]domain.EventContentField{"cover"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a misspelled field must be refused, not silently ignored, got %v", err)
	}
}

// Cross-tenant guard: the reset route is a content mutation like any other and
// goes through the same gate.
func TestResetSeriesContent_CrossTenantForbidden(t *testing.T) {
	fx, ev := seriesFixture(t)
	stranger := Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}

	_, err := fx.facade.ResetSeriesContent(context.Background(), stranger, ev.ID, nil)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another venue must not reset this date, got %v", err)
	}
}

// Putting the series' own words back is an editorial change to a card the
// platform approved, so it re-enters moderation exactly as a hand edit does.
func TestResetSeriesContent_DemotesWhenContentActuallyChanges(t *testing.T) {
	fx, ev := seriesFixture(t)
	in := updateFrom(*ev)
	in.Title = "Greek Party с Никосом"
	if _, err := fx.facade.Update(context.Background(), fx.actor, ev.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	fx.feed.demoted = nil

	if _, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID, nil); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(fx.feed.demoted) != 1 || fx.feed.demoted[0] != ev.ID {
		t.Fatalf("a reset that changes the words must re-enter moderation, got %v", fx.feed.demoted)
	}
}

// A reset on a date that already follows its series changes nothing, and must
// therefore cost the venue nothing either — no re-review for a no-op click.
func TestResetSeriesContent_NoOpDoesNotDemote(t *testing.T) {
	fx, ev := seriesFixture(t)

	if _, err := fx.facade.ResetSeriesContent(context.Background(), fx.actor, ev.ID, nil); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(fx.feed.demoted) != 0 {
		t.Fatalf("a no-op reset must not re-enter moderation, got %v", fx.feed.demoted)
	}
}

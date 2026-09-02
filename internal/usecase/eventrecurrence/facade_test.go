package eventrecurrence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ---

type fakeRepo struct {
	byID    map[uuid.UUID]*domain.EventRecurrence
	created *domain.EventRecurrence
	updated *domain.EventRecurrence
	active  map[uuid.UUID]bool
	// syncs records every SyncOccurrenceFeedStatus call, so a test can assert
	// that a decision about the series really reached the occurrences.
	syncs   []syncCall
	demoted []uuid.UUID
	// contentSyncs records every SyncOccurrenceContent call: what content the
	// series pushed down onto the dates it had already generated.
	contentSyncs []domain.EventContent
}

type syncCall struct {
	recurrenceID uuid.UUID
	from         []domain.FeedStatus
	to           domain.FeedStatus
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: map[uuid.UUID]*domain.EventRecurrence{}, active: map[uuid.UUID]bool{}}
}

func (f *fakeRepo) Create(_ context.Context, r *domain.EventRecurrence) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	f.created = r
	f.byID[r.ID] = r
	return nil
}

func (f *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.EventRecurrence, error) {
	r, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRepo) Update(_ context.Context, r *domain.EventRecurrence) error {
	if _, ok := f.byID[r.ID]; !ok {
		return domain.ErrNotFound
	}
	f.updated = r
	f.byID[r.ID] = r
	return nil
}

func (f *fakeRepo) SetActive(_ context.Context, id uuid.UUID, active bool) error {
	if _, ok := f.byID[id]; !ok {
		return domain.ErrNotFound
	}
	f.active[id] = active
	return nil
}

func (f *fakeRepo) ListByRestaurant(_ context.Context, restaurantID uuid.UUID, _, _ int) ([]domain.EventRecurrence, int, error) {
	var out []domain.EventRecurrence
	for _, r := range f.byID {
		if r.RestaurantID == restaurantID {
			out = append(out, *r)
		}
	}
	return out, len(out), nil
}

func (f *fakeRepo) ListActive(_ context.Context, _ uuid.UUID, _ int) ([]domain.ActiveEventRecurrence, error) {
	return nil, nil
}

func (f *fakeRepo) InsertOccurrences(_ context.Context, _ *domain.EventRecurrence, _ []time.Time) (int, error) {
	return 0, nil
}

func (f *fakeRepo) RecordSkip(_ context.Context, _ uuid.UUID, _ time.Time) error { return nil }

func (f *fakeRepo) TransitionFeedStatus(_ context.Context, id uuid.UUID, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) error {
	r, ok := f.byID[id]
	if !ok {
		return domain.ErrNotFound
	}
	for _, s := range from {
		if r.OccurrenceFeedStatus == s {
			r.OccurrenceFeedStatus = upd.Status
			r.FeedSubmittedAt = upd.SubmittedAt
			r.FeedReviewedBy = upd.ReviewedBy
			r.FeedReviewedAt = upd.ReviewedAt
			r.FeedRejectionReason = upd.RejectionReason
			return nil
		}
	}
	return domain.ErrInvalidStatus
}

func (f *fakeRepo) DemoteFeedAfterContentEdit(_ context.Context, id uuid.UUID) error {
	f.demoted = append(f.demoted, id)
	if r, ok := f.byID[id]; ok {
		r.OccurrenceFeedStatus = domain.FeedStatusAfterContentEdit(r.OccurrenceFeedStatus)
	}
	return nil
}

func (f *fakeRepo) SyncOccurrenceFeedStatus(_ context.Context, id uuid.UUID, _ time.Time, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) (int, error) {
	f.syncs = append(f.syncs, syncCall{recurrenceID: id, from: from, to: upd.Status})
	return 0, nil
}

func (f *fakeRepo) SyncOccurrenceContent(_ context.Context, _ uuid.UUID, _ time.Time, c domain.EventContent) (int, error) {
	f.contentSyncs = append(f.contentSyncs, c)
	return 0, nil
}

func (f *fakeRepo) ListByFeedStatus(_ context.Context, status domain.FeedStatus, _, _ int) ([]domain.EventRecurrence, int, error) {
	var out []domain.EventRecurrence
	for _, r := range f.byID {
		if r.OccurrenceFeedStatus == status {
			out = append(out, *r)
		}
	}
	return out, len(out), nil
}

type fakePerms struct {
	roles map[[2]uuid.UUID]domain.StaffRole
}

func (f *fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	role, ok := f.roles[[2]uuid.UUID{userID, restaurantID}]
	if !ok {
		return false, nil
	}
	return role.HasPermission(perm), nil
}

func permsWith(userID, rid uuid.UUID, role domain.StaffRole) *fakePerms {
	return &fakePerms{roles: map[[2]uuid.UUID]domain.StaffRole{{userID, rid}: role}}
}

func validInput(rid uuid.UUID) Input {
	return Input{
		RestaurantID:     rid,
		Title:            "Cocktail Wednesday",
		OccurrenceStatus: domain.EventPublished,
		Frequency:        domain.RecurrenceWeekly,
		Weekdays:         []domain.ISOWeekday{3},
		StartMinutes:     19 * 60,
		DurationMinutes:  180,
		StartsOn:         domain.CalendarDate{Year: 2026, Month: time.August, Day: 17},
		IsActive:         true,
	}
}

// --- authorization: identical to the event editor's, on purpose ---

func TestCreateHostessForbidden(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleHostess))

	_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validInput(rid))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not create a recurrence rule, got %v", err)
	}
	if repo.created != nil {
		t.Fatal("nothing must be written when the caller is denied")
	}
}

func TestCreateManagerAllowed(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validInput(rid))
	if err != nil {
		t.Fatalf("a manager must be able to create a rule: %v", err)
	}
	if rec.RestaurantID != rid || rec.Frequency != domain.RecurrenceWeekly {
		t.Fatalf("rule stored wrong: %+v", rec)
	}
}

// A manager of venue A must not reach venue B's rule — the lateral-move check
// every admin path in this codebase owes.
func TestUpdateAnotherVenuesRuleForbidden(t *testing.T) {
	venueA, venueB, actorID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, venueA, domain.StaffRoleManager))
	rec := &domain.EventRecurrence{ID: uuid.New(), RestaurantID: venueB}
	repo.byID[rec.ID] = rec

	_, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rec.ID, validInput(venueB))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another venue must be refused, got %v", err)
	}
	if repo.updated != nil {
		t.Fatal("nothing must be written")
	}
}

func TestSuperadminBypassesRestaurantScope(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{})

	if _, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleAdmin}, validInput(rid)); err != nil {
		t.Fatalf("a superadmin must be able to create a rule anywhere: %v", err)
	}
}

func TestSetActiveDeactivates(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, validInput(rid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.SetActive(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rec.ID, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if repo.active[rec.ID] {
		t.Fatal("the rule must be inactive")
	}
}

// --- validation ---

func TestValidationRefusals(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"empty title", func(in *Input) { in.Title = "  " }},
		{"unknown frequency", func(in *Input) { in.Frequency = "yearly" }},
		{"weekly without weekdays", func(in *Input) { in.Weekdays = nil }},
		{"weekday out of range", func(in *Input) { in.Weekdays = []domain.ISOWeekday{9} }},
		{"monthly without month day", func(in *Input) { in.Frequency = domain.RecurrenceMonthly }},
		{"start time out of the day", func(in *Input) { in.StartMinutes = 24 * 60 }},
		{"zero duration", func(in *Input) { in.DurationMinutes = 0 }},
		{"duration longer than a week", func(in *Input) { in.DurationMinutes = 8 * 24 * 60 }},
		{"unknown occurrence status", func(in *Input) { in.OccurrenceStatus = "archived" }},
		{"missing starts_on", func(in *Input) { in.StartsOn = domain.CalendarDate{} }},
		{"until before starts_on", func(in *Input) {
			d := domain.CalendarDate{Year: 2026, Month: time.August, Day: 1}
			in.UntilDate = &d
		}},
		// The zone rules of domain.NormalizeVenueTimezone apply verbatim: a
		// fixed-offset legacy name is refused because it does not follow DST.
		{"legacy fixed-offset timezone", func(in *Input) { in.Timezone = "EST" }},
		{"server-local timezone", func(in *Input) { in.Timezone = "Local" }},
		{"negative capacity", func(in *Input) { c := -1; in.Capacity = &c }},
		{"negative ticket price", func(in *Input) { p := int64(-1); in.TicketPriceMinor = &p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
			in := validInput(rid)
			tc.mutate(&in)
			_, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("want a validation error, got %v", err)
			}
			if repo.created != nil {
				t.Fatal("an invalid rule must not be written")
			}
		})
	}
}

// A monthly rule needs no weekdays, and any that were sent are dropped rather
// than stored as a lie the next reader has to interpret.
func TestNonWeeklyRuleDropsWeekdays(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	in := validInput(rid)
	in.Frequency = domain.RecurrenceDaily

	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(rec.Weekdays) != 0 {
		t.Fatalf("a daily rule must carry no weekdays, got %v", rec.Weekdays)
	}
}

func TestWeekdaysAreDedupedAndSorted(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	in := validInput(rid)
	in.Weekdays = []domain.ISOWeekday{4, 3, 4}

	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(rec.Weekdays) != 2 || rec.Weekdays[0] != 3 || rec.Weekdays[1] != 4 {
		t.Fatalf("want [3 4], got %v", rec.Weekdays)
	}
}

// An empty timezone means "follow the venue" and must stay empty — storing ""
// as a zone would read as UTC to time.LoadLocation.
func TestEmptyTimezoneStaysEmpty(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	in := validInput(rid)
	in.Timezone = "   "

	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Timezone != "" {
		t.Fatalf("want no zone override, got %q", rec.Timezone)
	}
}

// --- shared content of a series (migration 0097) ---

// Editing the series text must reach the dates that already exist. Before 0097
// it did not, and that is why «Афиша Greek Party» took eighteen edits.
func TestUpdatePushesContentToExistingOccurrences(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	actor := Actor{UserID: actorID, Role: domain.RoleRestaurant}
	rec, err := f.Create(context.Background(), actor, validInput(rid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	in := validInput(rid)
	in.Title = "Greek Party"
	in.Description = "Сиртаки и узо"
	cover := "https://cdn.example/greek.jpg"
	in.CoverImageURL = &cover
	if _, err := f.Update(context.Background(), actor, rec.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(repo.contentSyncs) != 1 {
		t.Fatalf("the new content must be pushed to the dates exactly once, got %d pushes", len(repo.contentSyncs))
	}
	got := repo.contentSyncs[0]
	if got.Title != "Greek Party" || got.Description != "Сиртаки и узо" || got.CoverImageURL == nil || *got.CoverImageURL != cover {
		t.Fatalf("the pushed content must be what was just saved, got %+v", got)
	}
}

// A schedule-only edit changes no words, so it must not rewrite a single date.
// The occurrences already generated keep their time — moving them is a
// deliberately separate decision (see the Update doc).
func TestUpdateWithoutContentChangeTouchesNoOccurrence(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))
	actor := Actor{UserID: actorID, Role: domain.RoleRestaurant}
	rec, err := f.Create(context.Background(), actor, validInput(rid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	in := validInput(rid)
	in.Weekdays = []domain.ISOWeekday{4}
	if _, err := f.Update(context.Background(), actor, rec.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(repo.contentSyncs) != 0 {
		t.Fatalf("a schedule-only edit must not rewrite any date, got %d pushes", len(repo.contentSyncs))
	}
}

// A refused edit must not reach the occurrences either: authorization comes
// first, and a cross-tenant caller may not retitle eighteen live dates.
func TestUpdateForbiddenPushesNothing(t *testing.T) {
	venueA, venueB, actorID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, venueA, domain.StaffRoleManager))
	rec := &domain.EventRecurrence{ID: uuid.New(), RestaurantID: venueB}
	repo.byID[rec.ID] = rec

	in := validInput(venueB)
	in.Title = "Чужая афиша"
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rec.ID, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected forbidden, got %v", err)
	}
	if len(repo.contentSyncs) != 0 {
		t.Fatalf("a refused edit must push nothing, got %d pushes", len(repo.contentSyncs))
	}
}

// --- venue translations on the series template (migration 0101) ---

func strPtr(s string) *string { return &s }

// The rule's venue line is the template every generated date inherits, so its
// translations have to live on the rule too — otherwise a series could only
// ever be translated one date at a time.
func TestCreate_WritesVenueTranslations(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	in := validInput(rid)
	in.Venue = "Летняя терраса"
	in.VenueI18n = domain.I18nPatch{"kk": strPtr("Жазғы террасса")}

	rec, err := f.Create(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.VenueI18n["kk"] != "Жазғы террасса" {
		t.Errorf("VenueI18n[kk] = %q", rec.VenueI18n["kk"])
	}
	if rec.VenueI18n["ru"] != "Летняя терраса" {
		t.Errorf(`VenueI18n["ru"] = %q, want it equal to the venue column`, rec.VenueI18n["ru"])
	}
}

// A Kazakh-only edit keeps the English, and the new map is what gets pushed
// down onto the dates that have not overridden the field.
func TestUpdate_VenueTranslationPatchKeepsOtherLanguagesAndSyncsDates(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	rec := &domain.EventRecurrence{
		ID: uuid.New(), RestaurantID: rid,
		Title:     "Cocktail Wednesday",
		Venue:     "Терраса",
		VenueI18n: domain.I18n{"ru": "Терраса", "en": "Terrace"},
	}
	repo.byID[rec.ID] = rec
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	in := validInput(rid)
	in.Venue = "Терраса"
	in.VenueI18n = domain.I18nPatch{"kk": strPtr("Террасса")}

	got, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rec.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.VenueI18n["en"] != "Terrace" {
		t.Errorf("VenueI18n[en] = %q, want the untouched English kept", got.VenueI18n["en"])
	}
	if got.VenueI18n["kk"] != "Террасса" {
		t.Errorf("VenueI18n[kk] = %q", got.VenueI18n["kk"])
	}
	if len(repo.contentSyncs) != 1 {
		t.Fatalf("a translated venue line must be pushed down onto the dates, got %d syncs", len(repo.contentSyncs))
	}
	if repo.contentSyncs[0].VenueI18n["kk"] != "Террасса" {
		t.Errorf("the synced content carries %v, want the new Kazakh venue line", repo.contentSyncs[0].VenueI18n)
	}
}

func TestUpdate_RejectsUnsupportedTranslationLanguage(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	rec := &domain.EventRecurrence{ID: uuid.New(), RestaurantID: rid, Title: "x"}
	repo.byID[rec.ID] = rec
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager))

	in := validInput(rid)
	in.VenueI18n = domain.I18nPatch{"fr": strPtr("Terrasse")}

	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, rec.ID, in); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation (→ 422), got %v", err)
	}
}

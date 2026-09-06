package platformpages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fake repository --------------------------------------------------------

type fakeRepo struct {
	rows      map[domain.PlatformPageSlug]domain.PlatformPage
	getErr    error
	updateErr error
	updates   int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[domain.PlatformPageSlug]domain.PlatformPage{}}
}

func (f *fakeRepo) Get(_ context.Context, slug domain.PlatformPageSlug) (*domain.PlatformPage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	row, ok := f.rows[slug]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := row
	return &cp, nil
}

func (f *fakeRepo) List(context.Context) ([]domain.PlatformPage, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	out := make([]domain.PlatformPage, 0, len(f.rows))
	for _, s := range domain.PlatformPageSlugs {
		if row, ok := f.rows[s]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRepo) Update(_ context.Context, p *domain.PlatformPage) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if _, ok := f.rows[p.Slug]; !ok {
		return domain.ErrNotFound
	}
	f.updates++
	p.UpdatedAt = time.Now()
	f.rows[p.Slug] = *p
	return nil
}

var _ domain.PlatformPageRepository = (*fakeRepo)(nil)

// --- fixtures ---------------------------------------------------------------

func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }
func venueUser() Actor  { return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant} }
func guest() Actor      { return Actor{UserID: uuid.New(), Role: domain.RoleUser} }

func ptr(s string) *string { return &s }
func bptr(b bool) *bool    { return &b }

// seeded mirrors migration 0105's seed: all seven slugs, empty body,
// unpublished — plus one already-published page ("offer") so read paths have
// something to actually serve.
func seeded() *fakeRepo {
	r := newFakeRepo()
	for _, s := range domain.PlatformPageSlugs {
		r.rows[s] = domain.PlatformPage{Slug: s, Title: string(s), Format: domain.PlatformPageFormatMarkdown}
	}
	published := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	r.rows[domain.PlatformPageOffer] = domain.PlatformPage{
		Slug:        domain.PlatformPageOffer,
		Title:       "Оферта",
		Body:        "# Оферта\n\nУсловия.",
		BodyI18n:    domain.I18n{"ru": "# Оферта\n\nУсловия.", "en": "# Offer\n\nTerms."},
		Format:      domain.PlatformPageFormatMarkdown,
		PublishedAt: &published,
	}
	return r
}

// --- GetPublished ------------------------------------------------------------

func TestGetPublished_HappyPath(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	p, err := u.GetPublished(context.Background(), "offer")
	if err != nil {
		t.Fatalf("GetPublished: %v", err)
	}
	if p.Body != "# Оферта\n\nУсловия." {
		t.Errorf("got body %q", p.Body)
	}
}

func TestGetPublished_UnpublishedIsNotFound(t *testing.T) {
	repo := seeded() // "privacy" exists but is unpublished (empty body, no PublishedAt)
	u := NewUseCase(repo)

	_, err := u.GetPublished(context.Background(), "privacy")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unpublished page, got %v", err)
	}
}

func TestGetPublished_UnknownSlugIsNotFound(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.GetPublished(context.Background(), "does-not-exist")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an unknown slug, got %v", err)
	}
}

// TestGetPublished_UnknownAndUnpublishedAreIndistinguishable pins the
// interface's documented promise: a guest probing random slugs must not be
// able to tell "no such page" apart from "exists but not published yet" —
// both have to fail exactly the same way (same sentinel, no extra code).
func TestGetPublished_UnknownAndUnpublishedAreIndistinguishable(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, errUnknown := u.GetPublished(context.Background(), "nope")
	_, errDraft := u.GetPublished(context.Background(), "privacy")

	if !errors.Is(errUnknown, domain.ErrNotFound) || !errors.Is(errDraft, domain.ErrNotFound) {
		t.Fatalf("both must be ErrNotFound, got unknown=%v draft=%v", errUnknown, errDraft)
	}
	if _, ok := domain.CodeOf(errUnknown); ok {
		t.Error("unknown slug must not carry a narrower code")
	}
	if _, ok := domain.CodeOf(errDraft); ok {
		t.Error("unpublished slug must not carry a narrower code")
	}
}

// --- ListAdmin / GetAdmin: authorization -------------------------------------

func TestListAdmin_RequiresSuperadmin(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	if _, err := u.ListAdmin(context.Background(), venueUser()); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("venue staff: want ErrForbidden, got %v", err)
	}
	if _, err := u.ListAdmin(context.Background(), guest()); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("guest: want ErrForbidden, got %v", err)
	}
	items, err := u.ListAdmin(context.Background(), superadmin())
	if err != nil {
		t.Fatalf("superadmin: %v", err)
	}
	if len(items) != len(domain.PlatformPageSlugs) {
		t.Fatalf("got %d pages, want all %d seeded", len(items), len(domain.PlatformPageSlugs))
	}
}

func TestGetAdmin_RequiresSuperadmin(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	if _, err := u.GetAdmin(context.Background(), venueUser(), "offer"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestGetAdmin_UnknownSlugIsNotFound(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	if _, err := u.GetAdmin(context.Background(), superadmin(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// --- Update: authorization ----------------------------------------------------

func TestUpdate_RequiresSuperadmin(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), venueUser(), "about", UpdateInput{Title: ptr("Новый заголовок")})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if repo.updates != 0 {
		t.Error("a forbidden write must not touch the repository")
	}
}

func TestUpdate_UnknownSlugIsNotFound(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), superadmin(), "nope", UpdateInput{Title: ptr("x")})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// --- Update: happy path -------------------------------------------------------

func TestUpdate_HappyPath_SetsBodyAndPublishes(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)
	actor := superadmin()

	out, err := u.Update(context.Background(), actor, "privacy", UpdateInput{
		Title:     ptr("Политика обработки данных"),
		Body:      ptr("# Политика\n\nТекст."),
		Published: bptr(true),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Title != "Политика обработки данных" || out.Body != "# Политика\n\nТекст." {
		t.Fatalf("fields not applied: %+v", out)
	}
	if !out.Published() {
		t.Fatal("want the page published")
	}
	if out.UpdatedBy == nil || *out.UpdatedBy != actor.UserID {
		t.Fatalf("want UpdatedBy = %s, got %v", actor.UserID, out.UpdatedBy)
	}
	// The invariant every *_i18n field in this schema keeps: i18n["ru"] equals
	// the plain column.
	if got := out.TitleI18n["ru"]; got != out.Title {
		t.Errorf("title_i18n[ru] = %q, want %q", got, out.Title)
	}

	// Re-reading through the same usecase (the public path) now finds it.
	pub, err := u.GetPublished(context.Background(), "privacy")
	if err != nil {
		t.Fatalf("GetPublished after publish: %v", err)
	}
	if pub.Body != out.Body {
		t.Errorf("public read got a different body: %q", pub.Body)
	}
}

// --- Update: publish-empty-body guard -----------------------------------------

func TestUpdate_PublishingWithEmptyBodyIsRejected(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), superadmin(), "jobs", UpdateInput{Published: bptr(true)})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodePageBodyEmpty {
		t.Fatalf("want code %q, got %q (ok=%v)", domain.CodePageBodyEmpty, code, ok)
	}
	if repo.updates != 0 {
		t.Error("a refused write must not touch the repository")
	}
}

func TestUpdate_PublishingWithWhitespaceOnlyBodyIsRejected(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), superadmin(), "jobs", UpdateInput{
		Body:      ptr("   \n\t  "),
		Published: bptr(true),
	})
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodePageBodyEmpty {
		t.Fatalf("want code %q, got %q (ok=%v, err=%v)", domain.CodePageBodyEmpty, code, ok, err)
	}
}

// Unpublishing (or leaving unpublished) with an empty body must NOT be
// refused — a fresh seeded row (empty body, draft) is exactly this state and
// must stay editable.
func TestUpdate_EmptyBodyIsFineWhileNotPublished(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	out, err := u.Update(context.Background(), superadmin(), "jobs", UpdateInput{Title: ptr("Вакансии BookEat")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Published() {
		t.Fatal("must remain unpublished")
	}
}

// --- Update: validation limits -------------------------------------------------

func TestUpdate_EmptyTitleIsRejected(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), superadmin(), "about", UpdateInput{Title: ptr("   ")})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

func TestUpdate_OverlongTitleIsRejected(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	long := strings.Repeat("a", domain.PlatformPageTitleMaxLen+1)
	_, err := u.Update(context.Background(), superadmin(), "about", UpdateInput{Title: ptr(long)})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

func TestUpdate_OverlongBodyIsRejected(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	long := strings.Repeat("a", domain.PlatformPageBodyMaxBytes+1)
	_, err := u.Update(context.Background(), superadmin(), "about", UpdateInput{Body: ptr(long)})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

// --- Update: partial i18n patch semantics --------------------------------------

// A patch that only sets Kazakh must not disturb an already-stored English
// translation — same "resend nothing you didn't change" contract as every
// other *_i18n write in this codebase (domain.I18nPatch).
func TestUpdate_I18nPatchIsPartial(t *testing.T) {
	repo := seeded()
	// Seed "offer" already carries an English translation (see seeded()).
	u := NewUseCase(repo)

	kk := "Оферта казакша"
	out, err := u.Update(context.Background(), superadmin(), "offer", UpdateInput{
		BodyI18n: domain.I18nPatch{"kk": &kk},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.BodyI18n["kk"] != kk {
		t.Errorf("kk not written: %+v", out.BodyI18n)
	}
	if out.BodyI18n["en"] != "# Offer\n\nTerms." {
		t.Errorf("existing en translation was disturbed: %+v", out.BodyI18n)
	}
}

func TestUpdate_I18nPatchRejectsUnsupportedLanguage(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	v := "??"
	_, err := u.Update(context.Background(), superadmin(), "offer", UpdateInput{
		TitleI18n: domain.I18nPatch{"fr": &v},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

// --- Update: already-published save does not move the timestamp ---------------

func TestUpdate_RepublishingAnAlreadyPublishedPageKeepsItsTimestamp(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	before := repo.rows[domain.PlatformPageOffer].PublishedAt
	out, err := u.Update(context.Background(), superadmin(), "offer", UpdateInput{Published: bptr(true)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !out.PublishedAt.Equal(*before) {
		t.Errorf("PublishedAt moved: before=%v after=%v", before, out.PublishedAt)
	}
}

// --- Update: unpublishing ------------------------------------------------------

func TestUpdate_UnpublishClearsPublishedAt(t *testing.T) {
	repo := seeded()
	u := NewUseCase(repo)

	out, err := u.Update(context.Background(), superadmin(), "offer", UpdateInput{Published: bptr(false)})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.Published() {
		t.Fatal("want unpublished")
	}

	if _, err := u.GetPublished(context.Background(), "offer"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("guest read after unpublish: want ErrNotFound, got %v", err)
	}
}

// --- Repository failure propagation --------------------------------------------

func TestUpdate_RepositoryErrorPropagates(t *testing.T) {
	repo := seeded()
	repo.updateErr = errors.New("boom")
	u := NewUseCase(repo)

	_, err := u.Update(context.Background(), superadmin(), "about", UpdateInput{Title: ptr("x")})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want the repository error to propagate, got %v", err)
	}
}

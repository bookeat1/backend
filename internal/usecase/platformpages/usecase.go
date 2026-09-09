// Package platformpages is the footer's editable text pages (spec
// "web-fixes-20260906.md" T4): "Как это работает", "Отмена брони", "Оферта",
// "Политика данных", "Контакты", "Вакансии" and "О BookEat". The set of pages
// is closed (domain.PlatformPageSlugs) — this package edits their content, it
// never creates or removes a page.
//
// Two audiences, three rules:
//   - the guest-facing site (GetPublished) sees a page only once it has been
//     published; an unpublished or unknown slug is ErrNotFound, never an
//     empty body;
//   - the cabinet (ListAdmin/GetAdmin/Update) is superadmin-only
//     (domain.CanManagePlatformContent) and always sees the row as it is
//     actually stored, published or not;
//   - there is no draft-over-published pair and no version history: Update
//     writes the live row directly, and a concurrent save from a second tab
//     simply wins if it lands last (spec T4, explicit).
package platformpages

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller of the admin routes. The public read has
// no actor at all — GetPublished is called before anyone signs in.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// UseCase is the platform-pages facade: the one public read plus the three
// admin operations. They are kept on a single interface (rather than split
// into their own files, see CLAUDE.md's usecase shape) because none of the
// four has enough independent logic to earn its own port — the pattern this
// package follows most closely is usecase/appversion, not usecase/auth.
type UseCase interface {
	// GetPublished is the public read: GET /pages/:slug. An unknown slug and
	// an existing-but-unpublished one are BOTH domain.ErrNotFound — the site
	// must not be able to tell "no such page" apart from "not published yet",
	// which would leak the fixed slug list to a guest probing random paths.
	GetPublished(ctx context.Context, slug string) (*domain.PlatformPage, error)
	// ListAdmin returns all seven pages for the cabinet's list screen.
	ListAdmin(ctx context.Context, actor Actor) ([]domain.PlatformPage, error)
	// GetAdmin returns one page as its owner sees it — every field, published
	// or not — for the cabinet's editor screen.
	GetAdmin(ctx context.Context, actor Actor, slug string) (*domain.PlatformPage, error)
	// Update merges a partial write onto one page and returns the stored
	// result.
	Update(ctx context.Context, actor Actor, slug string, in UpdateInput) (*domain.PlatformPage, error)
}

// UpdateInput is a PATCH-shaped write, same convention as
// usecase/appversion.SaveInput: every scalar is a pointer so "absent from the
// request" (keep the stored value) is distinguishable from "set it", and the
// *I18n fields are partial translation patches (domain.I18nPatch) so an editor
// filling in Kazakh does not have to resend Russian and English to keep them.
type UpdateInput struct {
	Title     *string
	TitleI18n domain.I18nPatch
	Body      *string
	BodyI18n  domain.I18nPatch
	// Published toggles domain.PlatformPage.PublishedAt: true sets it to now
	// (idempotent — saving an already-published page keeps it published, it
	// does not bump the timestamp), false clears it back to NULL. Absent
	// leaves the current published state untouched.
	Published *bool
}

type useCase struct {
	repo domain.PlatformPageRepository
}

// NewUseCase builds the platform-pages facade.
func NewUseCase(repo domain.PlatformPageRepository) UseCase {
	return &useCase{repo: repo}
}

// nowFunc is a package-level seam so a test can override the publish
// timestamp deterministically (`platformpages.nowFunc = func() time.Time {
// return fixed }`) without threading a clock through every constructor —
// there is exactly one caller of it, applyInput's Published branch.
var nowFunc = time.Now

func (u *useCase) GetPublished(ctx context.Context, rawSlug string) (*domain.PlatformPage, error) {
	slug := domain.PlatformPageSlug(strings.TrimSpace(rawSlug))
	if !domain.IsValidPlatformPageSlug(slug) {
		return nil, domain.ErrNotFound
	}
	p, err := u.repo.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !p.Published() {
		// Deliberately the SAME error as "unknown slug" above — see the
		// interface doc. Nothing in this response may distinguish the two.
		return nil, domain.ErrNotFound
	}
	return p, nil
}

func (u *useCase) ListAdmin(ctx context.Context, actor Actor) ([]domain.PlatformPage, error) {
	if err := requireManage(actor); err != nil {
		return nil, err
	}
	return u.repo.List(ctx)
}

func (u *useCase) GetAdmin(ctx context.Context, actor Actor, rawSlug string) (*domain.PlatformPage, error) {
	if err := requireManage(actor); err != nil {
		return nil, err
	}
	slug := domain.PlatformPageSlug(strings.TrimSpace(rawSlug))
	if !domain.IsValidPlatformPageSlug(slug) {
		return nil, domain.ErrNotFound
	}
	return u.repo.Get(ctx, slug)
}

func (u *useCase) Update(ctx context.Context, actor Actor, rawSlug string, in UpdateInput) (*domain.PlatformPage, error) {
	if err := requireManage(actor); err != nil {
		return nil, err
	}
	slug := domain.PlatformPageSlug(strings.TrimSpace(rawSlug))
	if !domain.IsValidPlatformPageSlug(slug) {
		return nil, domain.ErrNotFound
	}
	if err := validatePatches(in); err != nil {
		return nil, err
	}

	p, err := u.repo.Get(ctx, slug)
	if err != nil {
		return nil, err
	}

	applyInput(p, in)
	if err := validatePage(*p); err != nil {
		return nil, err
	}

	uid := actor.UserID
	p.UpdatedBy = &uid
	if err := u.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// requireManage is the defense-in-depth role re-check. The transport layer
// already mounts these routes behind RequireRole(domain.RoleAdmin); this makes
// sure a future re-mount on a wider group cannot hand page edits (including
// the offer and the privacy policy) to venue staff — same pattern as
// usecase/appversion.requirePlatform and usecase/cities.
func requireManage(actor Actor) error {
	if !domain.CanManagePlatformContent(actor.Role) {
		return fmt.Errorf("%w: only the platform may manage site pages", domain.ErrForbidden)
	}
	return nil
}

func validatePatches(in UpdateInput) error {
	for field, patch := range map[string]domain.I18nPatch{
		"title_i18n": in.TitleI18n,
		"body_i18n":  in.BodyI18n,
	} {
		if err := patch.Validate(field); err != nil {
			return err
		}
	}
	return nil
}

// applyInput merges the patch onto the stored page. Text fields go through
// domain.ApplyTranslations, which re-establishes the invariant every other
// *_i18n field in this schema keeps: the plain column and i18n["ru"] are the
// same Russian text.
func applyInput(p *domain.PlatformPage, in UpdateInput) {
	if in.Title != nil {
		p.Title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		// Body is Markdown prose: trimming only the outer whitespace, never
		// the internal formatting, and NOT trimmed at all when absent from
		// the request (an admin who only flips "Опубликовано" must not have
		// their body silently re-trimmed by a save they did not make to it).
		p.Body = strings.Trim(*in.Body, "\n\r\t ")
	}
	p.TitleI18n = domain.ApplyTranslations(p.TitleI18n, in.TitleI18n, p.Title)
	p.BodyI18n = domain.ApplyTranslations(p.BodyI18n, in.BodyI18n, p.Body)

	if in.Published != nil {
		switch {
		case *in.Published && p.PublishedAt == nil:
			t := nowFunc()
			p.PublishedAt = &t
		case !*in.Published:
			p.PublishedAt = nil
		}
		// *in.Published == true && already published: no-op, see the
		// UpdateInput doc — saving an already-published page must not move
		// its publish timestamp.
	}
}

// validatePage refuses a write that would be dishonest or unusable at read
// time: an empty title (the admin list and the site's <title> both need one),
// an over-long field, a body over the spec's 200 000-byte cap, or switching
// "Опубликовано" on with nothing to show for it.
func validatePage(p domain.PlatformPage) error {
	if p.Title == "" {
		return fmt.Errorf("%w: title must not be empty", domain.ErrValidation)
	}
	if err := checkRuneLen("title", p.Title, domain.PlatformPageTitleMaxLen); err != nil {
		return err
	}
	for lang, v := range p.TitleI18n {
		if err := checkRuneLen("title_i18n."+lang, v, domain.PlatformPageTitleMaxLen); err != nil {
			return err
		}
	}
	if err := checkByteLen("body", p.Body, domain.PlatformPageBodyMaxBytes); err != nil {
		return err
	}
	for lang, v := range p.BodyI18n {
		if err := checkByteLen("body_i18n."+lang, v, domain.PlatformPageBodyMaxBytes); err != nil {
			return err
		}
	}
	if p.Published() && strings.TrimSpace(p.Body) == "" {
		return domain.WithCode(domain.CodePageBodyEmpty,
			fmt.Errorf("%w: body must not be empty while published", domain.ErrValidation))
	}
	return nil
}

func checkRuneLen(field, v string, limit int) error {
	if len([]rune(v)) > limit {
		return fmt.Errorf("%w: %s is longer than %d characters", domain.ErrValidation, field, limit)
	}
	return nil
}

func checkByteLen(field, v string, limit int) error {
	if len(v) > limit {
		return fmt.Errorf("%w: %s is longer than %d bytes", domain.ErrValidation, field, limit)
	}
	return nil
}

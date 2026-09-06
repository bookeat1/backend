package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PlatformPageSlug identifies one of the platform's editable text pages
// (migration 0105) — the seven footer links Дамир asked to edit from the
// cabinet without a deploy ("Как это работает", "Отмена брони", "Оферта",
// "Политика данных", "Контакты", "Вакансии", "О BookEat"). "Блог" is NOT one
// of these: it links to the existing /articles feed and has no row here.
type PlatformPageSlug string

// The fixed, closed allowlist. It is closed on purpose (spec T4, "вне
// скоупа": произвольные слаги/новые страницы из кабинета) — the seven rows
// are seeded by migration 0105 and never created or deleted by the app, so a
// write can only ever update one of these, never invent an eighth page.
const (
	PlatformPageAbout        PlatformPageSlug = "about"
	PlatformPageJobs         PlatformPageSlug = "jobs"
	PlatformPageContacts     PlatformPageSlug = "contacts"
	PlatformPageHowItWorks   PlatformPageSlug = "how-it-works"
	PlatformPageCancellation PlatformPageSlug = "cancellation"
	PlatformPageOffer        PlatformPageSlug = "offer"
	PlatformPagePrivacy      PlatformPageSlug = "privacy"
)

// PlatformPageSlugs lists every valid slug in the order the admin "Страницы
// сайта" screen shows them (footer's "Компания" then "Помощь" columns, minus
// "Блог"). ListAdmin returns rows in this order rather than the database's
// insertion order, so the screen has a stable, predictable layout.
var PlatformPageSlugs = []PlatformPageSlug{
	PlatformPageAbout,
	PlatformPageJobs,
	PlatformPageContacts,
	PlatformPageHowItWorks,
	PlatformPageCancellation,
	PlatformPageOffer,
	PlatformPagePrivacy,
}

// IsValidPlatformPageSlug reports whether slug is one of the seven allowed
// pages. Unlike most of this schema's free-text fields, an unknown slug here
// is refused rather than silently accepted: the set is closed by design.
func IsValidPlatformPageSlug(slug PlatformPageSlug) bool {
	for _, s := range PlatformPageSlugs {
		if s == slug {
			return true
		}
	}
	return false
}

// Column limits from migration 0105. Checked in the usecase so an over-long
// write comes back as a 422 naming the field instead of a Postgres error.
const (
	PlatformPageTitleMaxLen = 200
	// PlatformPageBodyMaxBytes is measured in BYTES, not runes: the column is
	// `text` with no declared limit, but the spec caps the Markdown body at
	// 200 000 bytes so an editor cannot paste something the app never
	// expected to render.
	PlatformPageBodyMaxBytes = 200_000
)

// PlatformPageFormatMarkdown is the only body format this feature knows. It
// is stored as a column (not hardcoded in every reader) purely so a future
// format never needs a migration to become expressible — see 0105's
// comment — but today's only valid value is this one.
const PlatformPageFormatMarkdown = "markdown"

// PlatformPage is one row of platform_pages: a footer text page edited by the
// superadmin (domain.CanManagePlatformContent) and read by the guest-facing
// site.
//
// There is no draft/published pair of rows and no version history (spec T4,
// 🟡 "черновик поверх опубликованного" — decided: no). PublishedAt is the
// WHOLE state machine: NULL = draft (the site answers "page not found" for
// it), non-NULL = published (the site serves Body as it stands right now). A
// save that flips PublishedAt from set back to NULL, or that overwrites
// Body while already published, takes effect for the guest immediately —
// that is the deliberately simple model the spec asked for.
type PlatformPage struct {
	Slug        PlatformPageSlug
	Title       string
	TitleI18n   I18n
	Body        string
	BodyI18n    I18n
	Format      string
	PublishedAt *time.Time
	UpdatedBy   *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Published reports whether a guest may currently be served this page.
func (p PlatformPage) Published() bool { return p.PublishedAt != nil }

// PlatformPageRepository persists platform_pages. The row set itself is fixed
// (seeded by migration 0105, never created or deleted by the app), so unlike
// most repositories in this codebase there is no Create/Delete — only Get,
// List and Update.
type PlatformPageRepository interface {
	// Get reads one page by slug, regardless of its published state — the
	// caller (usecase) decides what "unpublished" means for it. ErrNotFound
	// when the slug is not one of the seeded rows (should not happen for a
	// slug that passed IsValidPlatformPageSlug against a correctly migrated
	// database, but a repository must not assume its caller always validates
	// first).
	Get(ctx context.Context, slug PlatformPageSlug) (*PlatformPage, error)
	// List returns every page, ordered by slug, for the admin screen.
	List(ctx context.Context) ([]PlatformPage, error)
	// Update writes the whole row. The caller has already merged its patch
	// onto a value read via Get, so this is a full write, not a
	// read-modify-write that could revert a field a concurrent save just
	// wrote (last-write-wins, as decided in spec T4 — "два редактора правят
	// одну страницу — побеждает последнее сохранение"). ErrNotFound if the
	// slug names no row (see Get).
	Update(ctx context.Context, p *PlatformPage) error
}

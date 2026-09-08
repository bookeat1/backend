package gastroguide

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The editor side of the guide. Everything here is SUPERADMIN-ONLY.
//
// Why not the venue RBAC matrix (PermRestaurantManage), which events, promos
// and the feed's venue side use: those objects belong to a restaurant, and their
// owner is the right person to edit them. A guide collection belongs to the
// PLATFORM — it is our editorial opinion about which venues are worth eating at.
// A restaurant owner who could touch it could put themselves into "лучшие
// завтраки", which is precisely the thing the guide's value depends on not
// happening. So the gate is the same one the payout-generation and feed
// moderation endpoints use: middleware.RequireRole(domain.RoleAdmin) at the
// router, re-checked here as defense-in-depth.

// EditorActor is the authenticated caller of an editor operation.
type EditorActor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// slugPattern is what a client-facing slug may look like: lowercase latin,
// digits and single hyphens. Slugs end up in URLs the app builds, so a slug with
// a space or a cyrillic letter is a broken link that only shows up in
// production.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxSlugLen / maxTitleLen keep a paste accident out of the database. Both are
// generous: nothing in the guide is meant to be long.
const (
	maxSlugLen  = 120
	maxTitleLen = 200
)

// Editor exposes the guide's write operations plus the cabinet reads (which show
// drafts and therefore cannot come from the guest facade).
type Editor interface {
	// --- categories ---
	ListCategories(ctx context.Context, actor EditorActor) ([]domain.GuideCategory, error)
	CreateCategory(ctx context.Context, actor EditorActor, in CategoryInput) (*domain.GuideCategory, error)
	UpdateCategory(ctx context.Context, actor EditorActor, id uuid.UUID, in CategoryInput) (*domain.GuideCategory, error)

	// --- collections ---
	ListCollections(ctx context.Context, actor EditorActor, in AdminListInput) ([]domain.GuideCollection, int, error)
	GetCollection(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollectionAdminDetail, error)
	CreateCollection(ctx context.Context, actor EditorActor, in CollectionInput) (*domain.GuideCollection, error)
	UpdateCollection(ctx context.Context, actor EditorActor, id uuid.UUID, in CollectionInput) (*domain.GuideCollection, error)
	// Publish takes a collection live. publishedAt nil means "now"; a future
	// time is a scheduled publication and is allowed on purpose.
	Publish(ctx context.Context, actor EditorActor, id uuid.UUID, publishedAt *time.Time) (*domain.GuideCollection, error)
	// Unpublish returns a collection to draft and CLEARS published_at.
	Unpublish(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollection, error)
	// Archive withdraws a collection, keeping its venue links.
	Archive(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollection, error)

	// --- membership ---
	SetCategories(ctx context.Context, actor EditorActor, collectionID uuid.UUID, categoryIDs []uuid.UUID) error
	AttachVenue(ctx context.Context, actor EditorActor, collectionID uuid.UUID, in AttachVenueInput) error
	// SetVenueHighlight ставит или снимает событие/акцию у уже добавленного
	// заведения. Оба nil — снять подсветку.
	SetVenueHighlight(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, eventID, promoID *uuid.UUID) error
	DetachVenue(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID) error
	SetVenueNote(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, note string, noteI18n domain.I18nPatch) error
	// ReorderVenues writes the intended FINAL order of the collection's venues.
	ReorderVenues(ctx context.Context, actor EditorActor, collectionID uuid.UUID, restaurantIDs []uuid.UUID) error
}

// CategoryInput is a rubric's editable fields as they arrive from the cabinet.
type CategoryInput struct {
	Slug  string
	Title string
	// TitleI18n is a PARTIAL translation update (domain.I18nPatch) and the one
	// field here that is not a full replace: a named language is written, a
	// null (or blank) one is removed, and a language the object does not
	// mention keeps whatever is stored. Two editors with the same form open no
	// longer overwrite each other's language.
	//
	// Title is the Russian text: it always wins over a `ru` key in the map, and
	// the merge re-establishes i18n["ru"] == Title (domain.ApplyTranslations).
	TitleI18n domain.I18nPatch
	Position  int
	IsActive  bool
}

// validateTranslations refuses a rubric's translation patch. Called BEFORE
// anything is read or written, so an unsupported language is a 422 whatever
// the id turns out to point at.
func (in CategoryInput) validateTranslations() error {
	return in.TitleI18n.Validate("title_i18n")
}

// CollectionInput is a collection's editable fields as they arrive from the
// cabinet. Status is absent by design — see Publish/Unpublish/Archive.
type CollectionInput struct {
	Slug  string
	Title string
	// The *I18n maps are PARTIAL translation updates — see CategoryInput. The
	// plain field next to each one is its Russian text and wins over a `ru`
	// key in the map.
	TitleI18n       domain.I18nPatch
	Subtitle        string
	SubtitleI18n    domain.I18nPatch
	Description     string
	DescriptionI18n domain.I18nPatch
	CoverImageURL   *string
	City            *domain.City
	// Kind is "collection" or "article". EMPTY means "collection": an admin
	// build that predates migration 0096 does not send the field, and its
	// creates must keep producing what they always produced. An unknown value
	// is a 422 (CodeGuideUnknownKind), never coerced.
	Kind     domain.GuideCollectionKind
	Position int
}

// validateTranslations refuses a collection's translation patches before
// anything is read or written.
func (in CollectionInput) validateTranslations() error {
	if err := in.TitleI18n.Validate("title_i18n"); err != nil {
		return err
	}
	if err := in.SubtitleI18n.Validate("subtitle_i18n"); err != nil {
		return err
	}
	return in.DescriptionI18n.Validate("description_i18n")
}

// AdminListInput narrows the cabinet's collection listing.
type AdminListInput struct {
	Statuses []domain.GuideCollectionStatus
	City     *domain.City
	// Kind narrows the cabinet listing to collections or to articles. Nil means
	// both.
	Kind    *domain.GuideCollectionKind
	Query   string
	Page    int
	PerPage int
}

// AttachVenueInput puts one venue into a collection, at the end.
type AttachVenueInput struct {
	RestaurantID uuid.UUID
	Note         string
	// NoteI18n is a PARTIAL translation update — see CategoryInput. On an
	// attach there is nothing stored yet, so it starts from an empty map.
	NoteI18n domain.I18nPatch
	// EventID / PromoID — необязательная подсветка блока: событие ИЛИ акция.
	EventID *uuid.UUID
	PromoID *uuid.UUID
}

type editor struct {
	repo  domain.GastroguideEditorRepository
	clock func() time.Time
}

// NewEditor constructs the gastroguide editor usecase.
func NewEditor(repo domain.GastroguideEditorRepository) Editor {
	return &editor{repo: repo, clock: time.Now}
}

// authorize is the single superadmin gate, called FIRST by every method. It
// duplicates middleware.RequireRole(domain.RoleAdmin) at the router on purpose:
// the day somebody mounts these routes on the wrong group, the usecase still
// refuses.
func (e *editor) authorize(a EditorActor) error {
	if a.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: the gastroguide editor is superadmin-only", domain.ErrForbidden)
	}
	return nil
}

// --- categories ---

func (e *editor) ListCategories(ctx context.Context, actor EditorActor) ([]domain.GuideCategory, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	return e.repo.ListAllCategories(ctx)
}

func (e *editor) CreateCategory(ctx context.Context, actor EditorActor, in CategoryInput) (*domain.GuideCategory, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateCategory(in, nil)
	if err != nil {
		return nil, err
	}
	return e.repo.CreateCategory(ctx, w)
}

// UpdateCategory reads the rubric before writing it, because title_i18n is a
// PARTIAL update: the stored map is one half of the result and the request is
// the other. Without the read, "I did not mention English" and "delete English"
// would be the same request — which is the bug this replaced.
func (e *editor) UpdateCategory(ctx context.Context, actor EditorActor, id uuid.UUID, in CategoryInput) (*domain.GuideCategory, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	if err := in.validateTranslations(); err != nil {
		return nil, err
	}
	current, err := e.repo.GetCategory(ctx, id)
	if err != nil {
		return nil, err
	}
	w, err := validateCategory(in, current.TitleI18n)
	if err != nil {
		return nil, err
	}
	return e.repo.UpdateCategory(ctx, id, w)
}

// --- collections ---

func (e *editor) ListCollections(ctx context.Context, actor EditorActor, in AdminListInput) ([]domain.GuideCollection, int, error) {
	if err := e.authorize(actor); err != nil {
		return nil, 0, err
	}
	for _, s := range in.Statuses {
		if !s.Valid() {
			return nil, 0, fmt.Errorf("%w: unknown collection status %q", domain.ErrValidation, s)
		}
	}
	if in.City != nil && !in.City.Valid() {
		return nil, 0, domain.WithCode(domain.CodeCityRequired,
			fmt.Errorf("%w: unknown city", domain.ErrValidation))
	}
	if in.Kind != nil && !in.Kind.Valid() {
		return nil, 0, domain.WithCode(domain.CodeGuideUnknownKind,
			fmt.Errorf("%w: unknown collection kind %q", domain.ErrValidation, *in.Kind))
	}
	return e.repo.ListCollectionsAdmin(ctx, domain.GuideCollectionAdminFilter{
		Statuses: in.Statuses, City: in.City, Kind: in.Kind, Query: in.Query,
		Page: in.Page, PerPage: in.PerPage,
	})
}

func (e *editor) GetCollection(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollectionAdminDetail, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	return e.repo.GetCollectionAdmin(ctx, id)
}

func (e *editor) CreateCollection(ctx context.Context, actor EditorActor, in CollectionInput) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateCollection(in, nil)
	if err != nil {
		return nil, err
	}
	return e.repo.CreateCollection(ctx, w)
}

// UpdateCollection replaces the editable fields, and refuses to turn an item
// that carries rubrics into an article.
//
// The refusal is here and not only in SQL on purpose: the alternative — writing
// kind='article' and quietly deleting the rubric links — is a destructive edit
// the editor never asked for and would not see in the response. Making them
// detach the rubrics first costs one extra call and keeps the deletion an
// explicit act.
//
// The collection is now read UNCONDITIONALLY, where before it was read only to
// check the rubrics of an article: its stored translation maps are half of what
// the write produces, because the `*_i18n` objects in the payload are partial
// patches. The read that the article check needed is the same one, so this
// costs no extra query on that path.
func (e *editor) UpdateCollection(ctx context.Context, actor EditorActor, id uuid.UUID, in CollectionInput) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	if err := in.validateTranslations(); err != nil {
		return nil, err
	}
	current, err := e.repo.GetCollectionAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	w, err := validateCollection(in, &current.GuideCollection)
	if err != nil {
		return nil, err
	}
	if w.Kind == domain.GuideKindArticle && len(current.Categories) > 0 {
		return nil, domain.WithCode(domain.CodeGuideArticleHasRubrics,
			fmt.Errorf("%w: an article carries no rubrics — detach %d rubric(s) first",
				domain.ErrValidation, len(current.Categories)))
	}
	return e.repo.UpdateCollection(ctx, id, w)
}

// Publish takes a collection live, and refuses to do so when the result would be
// broken.
//
// One precondition: published_at must exist. The DB CHECK says so; we supply
// `now` when the editor did not name a time, so "опубликовать" means what it
// says. A time in the FUTURE is accepted — that is scheduled publication, and
// the guest predicate (published_at <= now) already implements it.
//
// A collection with NO venues publishes fine. It used to be refused, because the
// guest listing hid such collections and "published but invisible" is a lie. The
// listing no longer hides them: an editorial piece about places that are not in
// the catalog is content in its own right, and it is what brings a guest back to
// the app. So the refusal lost its reason to exist along with the rule it
// guarded.
func (e *editor) Publish(ctx context.Context, actor EditorActor, id uuid.UUID, publishedAt *time.Time) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	current, err := e.repo.GetCollectionAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(current.Title) == "" || strings.TrimSpace(current.Slug) == "" {
		return nil, fmt.Errorf("%w: a published collection needs a slug and a title", domain.ErrValidation)
	}
	at := e.clock()
	if publishedAt != nil {
		at = *publishedAt
	}
	return e.repo.SetCollectionStatus(ctx, id, domain.GuideCollectionPublished, &at)
}

// Unpublish returns a collection to draft and clears published_at, so a later
// re-publish gets a fresh date instead of silently claiming the collection has
// been live since whenever it first was.
func (e *editor) Unpublish(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	return e.repo.SetCollectionStatus(ctx, id, domain.GuideCollectionDraft, nil)
}

// Archive withdraws a collection but KEEPS published_at: an archived collection
// is one that was live, and losing the date would lose that fact. The guest
// predicate is status-first, so an archived row with a date is invisible.
func (e *editor) Archive(ctx context.Context, actor EditorActor, id uuid.UUID) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	current, err := e.repo.GetCollectionAdmin(ctx, id)
	if err != nil {
		return nil, err
	}
	return e.repo.SetCollectionStatus(ctx, id, domain.GuideCollectionArchived, current.PublishedAt)
}

// --- membership ---

// SetCategories replaces a collection's whole rubric set. Attaching a rubric to
// an ARTICLE is refused: rubrics are what a collection is, and an article that
// carried one would show up in the guide's rubric navigation, which is exactly
// the thing migration 0096 separates. Detaching (an empty list) stays legal for
// either kind — that is how a collection is turned into an article.
func (e *editor) SetCategories(ctx context.Context, actor EditorActor, collectionID uuid.UUID, categoryIDs []uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	if len(categoryIDs) > 0 {
		current, err := e.repo.GetCollectionAdmin(ctx, collectionID)
		if err != nil {
			return err
		}
		if current.Kind == domain.GuideKindArticle {
			return domain.WithCode(domain.CodeGuideArticleHasRubrics,
				fmt.Errorf("%w: an article carries no rubrics", domain.ErrValidation))
		}
	}
	seen := make(map[uuid.UUID]bool, len(categoryIDs))
	for _, id := range categoryIDs {
		if seen[id] {
			return fmt.Errorf("%w: category %s is listed twice", domain.ErrValidation, id)
		}
		seen[id] = true
	}
	return e.repo.SetCollectionCategories(ctx, collectionID, categoryIDs)
}

func (e *editor) AttachVenue(ctx context.Context, actor EditorActor, collectionID uuid.UUID, in AttachVenueInput) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	if in.RestaurantID == uuid.Nil {
		return fmt.Errorf("%w: restaurant_id is required", domain.ErrValidation)
	}
	if in.EventID != nil && in.PromoID != nil {
		return fmt.Errorf("%w: a block may highlight an event or a promo, not both", domain.ErrValidation)
	}
	if err := in.NoteI18n.Validate("note_i18n"); err != nil {
		return err
	}
	note := strings.TrimSpace(in.Note)
	return e.repo.AttachVenue(ctx, collectionID, domain.GuideVenueAttachment{
		RestaurantID: in.RestaurantID,
		Note:         note,
		NoteI18n:     domain.ApplyTranslations(nil, in.NoteI18n, note),
		EventID:      in.EventID,
		PromoID:      in.PromoID,
	})
}

func (e *editor) SetVenueHighlight(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, eventID, promoID *uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	if eventID != nil && promoID != nil {
		return fmt.Errorf("%w: a block may highlight an event or a promo, not both", domain.ErrValidation)
	}
	return e.repo.SetVenueHighlight(ctx, collectionID, restaurantID, eventID, promoID)
}

func (e *editor) DetachVenue(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	return e.repo.DetachVenue(ctx, collectionID, restaurantID)
}

// SetVenueNote rewrites the editor's line under one venue's card. note_i18n is
// a PARTIAL update, so the note's stored translations are read first — they are
// half of the result.
//
// The venue is looked up in the collection's admin detail rather than through a
// dedicated read: it is the same query the cabinet screen itself runs, and a
// restaurant that is not in this collection has to be ErrNotFound here anyway
// (the repository would report the same thing from its zero rows affected).
func (e *editor) SetVenueNote(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, note string, noteI18n domain.I18nPatch) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	if err := noteI18n.Validate("note_i18n"); err != nil {
		return err
	}
	current, err := e.repo.GetCollectionAdmin(ctx, collectionID)
	if err != nil {
		return err
	}
	var stored domain.I18n
	found := false
	for _, v := range current.Venues {
		if v.RestaurantID == restaurantID {
			stored, found = v.NoteI18n, true
			break
		}
	}
	if !found {
		return fmt.Errorf("set guide venue note: %w", domain.ErrNotFound)
	}
	trimmed := strings.TrimSpace(note)
	return e.repo.UpdateVenueNote(ctx, collectionID, restaurantID, trimmed,
		domain.ApplyTranslations(stored, noteI18n, trimmed))
}

// ReorderVenues hands the intended final order straight to the repository, which
// checks it against the current membership and writes it in one transaction. The
// usecase adds only the authorization and the cheap "obviously wrong" checks, so
// the membership comparison happens exactly once, under the row lock, and cannot
// go stale between a usecase-level check and the write.
func (e *editor) ReorderVenues(ctx context.Context, actor EditorActor, collectionID uuid.UUID, restaurantIDs []uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	for _, id := range restaurantIDs {
		if id == uuid.Nil {
			return domain.WithCode(domain.CodeGuideOrderMismatch,
				fmt.Errorf("%w: the order contains an empty restaurant id", domain.ErrValidation))
		}
	}
	return e.repo.ReorderVenues(ctx, collectionID, restaurantIDs)
}

// --- validation ---

// validateCategory turns the cabinet's payload into the row to write. base is
// the rubric's CURRENTLY STORED title_i18n (nil on create) — the patch is
// merged onto it, and a language neither side mentions (including the ko/zh
// rows the old import left behind) survives untouched.
func validateCategory(in CategoryInput, base domain.I18n) (domain.GuideCategoryWrite, error) {
	if err := in.validateTranslations(); err != nil {
		return domain.GuideCategoryWrite{}, err
	}
	slug, err := normalizeSlug(in.Slug)
	if err != nil {
		return domain.GuideCategoryWrite{}, err
	}
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return domain.GuideCategoryWrite{}, err
	}
	return domain.GuideCategoryWrite{
		Slug: slug, Title: title,
		TitleI18n: domain.ApplyTranslations(base, in.TitleI18n, title),
		Position:  in.Position, IsActive: in.IsActive,
	}, nil
}

// validateCollection turns the cabinet's payload into the row to write. base is
// the collection as it is STORED (nil on create): its translation maps are what
// the partial patches are merged onto.
func validateCollection(in CollectionInput, base *domain.GuideCollection) (domain.GuideCollectionWrite, error) {
	if err := in.validateTranslations(); err != nil {
		return domain.GuideCollectionWrite{}, err
	}
	slug, err := normalizeSlug(in.Slug)
	if err != nil {
		return domain.GuideCollectionWrite{}, err
	}
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return domain.GuideCollectionWrite{}, err
	}
	if in.City != nil && !in.City.Valid() {
		return domain.GuideCollectionWrite{}, domain.WithCode(domain.CodeCityRequired,
			fmt.Errorf("%w: unknown city", domain.ErrValidation))
	}
	cover := in.CoverImageURL
	if cover != nil {
		trimmed := strings.TrimSpace(*cover)
		// An empty string is "no cover", not a cover whose URL is "". The guest
		// response omits a nil cover and would otherwise emit "" and make the
		// app render a broken image.
		if trimmed == "" {
			cover = nil
		} else {
			cover = &trimmed
		}
	}
	// An omitted kind is a collection: the field arrived with migration 0096,
	// and every admin build older than it posts a collection without saying so.
	kind := in.Kind
	if kind == "" {
		kind = domain.GuideKindCollection
	}
	if !kind.Valid() {
		return domain.GuideCollectionWrite{}, domain.WithCode(domain.CodeGuideUnknownKind,
			fmt.Errorf("%w: unknown collection kind %q", domain.ErrValidation, in.Kind))
	}
	var baseTitle, baseSubtitle, baseDescription domain.I18n
	if base != nil {
		baseTitle, baseSubtitle, baseDescription = base.TitleI18n, base.SubtitleI18n, base.DescriptionI18n
	}
	subtitle := strings.TrimSpace(in.Subtitle)
	description := strings.TrimSpace(in.Description)
	return domain.GuideCollectionWrite{
		Slug: slug, Title: title,
		TitleI18n:       domain.ApplyTranslations(baseTitle, in.TitleI18n, title),
		Subtitle:        subtitle,
		SubtitleI18n:    domain.ApplyTranslations(baseSubtitle, in.SubtitleI18n, subtitle),
		Description:     description,
		DescriptionI18n: domain.ApplyTranslations(baseDescription, in.DescriptionI18n, description),
		CoverImageURL:   cover, City: in.City, Kind: kind, Position: in.Position,
	}, nil
}

func normalizeSlug(raw string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if slug == "" {
		return "", fmt.Errorf("%w: slug is required", domain.ErrValidation)
	}
	if len(slug) > maxSlugLen {
		return "", fmt.Errorf("%w: slug is longer than %d characters", domain.ErrValidation, maxSlugLen)
	}
	if !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("%w: slug may contain only latin letters, digits and single hyphens", domain.ErrValidation)
	}
	return slug, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if len([]rune(title)) > maxTitleLen {
		return "", fmt.Errorf("%w: title is longer than %d characters", domain.ErrValidation, maxTitleLen)
	}
	return title, nil
}

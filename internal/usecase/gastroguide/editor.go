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
	DetachVenue(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID) error
	SetVenueNote(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, note string, noteI18n domain.I18n) error
	// ReorderVenues writes the intended FINAL order of the collection's venues.
	ReorderVenues(ctx context.Context, actor EditorActor, collectionID uuid.UUID, restaurantIDs []uuid.UUID) error
}

// CategoryInput is a rubric's editable fields as they arrive from the cabinet.
type CategoryInput struct {
	Slug      string
	Title     string
	TitleI18n domain.I18n
	Position  int
	IsActive  bool
}

// CollectionInput is a collection's editable fields as they arrive from the
// cabinet. Status is absent by design — see Publish/Unpublish/Archive.
type CollectionInput struct {
	Slug            string
	Title           string
	TitleI18n       domain.I18n
	Subtitle        string
	SubtitleI18n    domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	CoverImageURL   *string
	City            *domain.City
	Position        int
}

// AdminListInput narrows the cabinet's collection listing.
type AdminListInput struct {
	Statuses []domain.GuideCollectionStatus
	City     *domain.City
	Query    string
	Page     int
	PerPage  int
}

// AttachVenueInput puts one venue into a collection, at the end.
type AttachVenueInput struct {
	RestaurantID uuid.UUID
	Note         string
	NoteI18n     domain.I18n
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
	w, err := validateCategory(in)
	if err != nil {
		return nil, err
	}
	return e.repo.CreateCategory(ctx, w)
}

func (e *editor) UpdateCategory(ctx context.Context, actor EditorActor, id uuid.UUID, in CategoryInput) (*domain.GuideCategory, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateCategory(in)
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
	return e.repo.ListCollectionsAdmin(ctx, domain.GuideCollectionAdminFilter{
		Statuses: in.Statuses, City: in.City, Query: in.Query,
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
	w, err := validateCollection(in)
	if err != nil {
		return nil, err
	}
	return e.repo.CreateCollection(ctx, w)
}

func (e *editor) UpdateCollection(ctx context.Context, actor EditorActor, id uuid.UUID, in CollectionInput) (*domain.GuideCollection, error) {
	if err := e.authorize(actor); err != nil {
		return nil, err
	}
	w, err := validateCollection(in)
	if err != nil {
		return nil, err
	}
	return e.repo.UpdateCollection(ctx, id, w)
}

// Publish takes a collection live, and refuses to do so when the result would be
// invisible or broken.
//
// Two preconditions, both of them things the guest read enforces anyway:
//
//   - published_at must exist. The DB CHECK says so; we supply `now` when the
//     editor did not name a time, so "опубликовать" means what it says. A time
//     in the FUTURE is accepted — that is scheduled publication, and the guest
//     predicate (published_at <= now) already implements it.
//   - the collection must hold at least one ACTIVE venue. The guest listing
//     filters out collections with no guest-visible venue, so publishing an
//     empty one produces a collection that is "published" everywhere in the
//     cabinet and absent from the app, with nothing to point at. Refusing with
//     CodeGuideCollectionEmpty tells the editor exactly what to do.
//
// A collection whose venues are all currently DEACTIVATED fails the second
// check. That is the honest answer: right now there is nothing to show.
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
	n, err := e.repo.CountActiveVenues(ctx, id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, domain.WithCode(domain.CodeGuideCollectionEmpty,
			fmt.Errorf("%w: a collection with no active venue would be published and invisible", domain.ErrValidation))
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

func (e *editor) SetCategories(ctx context.Context, actor EditorActor, collectionID uuid.UUID, categoryIDs []uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
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
	return e.repo.AttachVenue(ctx, collectionID, domain.GuideVenueAttachment{
		RestaurantID: in.RestaurantID,
		Note:         strings.TrimSpace(in.Note),
		NoteI18n:     in.NoteI18n,
	})
}

func (e *editor) DetachVenue(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	return e.repo.DetachVenue(ctx, collectionID, restaurantID)
}

func (e *editor) SetVenueNote(ctx context.Context, actor EditorActor, collectionID, restaurantID uuid.UUID, note string, noteI18n domain.I18n) error {
	if err := e.authorize(actor); err != nil {
		return err
	}
	return e.repo.UpdateVenueNote(ctx, collectionID, restaurantID, strings.TrimSpace(note), noteI18n)
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

func validateCategory(in CategoryInput) (domain.GuideCategoryWrite, error) {
	slug, err := normalizeSlug(in.Slug)
	if err != nil {
		return domain.GuideCategoryWrite{}, err
	}
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return domain.GuideCategoryWrite{}, err
	}
	return domain.GuideCategoryWrite{
		Slug: slug, Title: title, TitleI18n: cleanI18n(in.TitleI18n),
		Position: in.Position, IsActive: in.IsActive,
	}, nil
}

func validateCollection(in CollectionInput) (domain.GuideCollectionWrite, error) {
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
	return domain.GuideCollectionWrite{
		Slug: slug, Title: title, TitleI18n: cleanI18n(in.TitleI18n),
		Subtitle: strings.TrimSpace(in.Subtitle), SubtitleI18n: cleanI18n(in.SubtitleI18n),
		Description: strings.TrimSpace(in.Description), DescriptionI18n: cleanI18n(in.DescriptionI18n),
		CoverImageURL: cover, City: in.City, Position: in.Position,
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

// cleanI18n drops empty translations so an editor clearing a language field does
// not leave {"kk": ""} behind — I18n.Resolve would then answer with an empty
// string instead of falling back to the base ru column.
func cleanI18n(m domain.I18n) domain.I18n {
	if len(m) == 0 {
		return nil
	}
	out := make(domain.I18n, len(m))
	for k, v := range m {
		if s := strings.TrimSpace(v); s != "" {
			out[strings.ToLower(strings.TrimSpace(k))] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

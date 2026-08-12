package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The editor side of the gastroguide (migration 0061). The guest read model
// lives in gastroguide.go and is deliberately read-only; everything here WRITES,
// and every one of these operations is superadmin-only — the guide is platform
// editorial content, not a venue's own page, so a restaurant owner must never be
// able to put themselves into "лучшие завтраки".

// GuideCollectionAdminFilter narrows the editor's collection listing. Unlike
// GuideCollectionFilter it can widen visibility, because the editor is meant to
// see drafts and archived rows — that is the entire point of the cabinet.
type GuideCollectionAdminFilter struct {
	// Statuses limits the listing to these publication states. Empty means all
	// three.
	Statuses []GuideCollectionStatus
	// City filters by the collection's OWN city column exactly: nil means no
	// filter at all. It deliberately does NOT fold in the city-agnostic rows the
	// guest listing adds, because an editor filtering by Astana is asking "what
	// is pinned to Astana", not "what would an Astana guest see".
	City *City
	// Query is a case-insensitive substring match over slug and title, so an
	// editor can find a collection without paging.
	Query   string
	Page    int
	PerPage int
}

// GuideCollectionAdminDetail is one collection as the EDITOR sees it: the
// collection itself, every venue in it (including deactivated ones, flagged),
// and the rubrics it belongs to as full rows rather than slugs.
//
// Deactivated venues are present here and absent from the guest detail on
// purpose: the editor has to see that slot 3 of their collection is currently
// dark, or the venue count they see will not match the one a guest sees and
// nobody will be able to explain why.
type GuideCollectionAdminDetail struct {
	GuideCollection
	// Venues are in editorial order, every attached venue, active or not.
	Venues []GuideCollectionVenue
	// Categories are the rubrics this collection belongs to, in the order they
	// were given inside the collection.
	Categories []GuideCategory
}

// GuideCollectionWrite is the full set of a collection's editable fields.
// Create and Update both take it: an editor form posts the whole collection,
// and a partial-update protocol would make "clear the subtitle" and "do not
// touch the subtitle" indistinguishable.
//
// Status and PublishedAt are NOT here. Publication is its own set of operations
// (Publish/Unpublish/Archive) with its own preconditions, so an editor cannot
// take a collection live as a side effect of fixing a typo.
type GuideCollectionWrite struct {
	Slug            string
	Title           string
	TitleI18n       I18n
	Subtitle        string
	SubtitleI18n    I18n
	Description     string
	DescriptionI18n I18n
	CoverImageURL   *string
	City            *City
	Position        int
}

// GuideCategoryWrite is a rubric's editable fields. Categories carry no
// publication axis — IsActive is the whole switch — and their position is a
// plain integer with no uniqueness, so there is no reorder operation for them:
// the editor sets each rubric's number directly.
type GuideCategoryWrite struct {
	Slug      string
	Title     string
	TitleI18n I18n
	Position  int
	IsActive  bool
}

// GuideVenueAttachment is one venue being put into a collection. Position is
// assigned by the repository (appended after the last one), never by the caller:
// two editors appending at the same moment would otherwise both compute the same
// number.
type GuideVenueAttachment struct {
	RestaurantID uuid.UUID
	Note         string
	NoteI18n     I18n
	// EventID / PromoID — чем проиллюстрирован блок. Не больше одного из двух
	// (схема это проверяет); оба nil — обычная карточка заведения.
	EventID *uuid.UUID
	PromoID *uuid.UUID
}

// GastroguideEditorRepository is the write model of the guide plus the reads the
// cabinet needs (which show drafts, and so cannot come from the guest model).
//
// Nothing here takes a `now`: unlike the guest reads, none of these answers
// depend on the clock — the editor sees a collection whatever its published_at
// says. The one place a clock is needed (defaulting publication to "now") is the
// usecase's, not the repository's.
type GastroguideEditorRepository interface {
	// --- categories ---

	// ListAllCategories returns every rubric, active or not, in editorial order.
	ListAllCategories(ctx context.Context) ([]GuideCategory, error)
	// CreateCategory inserts a rubric. A duplicate slug is ErrAlreadyExists
	// tagged CodeGuideSlugTaken.
	CreateCategory(ctx context.Context, in GuideCategoryWrite) (*GuideCategory, error)
	// UpdateCategory replaces a rubric's fields. Unknown id is ErrNotFound.
	UpdateCategory(ctx context.Context, id uuid.UUID, in GuideCategoryWrite) (*GuideCategory, error)

	// --- collections ---

	// ListCollectionsAdmin returns collections of ANY status, newest editorial
	// order first, paginated, plus the total.
	ListCollectionsAdmin(ctx context.Context, f GuideCollectionAdminFilter) ([]GuideCollection, int, error)
	// GetCollectionAdmin returns one collection of any status with every
	// attached venue and its rubrics. Unknown id is ErrNotFound.
	GetCollectionAdmin(ctx context.Context, id uuid.UUID) (*GuideCollectionAdminDetail, error)
	// CreateCollection inserts a collection as a DRAFT. A duplicate slug is
	// ErrAlreadyExists tagged CodeGuideSlugTaken.
	CreateCollection(ctx context.Context, in GuideCollectionWrite) (*GuideCollection, error)
	// UpdateCollection replaces a collection's editable fields, leaving its
	// status and published_at alone.
	UpdateCollection(ctx context.Context, id uuid.UUID, in GuideCollectionWrite) (*GuideCollection, error)
	// SetCollectionStatus moves a collection between draft/published/archived.
	// publishedAt is written as given: the DB refuses a published row without
	// one, so the usecase supplies it.
	SetCollectionStatus(ctx context.Context, id uuid.UUID, status GuideCollectionStatus, publishedAt *time.Time) (*GuideCollection, error)
	// CountActiveVenues returns how many venues of the collection a guest could
	// open right now — the same predicate the guest listing uses. It is what
	// publication is checked against.
	CountActiveVenues(ctx context.Context, id uuid.UUID) (int, error)

	// --- collection ↔ rubric ---

	// SetCollectionCategories replaces the whole rubric set of a collection, in
	// the given order. An empty slice detaches every rubric.
	SetCollectionCategories(ctx context.Context, collectionID uuid.UUID, categoryIDs []uuid.UUID) error

	// --- collection ↔ venue ---

	// AttachVenue appends a venue to the end of a collection. A venue already in
	// that collection is ErrAlreadyExists tagged CodeGuideVenueAlreadyAttached.
	// An unknown restaurant is ErrNotFound.
	AttachVenue(ctx context.Context, collectionID uuid.UUID, in GuideVenueAttachment) error
	// SetVenueHighlight переставляет (или снимает, если оба nil) событие/акцию
	// у уже добавленного заведения — отдельным вызовом, чтобы не пересобирать
	// привязку целиком ради одного поля.
	SetVenueHighlight(ctx context.Context, collectionID, restaurantID uuid.UUID, eventID, promoID *uuid.UUID) error
	// DetachVenue removes a venue from a collection and closes the gap it left,
	// so positions stay 1..N with no hole. Not in the collection is ErrNotFound.
	DetachVenue(ctx context.Context, collectionID, restaurantID uuid.UUID) error
	// UpdateVenueNote rewrites the editor's line under one venue's card.
	UpdateVenueNote(ctx context.Context, collectionID, restaurantID uuid.UUID, note string, noteI18n I18n) error
	// ReorderVenues writes a whole new ordering in ONE transaction:
	// restaurantIDs is the intended FINAL sequence and must name exactly the
	// collection's current members, each once. Anything else is ErrValidation
	// tagged CodeGuideOrderMismatch and nothing is written.
	ReorderVenues(ctx context.Context, collectionID uuid.UUID, restaurantIDs []uuid.UUID) error
	// ListCollectionVenueIDs returns the collection's current members in
	// editorial order. Used by the reorder check and by tests.
	ListCollectionVenueIDs(ctx context.Context, collectionID uuid.UUID) ([]uuid.UUID, error)
}

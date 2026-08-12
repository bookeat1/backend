package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// GuideCollectionStatus is a gastroguide collection's publication state, stored
// as VARCHAR (validated here, never a Postgres ENUM).
//
// It is the editor's staging lever: a collection is written and filled with
// venues while it is a draft, and nothing about it — not its slug, not its
// cover — is reachable by a guest until it is published.
type GuideCollectionStatus string

const (
	// GuideCollectionDraft is being prepared. Invisible to guests.
	GuideCollectionDraft GuideCollectionStatus = "draft"
	// GuideCollectionPublished is live from PublishedAt on (a future PublishedAt
	// is a scheduled publication, not a live collection).
	GuideCollectionPublished GuideCollectionStatus = "published"
	// GuideCollectionArchived was live once and has been withdrawn. Invisible to
	// guests, but its venue links are kept so it can be brought back.
	GuideCollectionArchived GuideCollectionStatus = "archived"
)

// Valid reports whether s is a known collection status.
func (s GuideCollectionStatus) Valid() bool {
	switch s {
	case GuideCollectionDraft, GuideCollectionPublished, GuideCollectionArchived:
		return true
	}
	return false
}

// GuideCategory is a rubric of the gastroguide ("Завтраки", "С детьми"). Slug is
// the stable client-facing name: the app links to a rubric by slug so a title
// rewrite does not break a link. Position is the editorial order.
type GuideCategory struct {
	ID        uuid.UUID
	Slug      string
	Title     string
	TitleI18n I18n
	Position  int
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GuideCollection is one editorial collection of venues. Title/Subtitle/
// Description are localized the same way the catalog is (base ru column +
// optional *_i18n jsonb — see I18n.Resolve).
type GuideCollection struct {
	ID              uuid.UUID
	Slug            string
	Title           string
	TitleI18n       I18n
	Subtitle        string
	SubtitleI18n    I18n
	Description     string
	DescriptionI18n I18n
	// CoverImageURL is the full public image URL, or nil when the collection has
	// no cover. Never a placeholder: nil means "there is no image".
	CoverImageURL *string
	// City nil means the collection is shown in every city.
	City        *City
	Status      GuideCollectionStatus
	PublishedAt *time.Time
	Position    int
	// VenueCount is how many venues a guest can actually open right now, not
	// how many the editor put in: a collection that says 12 and shows 9 is a
	// broken promise.
	VenueCount int
	// CategorySlugs are the rubrics this collection belongs to, in the guide's
	// own order, so the app can render its rubric chips without a second call.
	CategorySlugs []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// GuideCollectionVenue is one venue inside a collection: the editorial part
// (position, note) plus the catalog fields a card needs, so the guest read is
// one query and not a fan-out of catalog lookups.
type GuideCollectionVenue struct {
	RestaurantID    uuid.UUID
	Position        int
	Note            string
	NoteI18n        I18n
	Name            string
	NameI18n        I18n
	Address         string
	AddressI18n     I18n
	CuisineType     string
	CuisineTypeI18n I18n
	City            City
	PriceCategory   PriceCategory
	// PrimaryImageURL is the venue's primary catalog image, nil when it has none.
	PrimaryImageURL *string
	// Instagram — ссылка на инстаграм заведения из его соцсетей, пустая строка,
	// если её нет. В макете подпись блока выглядит как «адрес · @инстаграм», и
	// берётся она у ЗАВЕДЕНИЯ, а не у события.
	Instagram string
	// Highlight — событие или акция, которыми проиллюстрирован блок. nil, когда
	// блок остаётся простой карточкой заведения (в том числе если событие
	// удалили: ссылка обнуляется, а блок остаётся).
	Highlight *GuideHighlight
	// IsActive is the venue's catalog state. On a GUEST read it is always true —
	// the SQL filters deactivated venues out — and it exists for the EDITOR
	// read, which shows them: an editor has to see that a slot in their
	// collection is currently dark, otherwise the venue count in the cabinet and
	// the one a guest sees differ with no visible reason.
	IsActive bool
}

// GuideHighlightKind различает, чем проиллюстрирован блок подборки.
type GuideHighlightKind string

const (
	GuideHighlightEvent GuideHighlightKind = "event"
	GuideHighlightPromo GuideHighlightKind = "promo"
)

// GuideHighlight — событие или акция внутри блока подборки: заголовок, текст и
// галерея, которые в макете стоят выше адреса заведения.
type GuideHighlight struct {
	Kind            GuideHighlightKind
	ID              uuid.UUID
	Title           string
	TitleI18n       I18n
	Description     string
	DescriptionI18n I18n
	// StartsAt имеет смысл только у события; у акции это начало действия.
	StartsAt time.Time
	// Images — галерея (event_images / promo_images) в порядке редактора. Может
	// быть пустой: тогда блок показывает обложку заведения, как и раньше.
	Images []string
	// CoverImageURL — обложка события или акции, nil при её отсутствии.
	CoverImageURL *string
}

// GuideCollectionDetail is a collection together with its ordered, guest-visible
// venues.
type GuideCollectionDetail struct {
	GuideCollection
	Venues []GuideCollectionVenue
}

// GuideCollectionFilter narrows the public collection listing. Both filters are
// optional; neither can WIDEN visibility — the published-and-live rule lives in
// SQL and is not reachable from a query parameter.
type GuideCollectionFilter struct {
	// City selects collections pinned to that city plus the city-agnostic ones
	// (city IS NULL). Nil means no city filter at all.
	City *City
	// CategorySlug narrows to one rubric.
	CategorySlug *string
	Page         int
	PerPage      int
}

// GastroguideRepository is the guest-facing read model of the gastroguide. It
// is read-only on purpose: this increment ships the guest side only, and the
// editor tooling that writes these rows is a separate task.
//
// Every method takes `now` because visibility is time-dependent (a collection
// published with a future PublishedAt is not live yet) and the clock belongs to
// the usecase, not to the SQL.
type GastroguideRepository interface {
	// ListCategories returns the active rubrics that have at least one live
	// collection (a rubric that opens into an empty screen is not shown),
	// in editorial order.
	ListCategories(ctx context.Context, city *City, now time.Time) ([]GuideCategory, error)
	// ListPublishedCollections returns live collections in editorial order, with
	// their guest-visible venue count, paginated, plus the total.
	ListPublishedCollections(ctx context.Context, f GuideCollectionFilter, now time.Time) ([]GuideCollection, int, error)
	// GetPublishedCollectionBySlug returns a live collection with its ordered,
	// guest-visible venues. Returns ErrNotFound when the slug is unknown OR the
	// collection is not live — a draft must not be distinguishable from a
	// typo, or the slug of an unannounced collection leaks.
	GetPublishedCollectionBySlug(ctx context.Context, slug string, now time.Time) (*GuideCollectionDetail, error)
}

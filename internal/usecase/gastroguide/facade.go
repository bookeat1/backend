// Package gastroguide is the application logic of the editorial guide — the
// home screen's "подборки" block ("где поесть с детьми", "лучшие завтраки").
//
// Guest reads only. Everything an editor would need to WRITE a collection
// (create, reorder, publish, attach venues) is deliberately not here: that is a
// cabinet with its own RBAC and its own reordering semantics, and half of it
// would be worse than none.
//
// The facade holds no visibility logic of its own on purpose. Which collection
// is live and which venue may be shown lives in SQL
// (infrastructure/postgres/gastroguide), so no filter passed by a caller can
// widen it; the facade only validates the filters and supplies the clock.
package gastroguide

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade exposes the guest-facing reads of the gastroguide.
type Facade interface {
	// ListCategories returns the guide's rubrics that currently hold at least
	// one live collection.
	ListCategories(ctx context.Context, city *domain.City) ([]domain.GuideCategory, error)
	// ListCollections returns live collections, paginated, plus the total.
	ListCollections(ctx context.Context, in ListInput) ([]domain.GuideCollection, int, error)
	// GetCollection returns one live collection with its ordered venues.
	// ErrNotFound covers both "no such slug" and "not published".
	//
	// Deliberately KIND-AGNOSTIC: a slug is unique across the whole table, so
	// this resolves an article and a collection alike. See the transport for
	// why (deep links already in the wild).
	GetCollection(ctx context.Context, slug string) (*domain.GuideCollectionDetail, error)
}

// ListInput carries the optional filters of the collection listing.
type ListInput struct {
	City         *domain.City
	CategorySlug *string
	// Kind selects collections or articles (migration 0096). Nil means both,
	// which is what the endpoint that predates the split would ask for; the
	// transport always sets it, so /gastroguide/collections and /articles
	// cannot leak into each other.
	Kind    *domain.GuideCollectionKind
	Page    int
	PerPage int
}

type facade struct {
	repo   domain.GastroguideRepository
	events domain.EventRepository
	promos domain.PromoRepository
	clock  func() time.Time
}

// NewFacade constructs the gastroguide Facade. The event and promo
// repositories are here for ONE thing: a block illustrated by an event or a
// promo shows its gallery, and those live in their own tables. Both may be nil
// — then a block simply renders without a photo row, which is what every
// collection did before galleries existed.
func NewFacade(repo domain.GastroguideRepository, events domain.EventRepository, promos domain.PromoRepository) Facade {
	return &facade{repo: repo, events: events, promos: promos, clock: time.Now}
}

func (f *facade) ListCategories(ctx context.Context, city *domain.City) ([]domain.GuideCategory, error) {
	return f.repo.ListCategories(ctx, city, f.clock())
}

func (f *facade) ListCollections(ctx context.Context, in ListInput) ([]domain.GuideCollection, int, error) {
	if in.Kind != nil && !in.Kind.Valid() {
		return nil, 0, domain.WithCode(domain.CodeGuideUnknownKind,
			fmt.Errorf("%w: unknown collection kind %q", domain.ErrValidation, *in.Kind))
	}
	return f.repo.ListPublishedCollections(ctx, domain.GuideCollectionFilter{
		City:         in.City,
		CategorySlug: in.CategorySlug,
		Kind:         in.Kind,
		Page:         in.Page,
		PerPage:      in.PerPage,
	}, f.clock())
}

func (f *facade) GetCollection(ctx context.Context, slug string) (*domain.GuideCollectionDetail, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("%w: slug is required", domain.ErrValidation)
	}
	detail, err := f.repo.GetPublishedCollectionBySlug(ctx, slug, f.clock())
	if err != nil {
		return nil, err
	}
	f.attachHighlightGalleries(ctx, detail)
	return detail, nil
}

// attachHighlightGalleries дочитывает фотографии событий и акций, которыми
// проиллюстрированы блоки — ДВУМЯ запросами на всю подборку, а не по одному на
// блок.
//
// Ошибки намеренно проглатываются: галерея — украшение блока, а подборка
// читается целиком. Уронить статью из-за не загрузившихся фотографий значит
// показать гостю ошибку там, где можно показать текст.
func (f *facade) attachHighlightGalleries(ctx context.Context, detail *domain.GuideCollectionDetail) {
	if detail == nil {
		return
	}
	var eventIDs, promoIDs []uuid.UUID
	for i := range detail.Venues {
		h := detail.Venues[i].Highlight
		if h == nil {
			continue
		}
		switch h.Kind {
		case domain.GuideHighlightEvent:
			eventIDs = append(eventIDs, h.ID)
		case domain.GuideHighlightPromo:
			promoIDs = append(promoIDs, h.ID)
		}
	}

	var eventImages, promoImages map[uuid.UUID][]string
	if f.events != nil && len(eventIDs) > 0 {
		if m, err := f.events.ImagesByEvent(ctx, eventIDs); err == nil {
			eventImages = m
		}
	}
	if f.promos != nil && len(promoIDs) > 0 {
		if m, err := f.promos.ImagesByPromo(ctx, promoIDs); err == nil {
			promoImages = m
		}
	}

	for i := range detail.Venues {
		h := detail.Venues[i].Highlight
		if h == nil {
			continue
		}
		switch h.Kind {
		case domain.GuideHighlightEvent:
			h.Images = eventImages[h.ID]
		case domain.GuideHighlightPromo:
			h.Images = promoImages[h.ID]
		}
	}
}

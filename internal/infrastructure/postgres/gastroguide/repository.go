// Package gastroguide is the Postgres implementation of
// domain.GastroguideRepository — the guest-facing read model of the editorial
// guide (migration 0061).
//
// Two rules are enforced HERE, in SQL, and are not reachable from any query
// parameter (the same posture as EventRepository.ListPublicUpcoming and
// FeedRepository.ListCandidates):
//
//  1. A collection is visible only while status = 'published' AND
//     published_at <= now. A draft, an archived collection and a collection
//     scheduled for tomorrow are all equally absent.
//  2. A venue inside a collection is visible only while restaurants.is_active.
//     A deactivated venue cannot be opened or booked, so listing it would send
//     the guest into a dead end; the membership row is kept untouched, because
//     deactivation is routinely temporary and an editor must not lose their
//     curation to it.
//
// hidden_from_home is deliberately NOT applied: per the catalog convention that
// flag hides a venue from the merchandising rail on the main screen, not from
// the catalog, and a collection an editor curated by hand is catalog content
// reachable by its own link.
package gastroguide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.GastroguideRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the gastroguide repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.GastroguideRepository = (*Repository)(nil)

// liveCollection is the visibility predicate for a collection, written once and
// reused by every read so no query can drift away from it. `c` is the
// collections alias, $1 is `now`.
const liveCollection = `c.status = 'published' AND c.published_at <= $1::timestamptz`

// visibleVenues counts the venues of collection c a guest can actually open.
// Used both as the card's venue_count and as the "do not show an empty
// collection" filter.
const visibleVenues = `(SELECT count(*) FROM gastroguide_collection_venues cv
		JOIN restaurants r ON r.id = cv.restaurant_id
		WHERE cv.collection_id = c.id AND r.is_active)`

const collectionCols = `c.id, c.slug, c.title, c.title_i18n, c.subtitle, c.subtitle_i18n,
	c.description, c.description_i18n, c.cover_image_url, c.city, c.status, c.published_at,
	c.position, c.created_at, c.updated_at,
	` + visibleVenues + `::int AS venue_count,
	COALESCE((SELECT array_agg(cat.slug ORDER BY cc.position, cat.id)
		FROM gastroguide_collection_categories cc
		JOIN gastroguide_categories cat ON cat.id = cc.category_id
		WHERE cc.collection_id = c.id AND cat.is_active), '{}') AS category_slugs`

// ListCategories returns the active rubrics that hold at least one live,
// non-empty collection. A rubric whose collections are all drafts would open
// into an empty screen, so it is not offered.
func (r *Repository) ListCategories(ctx context.Context, city *domain.City, now time.Time) ([]domain.GuideCategory, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT cat.id, cat.slug, cat.title, cat.title_i18n, cat.position, cat.is_active,
			cat.created_at, cat.updated_at
		 FROM gastroguide_categories cat
		 WHERE cat.is_active
		   AND EXISTS (
			SELECT 1
			FROM gastroguide_collection_categories cc
			JOIN gastroguide_collections c ON c.id = cc.collection_id
			WHERE cc.category_id = cat.id
			  AND `+liveCollection+`
			  AND ($2::varchar IS NULL OR c.city IS NULL OR c.city = $2)
			  AND `+visibleVenues+` > 0
		   )
		 ORDER BY cat.position, cat.id`,
		now, cityArg(city))
	if err != nil {
		return nil, fmt.Errorf("list guide categories: %w", err)
	}
	defer rows.Close()

	var out []domain.GuideCategory
	for rows.Next() {
		var c domain.GuideCategory
		var titleI18n []byte
		if err := rows.Scan(&c.ID, &c.Slug, &c.Title, &titleI18n, &c.Position, &c.IsActive,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan guide category: %w", err)
		}
		c.TitleI18n = i18nFromDB(titleI18n)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guide categories: %w", err)
	}
	return out, nil
}

// ListPublishedCollections returns live collections in editorial order
// (position, then id as the stable tie-break), paginated, plus the total.
//
// Collections with no guest-visible venue are excluded: a guide entry that
// opens into an empty screen is worse than no entry, and after the is_active
// filter that state is reachable without an editor doing anything wrong.
func (r *Repository) ListPublishedCollections(ctx context.Context, f domain.GuideCollectionFilter, now time.Time) ([]domain.GuideCollection, int, error) {
	page, perPage := normalizePage(f.Page, f.PerPage)
	args := []any{now, cityArg(f.City)}
	from := ` FROM gastroguide_collections c
		WHERE ` + liveCollection + `
		  AND ($2::varchar IS NULL OR c.city IS NULL OR c.city = $2)
		  AND ` + visibleVenues + ` > 0`
	if f.CategorySlug != nil {
		args = append(args, *f.CategorySlug)
		from += `
		  AND EXISTS (SELECT 1 FROM gastroguide_collection_categories cc
			JOIN gastroguide_categories cat ON cat.id = cc.category_id
			WHERE cc.collection_id = c.id AND cat.is_active AND cat.slug = $` + strconv.Itoa(len(args)) + `)`
	}

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count guide collections: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx,
		`SELECT `+collectionCols+from+`
		 ORDER BY c.position, c.id
		 LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list guide collections: %w", err)
	}
	defer rows.Close()

	var items []domain.GuideCollection
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan guide collection: %w", err)
		}
		items = append(items, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate guide collections: %w", err)
	}
	return items, total, nil
}

// GetPublishedCollectionBySlug returns a live collection with its ordered,
// guest-visible venues. An unknown slug and a collection that is not live are
// the SAME answer (ErrNotFound): telling them apart would confirm the slug of a
// collection that has not been announced yet.
//
// Unlike the listing, a live collection whose venues are all deactivated is
// still returned — with an empty venue list. The guest followed a link to a
// page that exists; answering 404 would be a different lie.
func (r *Repository) GetPublishedCollectionBySlug(ctx context.Context, slug string, now time.Time) (*domain.GuideCollectionDetail, error) {
	q := sqltx.From(ctx, r.pool)
	row := q.QueryRow(ctx,
		`SELECT `+collectionCols+`
		 FROM gastroguide_collections c
		 WHERE `+liveCollection+` AND c.slug = $2`, now, slug)
	c, err := scanCollection(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get guide collection: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get guide collection: %w", err)
	}

	venues, err := r.listVenues(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	return &domain.GuideCollectionDetail{GuideCollection: *c, Venues: venues}, nil
}

// listVenues reads a collection's venues in the editor's explicit order. The
// ORDER BY is (position, restaurant_id): position is the editorial intent and
// restaurant_id is the tie-break that keeps the sequence identical on every
// request even if two rows ever share a number.
//
// The primary image is read with a LATERAL LIMIT 1 rather than a join, so a
// venue with several primary-flagged images cannot duplicate its own card.
func (r *Repository) listVenues(ctx context.Context, collectionID uuid.UUID) ([]domain.GuideCollectionVenue, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT cv.restaurant_id, cv.position, cv.note, cv.note_i18n,
			rest.name, rest.name_i18n, rest.address, rest.address_i18n,
			rest.cuisine_type, rest.cuisine_type_i18n, rest.city, rest.price_category,
			img.image_url, rest.is_active,
			COALESCE(soc.url, '') AS instagram,
			ev.id, ev.title, ev.title_i18n, ev.description, ev.description_i18n,
			ev.starts_at, ev.cover_image_url,
			pr.id, pr.title, pr.title_i18n, pr.description, pr.description_i18n,
			pr.starts_at, pr.cover_image_url
		 FROM gastroguide_collection_venues cv
		 JOIN restaurants rest ON rest.id = cv.restaurant_id
		 LEFT JOIN LATERAL (
			SELECT ri.image_url FROM restaurant_images ri
			WHERE ri.restaurant_id = rest.id
			ORDER BY ri.is_primary DESC, ri.created_at, ri.id
			LIMIT 1
		 ) img ON true
		 -- Инстаграм берём у ЗАВЕДЕНИЯ (в макете подпись «адрес · @инстаграм»),
		 -- первую подходящую ссылку: у заведения их может быть несколько.
		 LEFT JOIN LATERAL (
			SELECT rs.url FROM restaurant_social_links rs
			WHERE rs.restaurant_id = rest.id AND lower(rs.platform) = 'instagram'
			ORDER BY rs.created_at, rs.id
			LIMIT 1
		 ) soc ON true
		 -- Событие и акция подтягиваются целиком: заголовок и текст блока в
		 -- макете принадлежат им, а не заведению.
		 LEFT JOIN events ev ON ev.id = cv.event_id
		 LEFT JOIN promos pr ON pr.id = cv.promo_id
		 WHERE cv.collection_id = $1 AND rest.is_active
		 ORDER BY cv.position, cv.restaurant_id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("list guide collection venues: %w", err)
	}
	defer rows.Close()

	var out []domain.GuideCollectionVenue
	for rows.Next() {
		var v domain.GuideCollectionVenue
		var noteI18n, nameI18n, addrI18n, cuisineI18n []byte
		var ev, pr highlightRow
		if err := rows.Scan(&v.RestaurantID, &v.Position, &v.Note, &noteI18n,
			&v.Name, &nameI18n, &v.Address, &addrI18n,
			&v.CuisineType, &cuisineI18n, &v.City, &v.PriceCategory, &v.PrimaryImageURL,
			&v.IsActive, &v.Instagram,
			&ev.id, &ev.title, &ev.titleI18n, &ev.description, &ev.descriptionI18n,
			&ev.startsAt, &ev.cover,
			&pr.id, &pr.title, &pr.titleI18n, &pr.description, &pr.descriptionI18n,
			&pr.startsAt, &pr.cover); err != nil {
			return nil, fmt.Errorf("scan guide collection venue: %w", err)
		}
		v.NoteI18n = i18nFromDB(noteI18n)
		v.NameI18n = i18nFromDB(nameI18n)
		v.AddressI18n = i18nFromDB(addrI18n)
		v.CuisineTypeI18n = i18nFromDB(cuisineI18n)
		// Проверка в схеме гарантирует, что заполнено не больше одного из двух.
		if h := ev.toDomain(domain.GuideHighlightEvent); h != nil {
			v.Highlight = h
		} else if h := pr.toDomain(domain.GuideHighlightPromo); h != nil {
			v.Highlight = h
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate guide collection venues: %w", err)
	}
	return out, nil
}

func scanCollection(row pgx.Row) (*domain.GuideCollection, error) {
	var c domain.GuideCollection
	var titleI18n, subtitleI18n, descI18n []byte
	var city *string
	var slugs []string
	if err := row.Scan(&c.ID, &c.Slug, &c.Title, &titleI18n, &c.Subtitle, &subtitleI18n,
		&c.Description, &descI18n, &c.CoverImageURL, &city, &c.Status, &c.PublishedAt,
		&c.Position, &c.CreatedAt, &c.UpdatedAt, &c.VenueCount, &slugs); err != nil {
		return nil, err
	}
	c.TitleI18n = i18nFromDB(titleI18n)
	c.SubtitleI18n = i18nFromDB(subtitleI18n)
	c.DescriptionI18n = i18nFromDB(descI18n)
	if city != nil {
		v := domain.City(*city)
		c.City = &v
	}
	c.CategorySlugs = slugs
	return &c, nil
}

// cityArg keeps the "no city filter" case a real SQL NULL instead of an empty
// string, so the predicate reads the same way in every query.
func cityArg(city *domain.City) any {
	if city == nil {
		return nil
	}
	return string(*city)
}

func normalizePage(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	return page, perPage
}

func i18nFromDB(b []byte) domain.I18n {
	if len(b) == 0 {
		return nil
	}
	var m domain.I18n
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// highlightRow — сырые колонки события или акции из LEFT JOIN: все NULL, когда
// блок ни с чем не связан.
type highlightRow struct {
	id              *uuid.UUID
	title           *string
	titleI18n       []byte
	description     *string
	descriptionI18n []byte
	startsAt        *time.Time
	cover           *string
}

// toDomain собирает подсветку блока; nil, когда джойн ничего не дал. Галерея
// здесь НЕ читается: её дочитывает usecase одним батчем на всю подборку, чтобы
// не делать по запросу на каждый блок.
func (h highlightRow) toDomain(kind domain.GuideHighlightKind) *domain.GuideHighlight {
	if h.id == nil {
		return nil
	}
	out := &domain.GuideHighlight{Kind: kind, ID: *h.id, CoverImageURL: h.cover}
	if h.title != nil {
		out.Title = *h.title
	}
	if h.description != nil {
		out.Description = *h.description
	}
	if h.startsAt != nil {
		out.StartsAt = *h.startsAt
	}
	out.TitleI18n = i18nFromDB(h.titleI18n)
	out.DescriptionI18n = i18nFromDB(h.descriptionI18n)
	return out
}

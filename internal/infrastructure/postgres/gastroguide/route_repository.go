// The guest-facing read model of «Гастропрогулки» (migration 0078). It lives
// beside the collections in the same package because both are the gastroguide
// and both share the same helpers (i18nFromDB, cityArg, normalizePage) — and,
// more importantly, the same publication predicate, which must stay one idea in
// two queries rather than two ideas.
//
// Two rules are enforced HERE, in SQL, and are not reachable from a query
// parameter:
//
//  1. A route is visible only while status = 'published' AND published_at <=
//     now. A draft, an archived route and one scheduled for tomorrow are all
//     equally absent, and an unknown slug is the same 404 as a draft.
//
//  2. A stop's VENUE CARD is resolved only while restaurants.is_active. The
//     STOP itself is always returned: an itinerary whose second of five stops
//     silently disappeared is worse than one whose second stop reads as its own
//     text and offers no "open the venue" button. This is where a route
//     deliberately differs from a collection, which drops a dark venue from its
//     list entirely — a list stays correct when it shrinks, a sequence does not.
package gastroguide

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// RouteRepository implements domain.GastroRouteRepository.
type RouteRepository struct{ pool sqltx.Querier }

// NewRoutes builds the guest route repository.
func NewRoutes(pool sqltx.Querier) *RouteRepository { return &RouteRepository{pool: pool} }

var _ domain.GastroRouteRepository = (*RouteRepository)(nil)

// liveRoute is the visibility predicate for a route, written once and reused by
// every read. `rt` is the routes alias, $1 is `now`. It is character-for-
// character the collections' liveCollection rule.
const liveRoute = `rt.status = 'published' AND rt.published_at <= $1::timestamptz`

// pointCount counts ALL the stops of route rt — not just the openable ones.
// A stop whose venue is dark is still a stop on the walk, and the route's
// duration_label («1 день · 4 точки») was written about the full itinerary.
const pointCount = `(SELECT count(*) FROM gastro_route_points p WHERE p.route_id = rt.id)`

const routeCols = `rt.id, rt.slug, rt.title, rt.title_i18n, rt.description, rt.description_i18n,
	rt.cover_image_url, rt.duration_label, rt.duration_label_i18n, rt.city, rt.status,
	rt.published_at, rt.position, rt.created_at, rt.updated_at,
	` + pointCount + `::int AS point_count`

// pointCols is the stop plus the venue columns. The venue half is filled by a
// LEFT JOIN, so every one of those columns can be NULL and is scanned into a
// pointer; whether the join is allowed to match a deactivated venue is the ONE
// difference between the guest read and the editor read.
const pointCols = `p.id, p.position, p.kind, p.restaurant_id, p.title, p.title_i18n,
	p.description, p.description_i18n, p.photo_url, p.address, p.address_i18n,
	p.latitude, p.longitude,
	rest.id, rest.name, rest.name_i18n, rest.address, rest.address_i18n,
	rest.cuisine_type, rest.cuisine_type_i18n, rest.city, rest.price_category,
	img.image_url, rest.is_active`

// pointFrom joins the stops to their venues. `venueJoin` is spliced in as the
// extra join condition: the guest passes "AND rest.is_active", the editor
// passes nothing. `extra` narrows the rows further (the editor reads one stop
// by id); both are compile-time constants of this package, never user input.
func pointFrom(venueJoin, extra string) string {
	return ` FROM gastro_route_points p
		 LEFT JOIN restaurants rest ON rest.id = p.restaurant_id ` + venueJoin + `
		 LEFT JOIN LATERAL (
			SELECT ri.image_url FROM restaurant_images ri
			WHERE ri.restaurant_id = rest.id
			ORDER BY ri.is_primary DESC, ri.created_at, ri.id
			LIMIT 1
		 ) img ON true
		 WHERE p.route_id = $1 ` + extra + `
		 ORDER BY p.position, p.id`
}

// ListPublishedRoutes returns live routes in editorial order (position, then id
// as the stable tie-break), paginated, plus the total.
func (r *RouteRepository) ListPublishedRoutes(ctx context.Context, f domain.GastroRouteFilter, now time.Time) ([]domain.GastroRoute, int, error) {
	page, perPage := normalizePage(f.Page, f.PerPage)
	args := []any{now, cityArg(f.City)}
	from := ` FROM gastro_routes rt
		WHERE ` + liveRoute + `
		  AND ($2::varchar IS NULL OR rt.city IS NULL OR rt.city = $2)`

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count gastro routes: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx,
		`SELECT `+routeCols+from+`
		 ORDER BY rt.position, rt.id
		 LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list gastro routes: %w", err)
	}
	defer rows.Close()

	var items []domain.GastroRoute
	for rows.Next() {
		rt, err := scanRoute(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan gastro route: %w", err)
		}
		items = append(items, *rt)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate gastro routes: %w", err)
	}
	return items, total, nil
}

// GetPublishedRouteBySlug returns a live route with its ordered stops. An
// unknown slug and a route that is not live are the SAME answer (ErrNotFound):
// telling them apart would confirm the slug of an unannounced route.
func (r *RouteRepository) GetPublishedRouteBySlug(ctx context.Context, slug string, now time.Time) (*domain.GastroRouteDetail, error) {
	rt, err := scanRoute(sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+routeCols+`
		 FROM gastro_routes rt
		 WHERE `+liveRoute+` AND rt.slug = $2`, now, slug))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get gastro route: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get gastro route: %w", err)
	}

	points, err := r.listPoints(ctx, rt.ID, true)
	if err != nil {
		return nil, err
	}
	return &domain.GastroRouteDetail{GastroRoute: *rt, Points: points}, nil
}

// listPoints reads a route's stops in the editor's explicit order. guestOnly
// decides whether a deactivated venue may still fill the venue card — it is the
// only difference between what a guest and an editor get.
func (r *RouteRepository) listPoints(ctx context.Context, routeID uuid.UUID, guestOnly bool) ([]domain.GuideRoutePoint, error) {
	venueJoin := ""
	if guestOnly {
		venueJoin = "AND rest.is_active"
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+pointCols+pointFrom(venueJoin, ""), routeID)
	if err != nil {
		return nil, fmt.Errorf("list gastro route points: %w", err)
	}
	defer rows.Close()

	out := make([]domain.GuideRoutePoint, 0)
	for rows.Next() {
		p, err := scanRoutePoint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan gastro route point: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate gastro route points: %w", err)
	}
	return out, nil
}

func scanRoute(row pgx.Row) (*domain.GastroRoute, error) {
	var rt domain.GastroRoute
	var titleI18n, descI18n, durationI18n []byte
	var city *string
	if err := row.Scan(&rt.ID, &rt.Slug, &rt.Title, &titleI18n, &rt.Description, &descI18n,
		&rt.CoverImageURL, &rt.DurationLabel, &durationI18n, &city, &rt.Status,
		&rt.PublishedAt, &rt.Position, &rt.CreatedAt, &rt.UpdatedAt, &rt.PointCount); err != nil {
		return nil, err
	}
	rt.TitleI18n = i18nFromDB(titleI18n)
	rt.DescriptionI18n = i18nFromDB(descI18n)
	rt.DurationLabelI18n = i18nFromDB(durationI18n)
	if city != nil {
		v := domain.City(*city)
		rt.City = &v
	}
	return &rt, nil
}

// scanRoutePoint reads one stop and, when the join matched, its venue card. The
// venue is built only if rest.id came back non-NULL — a place stop, a stop
// whose venue row was deleted and (on the guest read) a stop whose venue is
// deactivated all land in the same branch and produce a stop with no card.
func scanRoutePoint(row pgx.Row) (*domain.GuideRoutePoint, error) {
	var p domain.GuideRoutePoint
	var titleI18n, descI18n, addrI18n []byte
	var v struct {
		id            *uuid.UUID
		name          *string
		nameI18n      []byte
		address       *string
		addressI18n   []byte
		cuisine       *string
		cuisineI18n   []byte
		city          *string
		priceCategory *string
		image         *string
		isActive      *bool
	}
	if err := row.Scan(&p.ID, &p.Position, &p.Kind, &p.RestaurantID, &p.Title, &titleI18n,
		&p.Description, &descI18n, &p.PhotoURL, &p.Address, &addrI18n,
		&p.Latitude, &p.Longitude,
		&v.id, &v.name, &v.nameI18n, &v.address, &v.addressI18n,
		&v.cuisine, &v.cuisineI18n, &v.city, &v.priceCategory, &v.image, &v.isActive); err != nil {
		return nil, err
	}
	p.TitleI18n = i18nFromDB(titleI18n)
	p.DescriptionI18n = i18nFromDB(descI18n)
	p.AddressI18n = i18nFromDB(addrI18n)
	if v.id == nil {
		return &p, nil
	}
	venue := domain.GuideRoutePointVenue{
		ID:              *v.id,
		NameI18n:        i18nFromDB(v.nameI18n),
		AddressI18n:     i18nFromDB(v.addressI18n),
		CuisineTypeI18n: i18nFromDB(v.cuisineI18n),
		PrimaryImageURL: v.image,
	}
	if v.name != nil {
		venue.Name = *v.name
	}
	if v.address != nil {
		venue.Address = *v.address
	}
	if v.cuisine != nil {
		venue.CuisineType = *v.cuisine
	}
	if v.city != nil {
		venue.City = domain.City(*v.city)
	}
	if v.priceCategory != nil {
		venue.PriceCategory = domain.PriceCategory(*v.priceCategory)
	}
	if v.isActive != nil {
		venue.IsActive = *v.isActive
	}
	p.Venue = &venue
	return &p, nil
}

// The editor (write) side of «Гастропрогулки». It sits next to the guest read
// model for the same reason the collection editor does: they share the column
// lists and the scanners, and two packages would mean two copies of routeCols
// drifting apart.
//
// What is NOT shared is the visibility rule — the editor's job is to see drafts
// and dark venues. What IS shared is the shape of a stop, so the cabinet and
// the app cannot disagree about what a route looks like.
package gastroguide

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// RouteEditorRepository implements domain.GastroRouteEditorRepository.
type RouteEditorRepository struct {
	pool sqltx.Querier
	tx   domain.TxManager
}

// NewRouteEditor builds the route editor repository. It needs a TxManager
// because appending, deleting and reordering stops are each only correct as a
// unit of several statements under a lock on the route.
func NewRouteEditor(pool sqltx.Querier, tx domain.TxManager) *RouteEditorRepository {
	return &RouteEditorRepository{pool: pool, tx: tx}
}

var _ domain.GastroRouteEditorRepository = (*RouteEditorRepository)(nil)

// --- routes ---

// ListRoutesAdmin returns routes of any status in editorial order.
func (r *RouteEditorRepository) ListRoutesAdmin(ctx context.Context, f domain.GastroRouteAdminFilter) ([]domain.GastroRoute, int, error) {
	page, perPage := normalizePage(f.Page, f.PerPage)

	args := []any{}
	where := []string{"TRUE"}
	if len(f.Statuses) > 0 {
		statuses := make([]string, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			statuses = append(statuses, string(s))
		}
		args = append(args, statuses)
		where = append(where, `rt.status = ANY($`+strconv.Itoa(len(args))+`::varchar[])`)
	}
	if f.City != nil {
		args = append(args, string(*f.City))
		where = append(where, `rt.city = $`+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		n := strconv.Itoa(len(args))
		where = append(where, `(rt.slug ILIKE $`+n+` OR rt.title ILIKE $`+n+`)`)
	}
	from := ` FROM gastro_routes rt WHERE ` + strings.Join(where, " AND ")

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin gastro routes: %w", err)
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
		return nil, 0, fmt.Errorf("list admin gastro routes: %w", err)
	}
	defer rows.Close()

	var items []domain.GastroRoute
	for rows.Next() {
		rt, err := scanRoute(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan admin gastro route: %w", err)
		}
		items = append(items, *rt)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate admin gastro routes: %w", err)
	}
	return items, total, nil
}

// GetRouteAdmin returns one route of any status with EVERY stop, including the
// ones whose venue is deactivated (flagged IsActive = false) — the editor has
// to see which stop of their itinerary a guest currently cannot open.
func (r *RouteEditorRepository) GetRouteAdmin(ctx context.Context, id uuid.UUID) (*domain.GastroRouteAdminDetail, error) {
	rt, err := r.getRoute(ctx, id)
	if err != nil {
		return nil, err
	}
	points, err := (&RouteRepository{pool: r.pool}).listPoints(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return &domain.GastroRouteAdminDetail{GastroRoute: *rt, Points: points}, nil
}

// CreateRoute inserts a route. It is always a draft: publication has its own
// precondition (at least one stop), and a create that could go straight live
// would let an editor publish an empty route in one call.
func (r *RouteEditorRepository) CreateRoute(ctx context.Context, in domain.GastroRouteWrite) (*domain.GastroRoute, error) {
	id := uuid.New()
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO gastro_routes
			(id, slug, title, title_i18n, description, description_i18n, cover_image_url,
			 duration_label, duration_label_i18n, city, status, position)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'draft', $11)`,
		id, in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Description, i18nToDB(in.DescriptionI18n),
		in.CoverImageURL, in.DurationLabel, i18nToDB(in.DurationLabelI18n),
		cityArg(in.City), in.Position)
	if err != nil {
		return nil, mapSlugConflict("create gastro route", err)
	}
	return r.getRoute(ctx, id)
}

// UpdateRoute replaces the editable fields and leaves status/published_at
// untouched — a typo fix must never change what a guest can see.
func (r *RouteEditorRepository) UpdateRoute(ctx context.Context, id uuid.UUID, in domain.GastroRouteWrite) (*domain.GastroRoute, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastro_routes
		 SET slug = $2, title = $3, title_i18n = $4, description = $5, description_i18n = $6,
			 cover_image_url = $7, duration_label = $8, duration_label_i18n = $9,
			 city = $10, position = $11, updated_at = now()
		 WHERE id = $1`,
		id, in.Slug, in.Title, i18nToDB(in.TitleI18n), in.Description, i18nToDB(in.DescriptionI18n),
		in.CoverImageURL, in.DurationLabel, i18nToDB(in.DurationLabelI18n),
		cityArg(in.City), in.Position)
	if err != nil {
		return nil, mapSlugConflict("update gastro route", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("update gastro route: %w", domain.ErrNotFound)
	}
	return r.getRoute(ctx, id)
}

// SetRouteStatus moves a route between publication states. The DB CHECK
// (published ⇒ published_at IS NOT NULL) is the backstop; the usecase supplies
// the time, so a violation here would be our bug, not an editor's.
func (r *RouteEditorRepository) SetRouteStatus(ctx context.Context, id uuid.UUID, status domain.GuideRouteStatus, publishedAt *time.Time) (*domain.GastroRoute, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastro_routes SET status = $2, published_at = $3, updated_at = now()
		 WHERE id = $1`, id, string(status), publishedAt)
	if err != nil {
		return nil, fmt.Errorf("set gastro route status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("set gastro route status: %w", domain.ErrNotFound)
	}
	return r.getRoute(ctx, id)
}

// CountPoints counts every stop of the route, whatever its kind and whatever
// the state of its venue — publication is checked against the ITINERARY, not
// against how much of it is bookable today.
func (r *RouteEditorRepository) CountPoints(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM gastro_route_points WHERE route_id = $1`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("count gastro route points: %w", err)
	}
	return n, nil
}

// --- points ---

// AddPoint appends a stop after the last one.
//
// It runs inside a transaction that first takes a row lock on the ROUTE.
// Without it two editors appending at the same moment both read the same
// max(position) and both write it, and the second commit dies on the unique
// constraint with a 500-shaped error. The lock makes stop edits on one route
// serial, which costs nothing and removes the race entirely.
func (r *RouteEditorRepository) AddPoint(ctx context.Context, routeID uuid.UUID, in domain.GuideRoutePointWrite) (*domain.GuideRoutePoint, error) {
	var out *domain.GuideRoutePoint
	err := r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.lockRoute(ctx, routeID); err != nil {
			return err
		}
		q := sqltx.From(ctx, r.pool)
		var next int
		if err := q.QueryRow(ctx,
			`SELECT COALESCE(max(position), 0) + 1 FROM gastro_route_points WHERE route_id = $1`,
			routeID).Scan(&next); err != nil {
			return fmt.Errorf("next gastro route point position: %w", err)
		}
		id := uuid.New()
		if _, err := q.Exec(ctx,
			`INSERT INTO gastro_route_points
				(id, route_id, position, kind, restaurant_id, title, title_i18n,
				 description, description_i18n, photo_url, address, address_i18n,
				 latitude, longitude)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, routeID, next, string(in.Kind), in.RestaurantID, in.Title, i18nToDB(in.TitleI18n),
			in.Description, i18nToDB(in.DescriptionI18n), in.PhotoURL, in.Address,
			i18nToDB(in.AddressI18n), in.Latitude, in.Longitude); err != nil {
			return mapPointWriteError("add gastro route point", err)
		}
		p, err := r.getPoint(ctx, routeID, id)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdatePoint replaces one stop's fields and keeps its position: moving a stop
// is the reorder operation, and letting an edit change the order would make a
// saved typo fix silently rearrange the walk.
func (r *RouteEditorRepository) UpdatePoint(ctx context.Context, routeID, pointID uuid.UUID, in domain.GuideRoutePointWrite) (*domain.GuideRoutePoint, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE gastro_route_points
		 SET kind = $3, restaurant_id = $4, title = $5, title_i18n = $6,
			 description = $7, description_i18n = $8, photo_url = $9,
			 address = $10, address_i18n = $11, latitude = $12, longitude = $13,
			 updated_at = now()
		 WHERE route_id = $1 AND id = $2`,
		routeID, pointID, string(in.Kind), in.RestaurantID, in.Title, i18nToDB(in.TitleI18n),
		in.Description, i18nToDB(in.DescriptionI18n), in.PhotoURL, in.Address,
		i18nToDB(in.AddressI18n), in.Latitude, in.Longitude)
	if err != nil {
		return nil, mapPointWriteError("update gastro route point", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("update gastro route point: %w", domain.ErrNotFound)
	}
	return r.getPoint(ctx, routeID, pointID)
}

// DeletePoint removes a stop and CLOSES THE GAP: every stop after it moves up
// one. A hole would be harmless for the guest (the read only sorts) but not for
// the editor — the next append computes max+1, and after a few delete/add
// cycles the numbers drift away from the visible order.
func (r *RouteEditorRepository) DeletePoint(ctx context.Context, routeID, pointID uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.lockRoute(ctx, routeID); err != nil {
			return err
		}
		q := sqltx.From(ctx, r.pool)
		var pos int
		err := q.QueryRow(ctx,
			`DELETE FROM gastro_route_points WHERE route_id = $1 AND id = $2 RETURNING position`,
			routeID, pointID).Scan(&pos)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("delete gastro route point: %w", domain.ErrNotFound)
			}
			return fmt.Errorf("delete gastro route point: %w", err)
		}
		if _, err := q.Exec(ctx,
			`UPDATE gastro_route_points SET position = position - 1
			 WHERE route_id = $1 AND position > $2`, routeID, pos); err != nil {
			return fmt.Errorf("close gastro route point gap: %w", err)
		}
		return nil
	})
}

// ReorderPoints writes a whole new ordering at once, on the same contract as
// the collection reorder: the payload is the intended FINAL sequence and must
// name exactly the route's current stops, each once. A missing, extra or
// repeated id means the editor's screen is stale, and guessing what they meant
// would silently rewrite an itinerary — it is CodeGuideOrderMismatch and
// nothing is written.
//
// All the new numbers land in ONE statement inside ONE transaction, which is
// what the DEFERRABLE unique (route_id, position) buys: a rotation passes
// through states where two stops share a number, and an immediate constraint
// would reject it row by row.
func (r *RouteEditorRepository) ReorderPoints(ctx context.Context, routeID uuid.UUID, pointIDs []uuid.UUID) error {
	return r.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := r.lockRoute(ctx, routeID); err != nil {
			return err
		}
		current, err := r.ListRoutePointIDs(ctx, routeID)
		if err != nil {
			return err
		}
		if err := sameSet(current, pointIDs); err != nil {
			return domain.WithCode(domain.CodeGuideOrderMismatch,
				fmt.Errorf("reorder gastro route points: %w: %s", domain.ErrValidation, err.Error()))
		}
		if len(pointIDs) == 0 {
			return nil
		}
		positions := make([]int32, len(pointIDs))
		for i := range pointIDs {
			positions[i] = int32(i + 1)
		}
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`UPDATE gastro_route_points p
			 SET position = t.pos
			 FROM unnest($2::uuid[], $3::int[]) AS t(pid, pos)
			 WHERE p.route_id = $1 AND p.id = t.pid`,
			routeID, pointIDs, positions); err != nil {
			return fmt.Errorf("reorder gastro route points: %w", err)
		}
		return nil
	})
}

// ListRoutePointIDs returns the stops in editorial order.
func (r *RouteEditorRepository) ListRoutePointIDs(ctx context.Context, routeID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id FROM gastro_route_points WHERE route_id = $1 ORDER BY position, id`, routeID)
	if err != nil {
		return nil, fmt.Errorf("list gastro route point ids: %w", err)
	}
	defer rows.Close()
	out := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan gastro route point id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate gastro route point ids: %w", err)
	}
	return out, nil
}

// --- helpers ---

func (r *RouteEditorRepository) getRoute(ctx context.Context, id uuid.UUID) (*domain.GastroRoute, error) {
	rt, err := scanRoute(sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+routeCols+` FROM gastro_routes rt WHERE rt.id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read gastro route: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("read gastro route: %w", err)
	}
	return rt, nil
}

// getPoint reads one stop with the EDITOR's venue join (dark venues included),
// so the cabinet gets the same row shape from a write as from a read.
func (r *RouteEditorRepository) getPoint(ctx context.Context, routeID, pointID uuid.UUID) (*domain.GuideRoutePoint, error) {
	p, err := scanRoutePoint(sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+pointCols+pointFrom("", "AND p.id = $2"), routeID, pointID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("read gastro route point: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("read gastro route point: %w", err)
	}
	return p, nil
}

// lockRoute takes a row lock on the route and doubles as its existence check,
// so a stop write against an unknown route is ErrNotFound rather than a
// foreign-key 500.
func (r *RouteEditorRepository) lockRoute(ctx context.Context, id uuid.UUID) error {
	var found uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT id FROM gastro_routes WHERE id = $1 FOR UPDATE`, id).Scan(&found)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("gastro route: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("lock gastro route: %w", err)
	}
	return nil
}

// mapPointWriteError turns the two schema refusals an editor can actually cause
// into domain errors: an unknown restaurant is ErrNotFound (not a 500), and a
// CHECK violation — a place stop carrying a venue, half a coordinate pair, an
// out-of-range latitude — is ErrValidation, because the usecase already refuses
// all of them and reaching this branch means the request was built by hand.
func mapPointWriteError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case foreignKeyViolation:
			return fmt.Errorf("%s: %w: unknown restaurant", op, domain.ErrNotFound)
		case checkViolation:
			return fmt.Errorf("%s: %w: %s", op, domain.ErrValidation, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

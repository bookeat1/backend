// Package venuefeature is the Postgres implementation of
// domain.VenueFeatureRepository: the platform-wide venue-feature dictionary
// («удобства», migration 0082) and the many-to-many link between a restaurant
// and its features.
//
// It is a deliberate mirror of the cuisine repository next door: the two
// dictionaries answer different questions but have the same shape, and keeping
// them literally parallel is what stops one of them drifting into its own
// dialect of ordering, error mapping and i18n handling.
package venuefeature

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.VenueFeatureRepository = (*Repository)(nil)

const cols = `id, code, name, name_i18n, display_order, is_active, created_at, updated_at`

// listOrder is the dictionary's canonical ordering, shared by every read so
// the app, the panel and the venue picker never see the same list in two
// different orders. `name` is the tie-break, `id` makes it total. Written with
// the `f.` alias because every read of this dictionary is a join.
const listOrder = `ORDER BY f.display_order ASC, f.name ASC, f.id ASC`

// List returns the dictionary with VenueCount filled.
//
// The count is computed in the same query by a correlated subquery rather than
// a GROUP BY join, for one reason: a LEFT JOIN + GROUP BY would drop nothing
// but WOULD make the "no venues yet" case indistinguishable from a bug in the
// join, and every feature we just seeded is in exactly that case. A scalar
// subquery returns an honest 0.
//
// Only ACTIVE venues are counted: the number exists to tell the owner (and
// later, a dashboard) how many places a guest could actually reach through
// this filter, and a hidden venue is not reachable.
func (r *Repository) List(ctx context.Context, f domain.VenueFeatureFilter) ([]domain.VenueFeature, error) {
	q := `SELECT ` + prefixed(cols, "f") + `,
	        (SELECT count(*) FROM restaurant_venue_features rvf
	           JOIN restaurants r ON r.id = rvf.restaurant_id AND r.is_active = true
	          WHERE rvf.feature_id = f.id) AS venue_count
	      FROM venue_features f`
	if !f.IncludeInactive {
		q += ` WHERE f.is_active = true`
	}
	q += ` ` + listOrder

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list venue features: %w", err)
	}
	defer rows.Close()

	out := make([]domain.VenueFeature, 0)
	for rows.Next() {
		var vf domain.VenueFeature
		var nameI18n []byte
		if err := rows.Scan(&vf.ID, &vf.Code, &vf.Name, &nameI18n, &vf.DisplayOrder,
			&vf.IsActive, &vf.CreatedAt, &vf.UpdatedAt, &vf.VenueCount); err != nil {
			return nil, fmt.Errorf("list venue features: %w", err)
		}
		vf.NameI18n = i18nFromDB(nameI18n)
		out = append(out, vf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list venue features: %w", err)
	}
	return out, nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.VenueFeature, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+cols+` FROM venue_features WHERE id = $1`, id)
	vf, err := scanFeature(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return vf, err
}

// selfAliases are the spellings every dictionary entry MUST be findable by:
// its display name and its machine code. This is the exact pair migration 0082
// seeds for the nineteen entries that exist today (`lower(btrim(name))` +
// `lower(btrim(code))`), reproduced here so a feature added through the admin
// route is born with the same rows instead of being invisible to the catalog
// filter until someone runs SQL by hand.
//
// For features this matters MORE than for cuisines: featureVenueMatchExpr in
// the restaurant repository resolves a `?features=` key through
// venue_feature_aliases ONLY — there is no legacy-string fallback branch like
// the cuisine expression has — so an entry without alias rows is invisible to
// the filter completely, by code and by name alike.
//
// Normalization goes through domain.NormalizeFeatureKey and NOT through SQL
// `lower(btrim(...))`: the lookup side compares against
// domain.NormalizeFeatureKeys(f.Features), so the two sides must use one
// function. NormalizeFeatureKey additionally collapses inner whitespace, which
// the migration's SQL does not — that is a superset, never a mismatch, and it
// is pinned by TestFeatureAliasNormalizationMatchesTheSearchKey.
func selfAliases(vf *domain.VenueFeature) []string {
	return domain.NormalizeFeatureKeys([]string{vf.Name, vf.Code})
}

// aliasInsert is the second half of Create/Update: it writes the entry's own
// spellings into venue_feature_aliases inside the SAME statement as the
// dictionary write, so a feature can never exist without them. One statement is
// also one transaction even on a bare pool, which is why this needs no
// TxManager and still cannot half-apply.
//
// The NOT EXISTS makes a repeat harmless: an alias this feature already owns is
// skipped instead of raising 23505, so re-saving an entry (or PATCHing one that
// predates this code and therefore has no aliases yet) is idempotent and
// self-healing.
//
// An alias owned by ANOTHER feature is deliberately NOT skipped: the insert
// then hits the primary key, the whole statement aborts, and the caller gets
// ErrAlreadyExists. Silently dropping it would leave the new entry unfindable
// by its own name or code — exactly the failure this code exists to prevent —
// and silently re-pointing it would steal a search key from a feature that
// already answers to it.
//
// KNOWN, ACCEPTED: two admins saving the SAME entry in the same instant can
// both pass the NOT EXISTS and the loser gets that conflict too — a spurious
// 409 on a legitimate save, safe (nothing is written) and fixed by retrying,
// which is why the error text does not claim another feature owns the
// spelling. (Same trade-off as the cuisine repository next door.)
const aliasInsert = `INSERT INTO venue_feature_aliases (alias, feature_id)
	SELECT a, w.id FROM w CROSS JOIN unnest($7::text[]) AS a
	 WHERE NOT EXISTS (SELECT 1 FROM venue_feature_aliases fa
	                    WHERE fa.alias = a AND fa.feature_id = w.id)`

func (r *Repository) Create(ctx context.Context, vf *domain.VenueFeature) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`WITH w AS (
			INSERT INTO venue_features (`+cols+`)
			VALUES ($1,$2,$3,$4,$5,$6,now(),now())
			RETURNING id
		)
		`+aliasInsert,
		vf.ID, vf.Code, vf.Name, i18nToDB(vf.NameI18n), vf.DisplayOrder, vf.IsActive,
		selfAliases(vf))
	if err != nil {
		return mapWrite(err, "create venue feature")
	}
	// A brand-new dictionary entry is carried by nobody yet; say so explicitly
	// instead of leaving whatever the caller happened to pass in.
	vf.VenueCount = 0
	return nil
}

// Update rewrites the entry and ADDS the aliases of its current name/code. It
// never removes the old ones: venue_feature_aliases is the set of spellings
// that MEAN this feature, not a projection of its current name. A former name
// is still a spelling — it sits in the chips an already-installed app scraped
// out of the catalog — and it also holds the hand-curated synonyms migration
// 0082 seeded («бизнес ланч» → business_lunch), which a rename has no business
// deleting.
func (r *Repository) Update(ctx context.Context, vf *domain.VenueFeature) error {
	// The command tag of the statement below counts ALIAS rows, not feature
	// rows (0 is normal — the aliases are usually already there), so "does the
	// id exist" has to come from the UPDATE's own RETURNING.
	var id uuid.UUID
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`WITH w AS (
			UPDATE venue_features SET code=$2, name=$3, name_i18n=$4,
			       display_order=$5, is_active=$6, updated_at=now()
			 WHERE id=$1
			RETURNING id
		), a AS (
			`+aliasInsert+`
		)
		SELECT id FROM w`,
		vf.ID, vf.Code, vf.Name, i18nToDB(vf.NameI18n), vf.DisplayOrder, vf.IsActive,
		selfAliases(vf)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return mapWrite(err, "update venue feature")
	}
	return nil
}

// ListByRestaurants loads the feature sets of a whole page in ONE query. The
// ordering inside a venue is the link position first, then the dictionary
// order as a stable tie-break.
//
// Hidden dictionary entries are still returned here: a venue that already
// carries a feature the platform later hid must keep rendering what it has —
// hiding stops the feature SPREADING (the usecase refuses to assign it), it
// does not silently strip it off the venues that already had it.
func (r *Repository) ListByRestaurants(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID][]domain.VenueFeature, error) {
	out := make(map[uuid.UUID][]domain.VenueFeature, len(restaurantIDs))
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT rvf.restaurant_id, `+prefixed(cols, "f")+`
		 FROM restaurant_venue_features rvf
		 JOIN venue_features f ON f.id = rvf.feature_id
		 WHERE rvf.restaurant_id = ANY($1)
		 ORDER BY rvf.restaurant_id, rvf.position ASC, f.display_order ASC, f.name ASC`,
		restaurantIDs)
	if err != nil {
		return nil, fmt.Errorf("list restaurant features: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rid uuid.UUID
		var vf domain.VenueFeature
		var nameI18n []byte
		if err := rows.Scan(&rid, &vf.ID, &vf.Code, &vf.Name, &nameI18n,
			&vf.DisplayOrder, &vf.IsActive, &vf.CreatedAt, &vf.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list restaurant features: %w", err)
		}
		vf.NameI18n = i18nFromDB(nameI18n)
		out[rid] = append(out[rid], vf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restaurant features: %w", err)
	}
	return out, nil
}

// ResolveIDs returns the requested features IN THE ORDER GIVEN and fails when
// any id is unknown.
func (r *Repository) ResolveIDs(ctx context.Context, ids []uuid.UUID) ([]domain.VenueFeature, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+cols+` FROM venue_features WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve venue features: %w", err)
	}
	defer rows.Close()

	byID := make(map[uuid.UUID]domain.VenueFeature, len(ids))
	for rows.Next() {
		vf, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		byID[vf.ID] = *vf
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve venue features: %w", err)
	}

	out := make([]domain.VenueFeature, 0, len(ids))
	for _, id := range ids {
		vf, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: unknown venue feature %s", domain.ErrValidation, id)
		}
		out = append(out, vf)
	}
	return out, nil
}

// SetForRestaurant replaces the venue's whole set. Delete + inserts are
// separate statements: callers run it inside a transaction.
func (r *Repository) SetForRestaurant(ctx context.Context, restaurantID uuid.UUID, ids []uuid.UUID) error {
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM restaurant_venue_features WHERE restaurant_id = $1`, restaurantID); err != nil {
		return fmt.Errorf("set restaurant features: %w", err)
	}
	for i, id := range ids {
		if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
			`INSERT INTO restaurant_venue_features (restaurant_id, feature_id, position, created_at)
			 VALUES ($1,$2,$3,now())
			 ON CONFLICT (restaurant_id, feature_id) DO UPDATE SET position = EXCLUDED.position`,
			restaurantID, id, i); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
				return fmt.Errorf("%w: unknown restaurant or venue feature", domain.ErrValidation)
			}
			return fmt.Errorf("set restaurant features: %w", err)
		}
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanFeature(row scanner) (*domain.VenueFeature, error) {
	var vf domain.VenueFeature
	var nameI18n []byte
	if err := row.Scan(&vf.ID, &vf.Code, &vf.Name, &nameI18n,
		&vf.DisplayOrder, &vf.IsActive, &vf.CreatedAt, &vf.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan venue feature: %w", err)
	}
	vf.NameI18n = i18nFromDB(nameI18n)
	return &vf, nil
}

// prefixed qualifies a comma-separated column list with a table alias, so the
// dictionary's column list is written once and reused in a join.
func prefixed(list, alias string) string {
	parts := strings.Split(list, ",")
	for i, col := range parts {
		parts[i] = alias + "." + strings.TrimSpace(col)
	}
	return strings.Join(parts, ", ")
}

// aliasPK is the primary key of venue_feature_aliases (one row per spelling).
// It is named explicitly so a taken spelling can be reported as such instead of
// as "code or name already used", which would send the admin looking at the
// wrong table.
const aliasPK = "venue_feature_aliases_pkey"

// mapWrite turns a unique-index violation into domain.ErrAlreadyExists. The
// unique indexes (code, normalized name, alias) are the ONLY duplicate guard:
// checking first and inserting after loses the race between two admins.
func mapWrite(err error, ctxMsg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		if pgErr.ConstraintName == aliasPK {
			return fmt.Errorf("%w: venue feature spelling already taken", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("%w: venue feature code or name already used", domain.ErrAlreadyExists)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
}

func i18nToDB(m domain.I18n) any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	return b
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

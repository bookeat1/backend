// Package restaurant is the Postgres implementation of the restaurant
// repositories.
package restaurant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/cuisine"
	"backend-core/internal/infrastructure/postgres/venuefeature"
	"backend-core/internal/infrastructure/sqltx"
)

const uniqueViolation = "23505"

// Repository implements domain.RestaurantRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the restaurant repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.RestaurantRepository = (*Repository)(nil)

const cols = `id, category_id, name, name_i18n, description, description_i18n,
	cuisine_type, cuisine_type_i18n, address, address_i18n, opening_hours,
	opening_hours_i18n, city, price_category, email, phone, latitude, longitude,
	kwaaka_restaurant_id, is_active, is_new, is_popular, is_premium,
	hidden_from_home, display_order, created_at, updated_at, price_min, price_max`

// policyCols are the venue's booking-policy overrides (all NULLABLE — NULL
// means "use the global default"). They are read only by GetByID: the policy is
// resolved per booking, and the catalog listing has no use for them. They are
// deliberately absent from cols so the Create/Update placeholder numbering
// stays untouched.
const policyCols = `timezone, booking_duration_minutes, booking_buffer_minutes,
	booking_lead_minutes, booking_horizon_days, cancel_deadline_minutes,
	confirm_sla_minutes, max_guests_per_booking, auto_confirm, confirm_on_create,
	booking_capacity_mode, booking_capacity_seats`

// listExtraCols are the columns a catalog LISTING row needs beyond cols, in the
// order scanListItem reads them.
//
// r.timezone is here — rather than the whole policyCols block — because the
// public payload reports an "open now" flag that MUST be computed in the
// venue's own zone. GetByID has always read the column; without it here, a
// venue outside the platform default zone would silently be judged against the
// fallback and reported open when it is shut.
//
// r.booking_capacity_mode / r.booking_capacity_seats are here for the same
// reason, one field further on: the payload also reports whether the venue can
// take an online booking at all, and for a seats-mode venue (0054) that answer
// comes from the declared seat count, not from the table list it deliberately
// does not keep. Without these two columns every table-less venue scans as
// table mode with zero tables and is published as unbookable — the exact
// venues seats mode exists to unblock.
//
// The rest of the booking policy is still resolved per booking and stays out of
// the listing.
const listExtraCols = `(SELECT image_url FROM restaurant_images i WHERE i.restaurant_id = r.id
		 ORDER BY i.is_primary DESC, i.created_at ASC LIMIT 1) AS primary_image,
	r.timezone, r.booking_capacity_mode, r.booking_capacity_seats`

func (r *Repository) Create(ctx context.Context, m *domain.Restaurant) error {
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	q := `INSERT INTO restaurants (` + cols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`
	_, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(m)...)
	if err != nil {
		return mapWrite(err, "create restaurant")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, m *domain.Restaurant) error {
	m.UpdatedAt = time.Now()
	q := `UPDATE restaurants SET category_id=$2, name=$3, name_i18n=$4, description=$5,
		description_i18n=$6, cuisine_type=$7, cuisine_type_i18n=$8, address=$9,
		address_i18n=$10, opening_hours=$11, opening_hours_i18n=$12, city=$13,
		price_category=$14, email=$15, phone=$16, latitude=$17, longitude=$18,
		kwaaka_restaurant_id=$19, is_active=$20, is_new=$21, is_popular=$22,
		is_premium=$23, hidden_from_home=$24, display_order=$25, updated_at=$26,
		price_min=$27, price_max=$28
		WHERE id=$1`
	// Built explicitly (not sliced out of r.args) so adding an INSERT column
	// can't silently shift the UPDATE placeholders out of alignment. Update
	// intentionally omits created_at.
	args := []any{
		m.ID, m.CategoryID, m.Name, i18nToDB(m.NameI18n), m.Description,
		i18nToDB(m.DescriptionI18n), m.CuisineType, i18nToDB(m.CuisineTypeI18n),
		m.Address, i18nToDB(m.AddressI18n), m.OpeningHours, i18nToDB(m.OpeningHoursI18n),
		string(m.City), string(m.PriceCategory), m.Email, m.Phone, m.Latitude, m.Longitude,
		m.KwaakaRestaurantID, m.IsActive, m.IsNew, m.IsPopular, m.IsPremium,
		m.HiddenFromHome, m.DisplayOrder, m.UpdatedAt, m.PriceMin, m.PriceMax,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update restaurant")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// UpdateCuisineTypeString rewrites ONLY the two derived backward-compatibility
// columns after a venue's cuisine set changed (see usecase/cuisines).
//
// `cuisine_type` used to be the source of truth typed by hand; since migration
// 0079 the source of truth is `restaurant_cuisines`, and this column is its
// comma-joined rendering, kept alive for the store builds (1.4 / 1.5) that read
// one string. Writing it through a narrow UPDATE — instead of the full Update
// above — is deliberate: a read-modify-write of the whole row from a caller
// that only knows about cuisines is how a concurrent edit of the venue profile
// gets silently reverted.
func (r *Repository) UpdateCuisineTypeString(ctx context.Context, id uuid.UUID, cuisineType string, i18n domain.I18n) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurants SET cuisine_type=$2, cuisine_type_i18n=$3, updated_at=now()
		 WHERE id=$1`, id, cuisineType, i18nToDB(i18n))
	if err != nil {
		return fmt.Errorf("update derived cuisine_type: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// RenameCityString rewrites ONLY the derived city string of every venue linked
// to a city, after the dictionary entry was renamed (see usecase/cities).
//
// `restaurants.city` is the backward-compatibility rendering of `city_id`
// exactly the way `cuisine_type` renders `restaurant_cuisines`: a store build
// reads that one string and sends it back as ?city=. Leaving it at the old
// spelling after a rename would make the venue's own screen and the catalog
// filter disagree about where the venue is.
//
// Scoped by city_id (not by the old string): a venue whose string was already
// out of sync still gets fixed, and a venue in another city can never be
// touched. Returns the number of venues rewritten — the caller logs it, and a
// surprising number is the first sign a rename hit more than intended.
func (r *Repository) RenameCityString(ctx context.Context, cityID uuid.UUID, name string) (int64, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurants SET city=$2, updated_at=now()
		 WHERE city_id=$1 AND city IS DISTINCT FROM $2`, cityID, name)
	if err != nil {
		return 0, fmt.Errorf("rename venue city string: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurants SET is_active=$2, updated_at=now() WHERE id=$1`, id, active)
	if err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// UpdateBookingPolicy patches the venue's booking-policy overrides. PATCH
// semantics: a nil field of o leaves its column untouched (so a NULL — "use the
// global default" — survives), a non-nil field is written. The SET list and its
// placeholders are built from scratch for this statement only, so the fixed
// numbering of Create/Update is unaffected.
func (r *Repository) UpdateBookingPolicy(ctx context.Context, id uuid.UUID, o domain.BookingPolicyOverride) error {
	args := []any{id}
	sets := make([]string, 0, 10)
	set := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s=$%d", col, len(args)))
	}
	if v := o.Timezone; v != nil {
		set("timezone", *v)
	}
	if v := o.BookingDurationMinutes; v != nil {
		set("booking_duration_minutes", *v)
	}
	if v := o.BookingBufferMinutes; v != nil {
		set("booking_buffer_minutes", *v)
	}
	if v := o.BookingLeadMinutes; v != nil {
		set("booking_lead_minutes", *v)
	}
	if v := o.BookingHorizonDays; v != nil {
		set("booking_horizon_days", *v)
	}
	if v := o.CancelDeadlineMinutes; v != nil {
		set("cancel_deadline_minutes", *v)
	}
	if v := o.ConfirmSLAMinutes; v != nil {
		set("confirm_sla_minutes", *v)
	}
	if v := o.MaxGuestsPerBooking; v != nil {
		set("max_guests_per_booking", *v)
	}
	if v := o.AutoConfirm; v != nil {
		set("auto_confirm", *v)
	}
	if v := o.ConfirmOnCreate; v != nil {
		set("confirm_on_create", *v)
	}
	if v := o.BookingCapacityMode; v != nil {
		set("booking_capacity_mode", string(*v))
	}
	if v := o.BookingCapacitySeats; v != nil {
		set("booking_capacity_seats", *v)
	}

	if len(sets) == 0 {
		// An empty patch is a no-op, but it must still report ErrNotFound for an
		// unknown venue rather than a silent success.
		return r.exists(ctx, id)
	}
	// updated_at is bumped through now() instead of a Go timestamp: this is a
	// blind UPDATE with no prior read, so there is no in-memory row to keep in
	// sync and the DB clock is the authoritative one.
	sets = append(sets, "updated_at=now()")

	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE restaurants SET `+strings.Join(sets, ", ")+` WHERE id=$1`, args...)
	if err != nil {
		return fmt.Errorf("update booking policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// exists returns nil when the restaurant is present, domain.ErrNotFound otherwise.
func (r *Repository) exists(ctx context.Context, id uuid.UUID) error {
	var one int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT 1 FROM restaurants WHERE id=$1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check restaurant: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+`, `+policyCols+` FROM restaurants WHERE id=$1`, id)
	base, err := scanRestaurantWithPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get restaurant: %w", err)
	}
	agg := &domain.RestaurantAggregate{Restaurant: *base}
	rel := &Related{pool: r.pool}
	if agg.Images, err = rel.ListImages(ctx, id); err != nil {
		return nil, err
	}
	if agg.Tags, err = rel.ListTags(ctx, id); err != nil {
		return nil, err
	}
	if agg.SocialLinks, err = rel.ListSocialLinks(ctx, id); err != nil {
		return nil, err
	}
	byVenue, err := cuisine.New(r.pool).ListByRestaurants(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	agg.Cuisines = byVenue[id]
	// Features come from the platform dictionary since migration 0082 — the
	// free-text restaurant_features table this used to read was dropped there.
	featByVenue, err := venuefeature.New(r.pool).ListByRestaurants(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	agg.Features = featByVenue[id]
	return agg, nil
}

func (r *Repository) ListActive(ctx context.Context, f domain.RestaurantFilter) ([]domain.RestaurantListItem, int, error) {
	// The admin catalog asks for hidden venues too (see IncludeInactive); every
	// other caller gets the active-only listing this method is named after.
	// "true" keeps the WHERE clause valid when the admin listing asks for every
	// venue and no other filter is set.
	where := []string{"true"}
	if !f.IncludeInactive {
		where = append(where, "r.is_active = true")
	}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		add("r.city = $%d", string(*f.City))
	}
	if f.Category != nil {
		add("r.category_id = $%d", *f.Category)
	}
	if len(f.IDs) > 0 {
		add("r.id = ANY($%d)", f.IDs)
	}
	if f.IsPopular != nil {
		add("r.is_popular = $%d", *f.IsPopular)
	}
	if f.IsNew != nil {
		add("r.is_new = $%d", *f.IsNew)
	}
	if keys := domain.NormalizeFeatureKeys(f.Features); len(keys) > 0 {
		where, args = appendFeatureConds(where, args, keys)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		// Escape LIKE wildcards so a term containing % or _ matches literally
		// instead of turning "%" into a match-everything filter.
		add(`r.name ILIKE '%%' || $%d || '%%' ESCAPE '\'`, escapeLike(s))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM restaurants r WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count restaurants: %w", err)
	}

	limit, offset := limitOffset(f.Page, f.PerPage, f.Unpaginated)
	args = append(args, limit, offset)
	q := `SELECT ` + prefixed(cols, "r") + `, ` + listExtraCols + `
		FROM restaurants r WHERE ` + whereSQL + `
		ORDER BY r.display_order ASC NULLS LAST, r.name ASC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list restaurants: %w", err)
	}
	defer rows.Close()

	var items []domain.RestaurantListItem
	for rows.Next() {
		base, primary, err := scanListItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, domain.RestaurantListItem{Restaurant: *base, PrimaryImage: primary})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list restaurants: %w", err)
	}
	if err := r.attachCuisines(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.attachFeatures(ctx, items); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// attachCuisines fills the cuisine set of a whole page in ONE extra query, so
// a 20-item catalog page costs two round trips rather than twenty-one. Loading
// it here — rather than in a usecase — is what keeps EVERY consumer of
// RestaurantListItem consistent: the catalog listing, the search and the
// favorites screen all render the same venue with the same cuisines.
func (r *Repository) attachCuisines(ctx context.Context, items []domain.RestaurantListItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].Restaurant.ID)
	}
	byVenue, err := cuisine.New(r.pool).ListByRestaurants(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Cuisines = byVenue[items[i].Restaurant.ID]
	}
	return nil
}

// attachFeatures does for the feature set what attachCuisines does for
// cuisines: one extra query per page, loaded in the repository so the catalog,
// the search and the favorites screen all render the same venue with the same
// features.
func (r *Repository) attachFeatures(ctx context.Context, items []domain.RestaurantListItem) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].Restaurant.ID)
	}
	byVenue, err := venuefeature.New(r.pool).ListByRestaurants(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Features = byVenue[items[i].Restaurant.ID]
	}
	return nil
}

// searchTextExpr is the exact SQL expression the FTS and trigram indexes in
// migration 0026 are built over (restaurant_search_text applied to the four
// searchable columns). The query below must reference it verbatim — down to
// column order — or the planner will not use those GIN indexes. The `r.`
// alias resolves to the same columns the unqualified index expression names,
// which is why the index still matches. See the migration's comment for the
// all-locale rationale.
// cuisineMatchExpr matches a venue against an OR-set of NORMALIZED cuisine
// keys (domain.NormalizeCuisineKeys: trimmed, lower-cased, spaces collapsed).
// $%d is a text[], cast explicitly on both uses: the same bound parameter
// feeding two differently-typed comparisons is exactly the shape that trips
// SQLSTATE 42P08 under the extended protocol (see
// bugs/bookeat-backend-cas-sql-type-inference.md).
//
// Two branches, and both are required:
//
//  1. the dictionary link — the venue has a cuisine whose alias (or code, which
//     is seeded as an alias) is one of the requested keys. This is what makes a
//     venue with SEVERAL cuisines findable by each of them;
//  2. the legacy string — the venue's own `cuisine_type`, normalized the same
//     way, matched BOTH whole and comma-part by comma-part. This is not a
//     leftover: venues whose composite value is still awaiting a manual split
//     have no links yet, and dropping this branch would make them disappear
//     from the very filter they answer today. The per-part comparison is what
//     keeps an already-installed app working: it scrapes its chips out of the
//     catalog, so its chip literally reads «Кафе, европейская», the transport
//     splits that on commas, and neither half would equal the whole string.
//     Splitting HERE decides nothing and writes nothing — the raw value stays
//     exactly as it is until a human rules on it.
//
// Case-insensitivity is itself a fix: before this, «Европейская» and
// «европейская» were two different filters, and the app sent whichever spelling
// it had scraped out of the catalog.
const cuisineMatchExpr = `(
	EXISTS (SELECT 1 FROM restaurant_cuisines rc
	          JOIN cuisine_aliases ca ON ca.cuisine_id = rc.cuisine_id
	         WHERE rc.restaurant_id = r.id AND ca.alias = ANY($%d::text[]))
	OR lower(btrim(r.cuisine_type)) = ANY($%d::text[])
	OR EXISTS (SELECT 1 FROM unnest(string_to_array(r.cuisine_type, ',')) AS part
	            WHERE lower(btrim(part)) = ANY($%d::text[]))
)`

// featureVenueMatchExpr matches a venue that carries ONE feature, identified by
// any approved spelling: the feature's own name, its code, or a row in
// venue_feature_aliases (both name and code are seeded as aliases by migration
// 0082, so this single lookup covers all three).
//
// $%d is a single text key, cast explicitly — same 42P08 discipline as the
// cuisine expression next door.
const featureVenueMatchExpr = `EXISTS (
	SELECT 1 FROM restaurant_venue_features rvf
	  JOIN venue_feature_aliases fa ON fa.feature_id = rvf.feature_id
	 WHERE rvf.restaurant_id = r.id AND fa.alias = $%d::text)`

// appendFeatureConds AND-combines one EXISTS per requested feature.
//
// AND, not OR, and that is the whole point (decision 2026-08-25): a guest who
// ticked «Намазхана» and «Парковка» is asking for a place with both, and a
// venue with only one of them is not an answer. Cuisine is the opposite — see
// cuisineMatchExpr, which is a single OR-set.
//
// One subquery per key rather than a `GROUP BY ... HAVING count(*) = n` because
// the planner can drive each EXISTS off idx_restaurant_venue_features_feature
// and stop at the first hit, and because it degrades honestly: an unknown key
// (a typo, or a feature no venue carries) simply matches nothing, which is the
// truthful answer to "show me venues with X" — never a silently dropped filter.
//
// It returns the grown args/where slices, in the same shape the callers' local
// `add` helper uses.
func appendFeatureConds(where []string, args []any, keys []string) ([]string, []any) {
	for _, k := range keys {
		args = append(args, k)
		where = append(where, fmt.Sprintf(featureVenueMatchExpr, len(args)))
	}
	return where, args
}

const searchTextExpr = `restaurant_search_text(r.name, r.description, r.name_i18n, r.description_i18n)`

// menuSearchTextExpr is the exact SQL expression the two GIN indexes in
// migration 0095 are built over (menu_item_search_text applied to the dish name
// and its translations). Same rule as searchTextExpr: reference it VERBATIM or
// the planner will not use the indexes. `m.` resolves to the same columns the
// unqualified index expression names.
const menuSearchTextExpr = `menu_item_search_text(m.name, m.name_i18n)`

// menuMatchExpr is the text predicate over a menu item: FTS match OR trigram
// word-similarity, the same two branches (and therefore the same typo
// tolerance) the venue's own text gets. $%d is the query, cast ::text at every
// use site — the parameter feeds plainto_tsquery, <% and word_similarity, and a
// bare $n reused across differently-typed call sites trips SQLSTATE 42P08 (see
// bugs/bookeat-backend-cas-sql-type-inference.md).
//
// `m.is_available` is NOT decoration: it is both the product rule (a dish in
// the stop list must not pull its venue into the result) and the predicate of
// the partial indexes — drop it and the indexes stop matching.
const menuMatchExpr = `m.is_available
	AND (to_tsvector('russian', ` + menuSearchTextExpr + `) @@ plainto_tsquery('russian', $%[1]d::text)
	     OR $%[1]d::text <%% ` + menuSearchTextExpr + `)`

// menuMatchJoin is a LEFT JOIN over ONE pre-aggregated, UNCORRELATED scan of
// the matching dishes, keyed by venue. Shape matters:
//
//   - uncorrelated + grouped means Postgres runs the menu side ONCE, driven by
//     the GIN indexes, and hashes the result; the outer scan then costs a hash
//     probe per venue. A correlated `EXISTS (... WHERE m.restaurant_id = r.id)`
//     under an OR degrades into a per-row SubPlan instead — same answer, one
//     index probe per venue;
//   - it is a LEFT JOIN, not an inner one, because a venue matched by its OWN
//     name must still come back when nothing in its menu matches;
//   - the aggregates are what let the ORDER BY rank dish-only hits among
//     themselves instead of leaving them all tied at zero.
const menuMatchJoin = `LEFT JOIN (
		SELECT m.restaurant_id,
		       max(ts_rank(to_tsvector('russian', ` + menuSearchTextExpr + `),
		                   plainto_tsquery('russian', $%[1]d::text))) AS fts_rank,
		       max(word_similarity($%[1]d::text, ` + menuSearchTextExpr + `)) AS sim
		  FROM menu_items m
		 WHERE ` + menuMatchExpr + `
		 GROUP BY m.restaurant_id
	) mm ON mm.restaurant_id = r.id`

// Search implements domain.RestaurantRepository.Search: a full-text +
// trigram-fuzzy search over the localized name/description of active
// restaurants AND over the localized names of their available menu items,
// combined with the city / cuisine / price / feature filters.
func (r *Repository) Search(ctx context.Context, f domain.RestaurantSearchFilter) ([]domain.RestaurantListItem, int, error) {
	where := []string{"r.is_active = true"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		add("r.city = $%d", string(*f.City))
	}
	if keys := domain.NormalizeCuisineKeys(f.Cuisines); len(keys) > 0 {
		// Bound once, referenced three times — hence not add(), which assumes
		// a single placeholder.
		args = append(args, keys)
		where = append(where, fmt.Sprintf(cuisineMatchExpr, len(args), len(args), len(args)))
	}
	if keys := domain.NormalizeFeatureKeys(f.Features); len(keys) > 0 {
		where, args = appendFeatureConds(where, args, keys)
	}
	if f.Price != nil {
		add("r.price_category = $%d", string(*f.Price))
	}

	// qN is the placeholder holding the search text, referenced several times
	// (WHERE + ORDER BY). It is bound once and reused. An explicit ::text cast on
	// every use keeps pgx's extended-protocol type inference unambiguous — the
	// same parameter feeds plainto_tsquery, the <% operator and word_similarity,
	// and a bare $n reused across differently-typed call sites can trip SQLSTATE
	// 42P08 (see bugs/bookeat-backend-cas-sql-type-inference.md).
	q := strings.TrimSpace(f.Query)
	var qN int
	// joinSQL is empty unless there is a text query: without one there is
	// nothing to match a dish against, and the browse must not pay for a join.
	joinSQL := ""
	// venueMatch is the venue's OWN text predicate, reused verbatim in the WHERE
	// and in the ORDER BY (where it separates "found by its name" from "found by
	// its menu"). Kept as one string so those two can never drift apart.
	venueMatch := ""
	if q != "" {
		args = append(args, q)
		qN = len(args)
		// FTS match OR trigram word-similarity (typo tolerance). Both branches
		// are index-backed (idx_restaurants_search_fts / _trgm), so the planner
		// can BitmapOr them instead of scanning.
		venueMatch = fmt.Sprintf(
			`(to_tsvector('russian', %s) @@ plainto_tsquery('russian', $%d::text)
			  OR $%d::text <%% %s)`,
			searchTextExpr, qN, qN, searchTextExpr)
		joinSQL = fmt.Sprintf(menuMatchJoin, qN)
		// The venue answers the query either about itself or through its menu.
		// mm.restaurant_id IS NOT NULL is "the LEFT JOIN found a matching
		// available dish" — see menuMatchJoin for why this is a hashed join and
		// not a correlated EXISTS.
		where = append(where, `(`+venueMatch+` OR mm.restaurant_id IS NOT NULL)`)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM restaurants r `+joinSQL+` WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search restaurants: %w", err)
	}

	// Ordering: with a text query, a venue matched by its own name/description
	// outranks one matched only through a dish — the guest typed a word, and
	// "this place IS that" is a stronger answer than "this place cooks that".
	// Within each class: FTS relevance, then trigram word-similarity, then the
	// best-matching dish's ranks, tie-broken by id so a page boundary is
	// deterministic (equal-ranked rows never reshuffle between pages). Without a
	// query the endpoint degrades to a filtered browse ordered like the catalog
	// listing.
	orderSQL := `ORDER BY r.display_order ASC NULLS LAST, r.name ASC, r.id ASC`
	if q != "" {
		orderSQL = fmt.Sprintf(
			`ORDER BY %s DESC,
			 ts_rank(to_tsvector('russian', %s), plainto_tsquery('russian', $%d::text)) DESC,
			 word_similarity($%d::text, %s) DESC,
			 COALESCE(mm.fts_rank, 0) DESC, COALESCE(mm.sim, 0) DESC, r.id ASC`,
			venueMatch, searchTextExpr, qN, qN, searchTextExpr)
	}

	limit, offset := limitOffset(f.Page, f.PerPage, f.Unpaginated)
	args = append(args, limit, offset)
	q2 := `SELECT ` + prefixed(cols, "r") + `, ` + listExtraCols + `
		FROM restaurants r ` + joinSQL + ` WHERE ` + whereSQL + `
		` + orderSQL + `
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q2, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search restaurants: %w", err)
	}
	defer rows.Close()

	var items []domain.RestaurantListItem
	for rows.Next() {
		base, primary, err := scanListItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, domain.RestaurantListItem{Restaurant: *base, PrimaryImage: primary})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("search restaurants: %w", err)
	}
	if err := r.attachCuisines(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.attachFeatures(ctx, items); err != nil {
		return nil, 0, err
	}
	if err := r.attachMatchedDishes(ctx, items, q); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// attachMatchedDishes fills RestaurantListItem.MatchedDish for the rows that a
// menu item pulled in, so the card can say WHY the venue is in the result.
//
// One extra query per page, batched over the page's ids — the same shape as
// attachCuisines / attachFeatures. Deliberately not a lateral join inside the
// main query: that would drag the dish columns through the count query, the
// ordering and every scan path, to decorate at most `per_page` rows.
//
// DISTINCT ON picks ONE dish per venue — the best-matching available one, by
// the same FTS-then-trigram order the venues themselves are ranked with, with
// the id tie-break so the caption is stable between identical requests. A venue
// found by its own name and by a dish gets the dish too: that is not a
// contradiction, it is extra explanation the transport is free to ignore.
func (r *Repository) attachMatchedDishes(ctx context.Context, items []domain.RestaurantListItem, q string) error {
	if q == "" || len(items) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].Restaurant.ID)
	}
	sql := fmt.Sprintf(`SELECT DISTINCT ON (m.restaurant_id)
			m.restaurant_id, m.id, m.name, m.name_i18n
		  FROM menu_items m
		 WHERE m.restaurant_id = ANY($1::uuid[]) AND %s
		 ORDER BY m.restaurant_id,
			ts_rank(to_tsvector('russian', %s), plainto_tsquery('russian', $2::text)) DESC,
			word_similarity($2::text, %s) DESC, m.id ASC`,
		fmt.Sprintf(menuMatchExpr, 2), menuSearchTextExpr, menuSearchTextExpr)

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, sql, ids, q)
	if err != nil {
		return fmt.Errorf("matched dishes: %w", err)
	}
	defer rows.Close()

	byVenue := make(map[uuid.UUID]domain.MatchedDish, len(items))
	for rows.Next() {
		var venueID uuid.UUID
		var d domain.MatchedDish
		var nameI18n []byte
		if err := rows.Scan(&venueID, &d.ID, &d.Name, &nameI18n); err != nil {
			return fmt.Errorf("scan matched dish: %w", err)
		}
		d.NameI18n = i18nFromDB(nameI18n)
		byVenue[venueID] = d
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("matched dishes: %w", err)
	}
	for i := range items {
		if d, ok := byVenue[items[i].Restaurant.ID]; ok {
			dish := d
			items[i].MatchedDish = &dish
		}
	}
	return nil
}

func (r *Repository) args(m *domain.Restaurant) []any {
	return []any{
		m.ID, m.CategoryID, m.Name, i18nToDB(m.NameI18n), m.Description,
		i18nToDB(m.DescriptionI18n), m.CuisineType, i18nToDB(m.CuisineTypeI18n),
		m.Address, i18nToDB(m.AddressI18n), m.OpeningHours, i18nToDB(m.OpeningHoursI18n),
		string(m.City), string(m.PriceCategory), m.Email, m.Phone, m.Latitude, m.Longitude,
		m.KwaakaRestaurantID, m.IsActive, m.IsNew, m.IsPopular, m.IsPremium,
		m.HiddenFromHome, m.DisplayOrder, m.CreatedAt, m.UpdatedAt, m.PriceMin, m.PriceMax,
	}
}

type scanner interface{ Scan(dest ...any) error }

func scanRestaurant(row scanner) (*domain.Restaurant, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
		&m.PriceMin, &m.PriceMax,
	); err != nil {
		return nil, err
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, nil
}

// scanRestaurantWithPolicy scans the base columns plus the booking-policy
// overrides. Without it every venue would silently fall back to the env
// defaults and restaurant-level auto_confirm / SLA settings would be ignored.
func scanRestaurantWithPolicy(row scanner) (*domain.Restaurant, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	// Scanned as *string, not as *domain.CapacityMode: pgx has no reason to
	// know about a domain type, and a legacy/unknown label must reach
	// resolvePolicy (which ignores it) rather than fail the scan.
	var capacityMode *string
	p := &m.BookingPolicy
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
		&m.PriceMin, &m.PriceMax,
		&p.Timezone, &p.BookingDurationMinutes, &p.BookingBufferMinutes,
		&p.BookingLeadMinutes, &p.BookingHorizonDays, &p.CancelDeadlineMinutes,
		&p.ConfirmSLAMinutes, &p.MaxGuestsPerBooking, &p.AutoConfirm, &p.ConfirmOnCreate,
		&capacityMode, &p.BookingCapacitySeats,
	); err != nil {
		return nil, err
	}
	if capacityMode != nil {
		mode := domain.CapacityMode(*capacityMode)
		p.BookingCapacityMode = &mode
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, nil
}

// Columns is the ordered restaurant column list (same as the unexported cols
// used by this file's own statements), exported so a sibling infrastructure
// package can join through its own table and still scan a full Restaurant row
// without duplicating this list — see
// internal/infrastructure/postgres/favorite.Repository.ListByUser.
const Columns = cols

// ListExtraColumns are the trailing listing-only columns (primary_image, then
// the venue timezone) that must follow Columns in any SELECT fed to
// ScanListItem. Exported together with Columns so a sibling package cannot
// spell one and forget the other — that mismatch is a runtime scan error, not
// a compile error.
const ListExtraColumns = listExtraCols

// ScanListItem scans one row shaped like ListActive's SELECT (Columns followed
// by ListExtraColumns) into a Restaurant plus its primary image URL. Exported
// for the same reason as Columns.
func ScanListItem(row scanner) (*domain.Restaurant, *string, error) {
	return scanListItem(row)
}

func scanListItem(row scanner) (*domain.Restaurant, *string, error) {
	var m domain.Restaurant
	var city, price string
	var name, desc, cuisine, addr, opening []byte
	var primary *string
	// Scanned as a plain *string for the same reason GetByID does it: pgx knows
	// nothing about domain.CapacityMode, and an unknown/legacy label must reach
	// resolvePolicy (which ignores it, leaving the venue in table mode) rather
	// than fail the whole catalog page.
	var capacityMode *string
	if err := row.Scan(
		&m.ID, &m.CategoryID, &m.Name, &name, &m.Description, &desc,
		&m.CuisineType, &cuisine, &m.Address, &addr, &m.OpeningHours, &opening,
		&city, &price, &m.Email, &m.Phone, &m.Latitude, &m.Longitude,
		&m.KwaakaRestaurantID, &m.IsActive, &m.IsNew, &m.IsPopular, &m.IsPremium,
		&m.HiddenFromHome, &m.DisplayOrder, &m.CreatedAt, &m.UpdatedAt,
		&m.PriceMin, &m.PriceMax, &primary,
		&m.BookingPolicy.Timezone,
		&capacityMode, &m.BookingPolicy.BookingCapacitySeats,
	); err != nil {
		return nil, nil, err
	}
	if capacityMode != nil {
		mode := domain.CapacityMode(*capacityMode)
		m.BookingPolicy.BookingCapacityMode = &mode
	}
	m.City = domain.City(city)
	m.PriceCategory = domain.PriceCategory(price)
	m.NameI18n = i18nFromDB(name)
	m.DescriptionI18n = i18nFromDB(desc)
	m.CuisineTypeI18n = i18nFromDB(cuisine)
	m.AddressI18n = i18nFromDB(addr)
	m.OpeningHoursI18n = i18nFromDB(opening)
	return &m, primary, nil
}

// limitOffset resolves the LIMIT/OFFSET pair for a catalog read.
//
// Normal reads take one page, with the shared defaults (domain.NormalizePaging)
// rather than a copy of "20 / cap 100" that can drift from the one the usecase
// and transport layers apply.
//
// An unpaginated read asks for the WHOLE matching set: the caller has a filter
// it can only evaluate in Go over every matching row (see
// domain.VenueStateFilter). It is still bounded — domain.CatalogScanLimit is a
// safety ceiling, not a page size, and the caller compares the number of rows it
// got against the count to notice if it ever bites.
func limitOffset(page, perPage int, unpaginated bool) (int, int) {
	if unpaginated {
		return domain.CatalogScanLimit, 0
	}
	page, perPage = domain.NormalizePaging(page, perPage)
	return perPage, (page - 1) * perPage
}

// escapeLike escapes the LIKE/ILIKE metacharacters (backslash first) so a
// user-supplied term is matched literally under `ESCAPE '\'`.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListManageableBrief returns a lightweight (id, name, name_i18n) row for EVERY
// restaurant, ordered by name. It backs the superadmin variant of
// GET /admin/my-restaurants: a superadmin manages the whole platform, so this
// deliberately includes inactive and hidden-from-home venues (unlike the public
// catalog reads). Returns an empty slice, not an error, when there are none.
func (r *Repository) ListManageableBrief(ctx context.Context) ([]domain.RestaurantBrief, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, name, name_i18n FROM restaurants ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list restaurants brief: %w", err)
	}
	defer rows.Close()
	var out []domain.RestaurantBrief
	for rows.Next() {
		var b domain.RestaurantBrief
		var nameI18n []byte
		if err := rows.Scan(&b.ID, &b.Name, &nameI18n); err != nil {
			return nil, err
		}
		b.NameI18n = i18nFromDB(nameI18n)
		out = append(out, b)
	}
	return out, rows.Err()
}

// prefixed rewrites a bare column list into a table-qualified one.
func prefixed(colList, alias string) string {
	parts := strings.Split(colList, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
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

// mapWrite maps a unique_violation to domain.ErrAlreadyExists, otherwise wraps
// err with resource for context. resource should name the entity/operation
// being written (e.g. "create restaurant", "create manager").
func mapWrite(err error, resource string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, resource)
	}
	return fmt.Errorf("%s: %w", resource, err)
}

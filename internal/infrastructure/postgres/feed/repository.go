// Package feed is the Postgres implementation of domain.FeedRepository — the
// merchandising feed's read model over promos + events (migration 0050) plus
// the platform's moderation writes.
//
// The read model is deliberately a UNION ALL of two nearly identical SELECTs
// rather than a view or a third table: promos and events stay independent
// entities, and the union is the ONE place their shapes are aligned. Both
// branches must therefore project the SAME columns in the SAME order — see
// itemColumns; adding a column means adding it to both.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.FeedRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the feed repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.FeedRepository = (*Repository)(nil)

// itemSelect builds one branch of the union. table is 'promos' or 'events' —
// NEVER caller input: it comes from tableFor, a closed switch over the two
// known kinds, so no string here can carry an injection.
//
// The venue is LEFT-joined since migration 0085: a PLATFORM promo or event has
// no venue at all, and an inner join would have silently deleted the whole
// feature from the main screen. Everything the ranking reads off the venue is
// therefore COALESCEd to a neutral value — is_active true, hidden_from_home
// false, so the eligibility rule that exists to hide a DEACTIVATED venue does
// not also hide a card that never had one. The card's city becomes
// COALESCE(i.city, r.city): the item's own override first, its venue second,
// NULL (= every city) only when it has neither.
//
// The two branches now differ only in `terms` and `discount_percent`, which
// events do not have: terms is projected as an empty string and discount as
// NULL so the column list stays identical. Both tables carry cover_image_url
// (promos since migration 0060), so the card picture is read the same way for
// both kinds; discount is promo-only (migration 0066).
//
// extra appends columns AFTER the shared list. It exists for exactly one
// caller — the recurrence collapse in ListCandidates, which needs
// recurrence_id and a row_number inside a derived table and then projects the
// shared list back out (see collapsedEventCols). Anything passed here is a
// literal from this file, never caller input.
func itemSelect(kind domain.FeedItemKind, extra ...string) string {
	table, alias := tableFor(kind), "i"
	cover, terms, discount := alias+".cover_image_url", alias+".terms", alias+".discount_percent"
	// Галерея (migration 0070) живёт в дочерней таблице, своей у каждого вида.
	// Читаем её тем же запросом коррелированным подзапросом: карточка ленты
	// рисует ленту фотографий, и добор по одному запросу на карточку вернул бы
	// N+1 в самый горячий экран приложения. Порядок — редакторский position,
	// created_at как устойчивый тай-брейк, как и в остальных чтениях галерей.
	images := `(SELECT COALESCE(array_agg(g.image_url ORDER BY g.position, g.created_at), '{}'::varchar[])
			FROM event_images g WHERE g.event_id = ` + alias + `.id)`
	if kind == domain.FeedItemPromo {
		images = `(SELECT COALESCE(array_agg(g.image_url ORDER BY g.position, g.created_at), '{}'::varchar[])
			FROM promo_images g WHERE g.promo_id = ` + alias + `.id)`
	}
	if kind == domain.FeedItemEvent {
		terms = "''::text"
		discount = "NULL::int"
	}
	tail := ""
	if len(extra) > 0 {
		tail = ",\n\t\t" + strings.Join(extra, ",\n\t\t")
	}
	return `SELECT '` + string(kind) + `'::text AS kind,
		` + alias + `.id, ` + alias + `.restaurant_id,
		-- COALESCE, because the venue is LEFT-joined: a platform card has no
		-- venue row, and a NULL here does not scan into a Go string — it fails
		-- the WHOLE main-screen query, not just its own row.
		COALESCE(r.name, '') AS restaurant_name, r.name_i18n AS restaurant_name_i18n,
		COALESCE(` + alias + `.city, r.city) AS city,
		r.category_id,
		COALESCE(r.is_active, true) AS venue_is_active,
		COALESCE(r.hidden_from_home, false) AS venue_hidden_from_home,
		` + alias + `.title, ` + alias + `.title_i18n, ` + alias + `.description, ` + alias + `.description_i18n,
		` + alias + `.starts_at, ` + alias + `.ends_at,
		` + cover + ` AS cover_image_url, ` + images + ` AS images,
		` + terms + ` AS terms, ` + discount + ` AS discount_percent,
		` + alias + `.status AS item_status,
		` + alias + `.feed_status, ` + alias + `.feed_submitted_at, ` + alias + `.feed_reviewed_by,
		` + alias + `.feed_reviewed_at, ` + alias + `.feed_rejection_reason, ` + alias + `.feed_placement_weight,
		` + alias + `.created_at` + tail + `
	FROM ` + table + ` ` + alias + `
	LEFT JOIN restaurants r ON r.id = ` + alias + `.restaurant_id`
}

// collapsedEventCols reads the shared column list back out of the derived table
// the recurrence collapse builds: same columns, same ORDER as itemSelect
// projects them, because scanCandidate depends on that order. The two helper
// columns of the derived table (recurrence_id, rn) are deliberately absent —
// they are a filtering device and must never reach the scanner.
const collapsedEventCols = `ev.kind, ev.id, ev.restaurant_id,
	ev.restaurant_name, ev.restaurant_name_i18n,
	ev.city, ev.category_id, ev.venue_is_active, ev.venue_hidden_from_home,
	ev.title, ev.title_i18n, ev.description, ev.description_i18n,
	ev.starts_at, ev.ends_at,
	ev.cover_image_url, ev.images,
	ev.terms, ev.discount_percent,
	ev.item_status,
	ev.feed_status, ev.feed_submitted_at, ev.feed_reviewed_by,
	ev.feed_reviewed_at, ev.feed_rejection_reason, ev.feed_placement_weight,
	ev.created_at`

// feedVisibleSQL is domain.FeedEligible's venue-and-city half, in SQL. It is
// written ONCE and pasted into both union branches, because the one thing worse
// than this rule being in two languages is it being in two languages twice.
//
// $1 is the requested city. Read it as: the card belongs to this city (its own
// override, or its venue's) or to no city at all, AND — only if it has a venue —
// that venue is visible.
const feedVisibleSQL = `(COALESCE(i.city, r.city) IS NULL OR COALESCE(i.city, r.city) = $1::varchar)
		  AND (i.restaurant_id IS NULL OR (r.is_active AND NOT r.hidden_from_home))`

// tableFor maps a kind to its table. The default panics rather than returning
// an empty name: an unknown kind here is a programming error, and silently
// querying nothing would hide it.
func tableFor(kind domain.FeedItemKind) string {
	switch kind {
	case domain.FeedItemPromo:
		return "promos"
	case domain.FeedItemEvent:
		return "events"
	default:
		panic("feed: unknown item kind " + string(kind))
	}
}

// ListCandidates is the guest main-screen read: ONE query, no per-card
// follow-up. It enforces domain.FeedEligible in SQL (published AND approved AND
// inside the window AND at an active, not-hidden-from-home venue in the
// requested city) and joins in, per candidate, the venue's published-review
// aggregate and whether the venue's cuisine is one the guest chose.
//
// A recurring series is collapsed to its nearest upcoming occurrence inside
// the event branch, before the union — see the comment on eventBranch. The
// usecase's total is len(candidates) after ranking, so collapsing here is also
// what keeps the reported count and the pagination honest: the number the
// client is told matches the set it can actually page through.
//
// $1 city, $2 the signed-in guest (NULL when anonymous — the prefs CTE is then
// empty and every item scores a neutral 0 for the preference signal), $3 now,
// $4 the candidate cap. Each parameter carries an explicit cast on first use:
// a bound parameter reused in several positions can otherwise be deduced into
// two different types under the extended protocol (SQLSTATE 42P08).
func (r *Repository) ListCandidates(ctx context.Context, q domain.FeedQuery) ([]domain.FeedItem, error) {
	limit := q.Limit
	if limit < 1 {
		limit = 100
	}
	// Promos must have STARTED; an upcoming event is promoted before it starts.
	// The city, the venue flags and the "no venue at all" case are all folded
	// into feedVisibleSQL so the two union branches state the rule once. A NULL
	// effective city passes for EVERY city — that is the platform card meant to
	// run everywhere, and no venue-bound row can reach it (restaurants.city is
	// NOT NULL).
	promoBranch := itemSelect(domain.FeedItemPromo) + `
		WHERE ` + feedVisibleSQL + `
		  AND i.status = 'published' AND i.feed_status = 'approved'
		  AND i.starts_at <= $3::timestamptz AND i.ends_at > $3`
	// A recurring series contributes exactly ONE card — its nearest upcoming
	// occurrence — and it does so BEFORE the union, so the surviving card
	// competes with promos on the very same ordering and ranking rules as any
	// one-off item. Without this a daily rule buries the whole main screen
	// under one venue (the «Живая музыка в ресторане INZHU» incident, 98
	// cards), which is the same reason the public Афиша collapses in
	// event.ListPublicUpcoming — this is that rule, applied to the feed.
	//
	// row_number() runs over the ALREADY-FILTERED set, so "nearest" means
	// nearest among what the guest may actually see: an occurrence that has
	// ended, was never published or was not approved for the feed is not a
	// candidate for the one surviving card, and when today's date passes,
	// tomorrow's automatically takes its place.
	//
	// The explicit `recurrence_id IS NULL` branch is not optional: every
	// one-off event shares the NULL partition, so filtering on rn = 1 alone
	// would drop all of them but one.
	eventBranch := `SELECT ` + collapsedEventCols + ` FROM (
			` + itemSelect(domain.FeedItemEvent,
		"i.recurrence_id",
		"row_number() OVER (PARTITION BY i.recurrence_id ORDER BY i.starts_at ASC, i.id ASC) AS rn") + `
			WHERE ` + feedVisibleSQL + `
			  AND i.status = 'published' AND i.feed_status = 'approved'
			  AND i.ends_at > $3
		) ev
		WHERE ev.recurrence_id IS NULL OR ev.rn = 1`

	// prefs: the guest's picked cuisines. Until migration 0079 this read
	// user_cuisine_preferences.category_id and compared it to
	// restaurants.category_id — the VENUE TYPE dictionary, which was empty and
	// which no restaurant referenced. matches_pref was therefore false for
	// every card ever served, and the 400-point cuisine signal below has never
	// once fired in production. It compares dictionary cuisines now.
	sql := `WITH prefs AS (
			SELECT cuisine_id FROM user_cuisine_preferences WHERE user_id = $2::uuid
		),
		candidates AS (
			` + promoBranch + `
			UNION ALL
			` + eventBranch + `
		)
		SELECT c.*,
			COALESCE(rt.avg_rating, 0)::float8 AS venue_rating,
			COALESCE(rt.review_count, 0)::int  AS venue_review_count,
			EXISTS (SELECT 1 FROM restaurant_cuisines rc
			         JOIN prefs p ON p.cuisine_id = rc.cuisine_id
			        WHERE rc.restaurant_id = c.restaurant_id) AS matches_pref,
			EXISTS (SELECT 1 FROM prefs) AS has_prefs
		FROM candidates c
		-- LATERAL, not a platform-wide GROUP BY: the aggregate is computed for
		-- the candidate venues only, using idx_reviews_published_listing.
		LEFT JOIN LATERAL (
			SELECT avg(rating)::float8 AS avg_rating, count(*)::int AS review_count
			FROM reviews rv
			WHERE rv.restaurant_id = c.restaurant_id AND rv.status = 'published'
		) rt ON true
		-- A deterministic pre-order so the LIMIT truncation itself is
		-- reproducible; the domain ranking re-orders exactly these rows.
		ORDER BY c.feed_placement_weight DESC, c.ends_at ASC, c.id ASC
		LIMIT $4`

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, sql, string(q.City), q.UserID, q.Now, limit)
	if err != nil {
		return nil, fmt.Errorf("list feed candidates: %w", err)
	}
	defer rows.Close()

	var items []domain.FeedItem
	for rows.Next() {
		it, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan feed candidate: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feed candidates: %w", err)
	}
	return items, nil
}

// GetItem returns one item with its placement regardless of any status, so the
// usecase can resolve the owning restaurant before authorizing.
func (r *Repository) GetItem(ctx context.Context, kind domain.FeedItemKind, id uuid.UUID) (*domain.FeedItem, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, itemSelect(kind)+` WHERE i.id = $1`, id)
	it, err := scanItem(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get feed item: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get feed item: %w", err)
	}
	return it, nil
}

// ListByRestaurant is the venue's own "where do my submissions stand" view:
// every promo and event it owns, newest supplied first, with (kind, id) as the
// stable tie-break so a page boundary never swallows an item.
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.FeedItem, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM promos WHERE restaurant_id = $1::uuid)
			  + (SELECT count(*) FROM events WHERE restaurant_id = $1)`,
		restaurantID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count restaurant feed items: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	sql := itemSelect(domain.FeedItemPromo) + ` WHERE i.restaurant_id = $1::uuid
		UNION ALL ` + itemSelect(domain.FeedItemEvent) + ` WHERE i.restaurant_id = $1
		ORDER BY created_at DESC, kind ASC, id ASC
		LIMIT $2 OFFSET $3`
	rows, err := q.Query(ctx, sql, restaurantID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list restaurant feed items: %w", err)
	}
	items, err := collect(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// ListByFeedStatus is the platform moderation queue: oldest submission first
// (FIFO), with (kind, id) as the stable tie-break. Rows that never carried a
// submitted_at sort last (NULLS LAST) instead of jumping the queue.
func (r *Repository) ListByFeedStatus(ctx context.Context, status domain.FeedStatus, page, perPage int) ([]domain.FeedItem, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM promos WHERE feed_status = $1::varchar)
			  + (SELECT count(*) FROM events WHERE feed_status = $1)`,
		string(status)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count feed queue: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	sql := itemSelect(domain.FeedItemPromo) + ` WHERE i.feed_status = $1::varchar
		UNION ALL ` + itemSelect(domain.FeedItemEvent) + ` WHERE i.feed_status = $1
		ORDER BY feed_submitted_at ASC NULLS LAST, kind ASC, id ASC
		LIMIT $2 OFFSET $3`
	rows, err := q.Query(ctx, sql, string(status), perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list feed queue: %w", err)
	}
	items, err := collect(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// TransitionFeedStatus is the CAS the whole moderation flow rests on: the
// WHERE clause carries the expected current status, so two concurrent decisions
// cannot both apply — the loser sees ErrInvalidStatus rather than overwriting
// the winner. A zero-row UPDATE is disambiguated by a follow-up existence check
// (absent id vs wrong status).
func (r *Repository) TransitionFeedStatus(ctx context.Context, kind domain.FeedItemKind, id uuid.UUID, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) error {
	if len(from) == 0 {
		return fmt.Errorf("%w: a feed transition needs an expected current status", domain.ErrValidation)
	}
	table := tableFor(kind)
	q := sqltx.From(ctx, r.pool)

	tag, err := q.Exec(ctx,
		`UPDATE `+table+` SET
			feed_status = $2::varchar,
			feed_submitted_at = $3,
			feed_reviewed_by = $4,
			feed_reviewed_at = $5,
			feed_rejection_reason = $6,
			-- NULL means "leave the paid placement alone": a moderation
			-- decision must never silently reset a sold weight.
			feed_placement_weight = COALESCE($7::int, feed_placement_weight),
			updated_at = now()
		 WHERE id = $1 AND feed_status = ANY($8::text[])`,
		id, string(upd.Status), upd.SubmittedAt, upd.ReviewedBy, upd.ReviewedAt,
		upd.RejectionReason, upd.PlacementWeight, statusStrings(from))
	if err != nil {
		return fmt.Errorf("transition feed status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.explainNoRows(ctx, table, id, "transition feed status")
	}
	return nil
}

// SetPlacementWeight moves the commercial dial without touching moderation.
func (r *Repository) SetPlacementWeight(ctx context.Context, kind domain.FeedItemKind, id uuid.UUID, weight int) error {
	table := tableFor(kind)
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE `+table+` SET feed_placement_weight = $2, updated_at = now() WHERE id = $1`, id, weight)
	if err != nil {
		return fmt.Errorf("set placement weight: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set placement weight: %w", domain.ErrNotFound)
	}
	return nil
}

// ApprovePlatformItem stamps the platform's own approval on the platform's own
// item, in ONE guarded UPDATE. The guard is the point:
//
//	restaurant_id IS NULL     -- only PLATFORM content, never a venue's
//	feed_status = 'not_submitted' -- only a fresh item, never a decided one
//
// Both conditions live in the WHERE clause rather than in the caller, so no
// present or future usecase can turn this into a way to approve venue content
// without a moderator, and a duplicated call (a retried create) cannot re-stamp
// a reviewer over an existing decision — it gets ErrInvalidStatus instead.
//
// feed_submitted_at is written together with the decision: the platform
// submitted and approved in the same act, and leaving it NULL would produce an
// approved row that was never submitted — an audit trail that reads like a bug.
func (r *Repository) ApprovePlatformItem(ctx context.Context, kind domain.FeedItemKind, id, reviewerID uuid.UUID, at time.Time) error {
	table := tableFor(kind)
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE `+table+` SET
			feed_status = $2::varchar,
			feed_submitted_at = $3::timestamptz,
			feed_reviewed_by = $4::uuid,
			feed_reviewed_at = $3,
			feed_rejection_reason = NULL,
			updated_at = now()
		 WHERE id = $1
		   AND restaurant_id IS NULL
		   AND feed_status = $5::varchar`,
		id, string(domain.FeedApproved), at, reviewerID, string(domain.FeedNotSubmitted))
	if err != nil {
		return fmt.Errorf("approve platform feed item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.explainNoRows(ctx, table, id, "approve platform feed item")
	}
	return nil
}

// DemoteAfterContentEdit mirrors domain.FeedStatusAfterContentEdit: a decision
// made about specific words stops being valid when the words change. It is a
// no-op for an item nobody decided on, and it never touches the placement
// weight — editing a promo is not how a venue loses or gains a paid slot.
//
// A missing id is NOT an error here: the caller invokes this right before
// editing an item it already resolved, and turning a benign race into a 404
// would only mask the real edit error.
//
// `restaurant_id IS NOT NULL` is domain.FeedDemotableAfterContentEdit in SQL:
// PLATFORM content is never demoted, because the editor IS the reviewer and a
// demotion would hide the platform's own card behind a review nobody can
// perform. The exemption sits in the write, not in the two usecases that call
// this, so it cannot be half-applied.
func (r *Repository) DemoteAfterContentEdit(ctx context.Context, kind domain.FeedItemKind, id uuid.UUID) error {
	table := tableFor(kind)
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE `+table+` SET
			feed_status = $2::varchar,
			feed_reviewed_by = NULL,
			feed_reviewed_at = NULL,
			feed_rejection_reason = NULL,
			updated_at = now()
		 WHERE id = $1 AND restaurant_id IS NOT NULL AND feed_status = ANY($3::text[])`,
		id, string(domain.FeedPendingReview),
		[]string{string(domain.FeedApproved), string(domain.FeedRejected)})
	if err != nil {
		return fmt.Errorf("demote feed placement after edit: %w", err)
	}
	return nil
}

// --- helpers ---

// explainNoRows turns a zero-row CAS into the right sentinel: ErrNotFound when
// the row is gone, ErrInvalidStatus when it is simply in another state.
func (r *Repository) explainNoRows(ctx context.Context, table string, id uuid.UUID, op string) error {
	var exists bool
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return fmt.Errorf("%s: %w: the item is not in the expected feed status", op, domain.ErrInvalidStatus)
}

func collect(rows pgx.Rows) ([]domain.FeedItem, error) {
	defer rows.Close()
	var items []domain.FeedItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan feed item: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feed items: %w", err)
	}
	return items, nil
}

// scanItem reads the shared column list produced by itemSelect. Both union
// branches project it identically, so one scanner serves every query here.
func scanItem(row pgx.Row) (*domain.FeedItem, error) {
	var it domain.FeedItem
	var nameI18n, titleI18n, descI18n []byte
	if err := row.Scan(
		&it.Kind, &it.ID, &it.RestaurantID,
		&it.RestaurantName, &nameI18n,
		&it.City, &it.CategoryID, &it.VenueIsActive, &it.VenueHiddenFromHome,
		&it.Title, &titleI18n, &it.Description, &descI18n,
		&it.StartsAt, &it.EndsAt,
		&it.CoverImageURL, &it.Images, &it.Terms, &it.DiscountPercent,
		&it.ItemStatus,
		&it.Placement.Status, &it.Placement.SubmittedAt, &it.Placement.ReviewedBy,
		&it.Placement.ReviewedAt, &it.Placement.RejectionReason, &it.Placement.PlacementWeight,
		&it.CreatedAt,
	); err != nil {
		return nil, err
	}
	it.RestaurantNameI18n = i18nFromDB(nameI18n)
	it.TitleI18n = i18nFromDB(titleI18n)
	it.DescriptionI18n = i18nFromDB(descI18n)
	return &it, nil
}

// scanCandidate reads the shared columns plus the four ranking extras the main
// feed query appends.
func scanCandidate(row pgx.Row) (*domain.FeedItem, error) {
	var it domain.FeedItem
	var nameI18n, titleI18n, descI18n []byte
	if err := row.Scan(
		&it.Kind, &it.ID, &it.RestaurantID,
		&it.RestaurantName, &nameI18n,
		&it.City, &it.CategoryID, &it.VenueIsActive, &it.VenueHiddenFromHome,
		&it.Title, &titleI18n, &it.Description, &descI18n,
		&it.StartsAt, &it.EndsAt,
		&it.CoverImageURL, &it.Images, &it.Terms, &it.DiscountPercent,
		&it.ItemStatus,
		&it.Placement.Status, &it.Placement.SubmittedAt, &it.Placement.ReviewedBy,
		&it.Placement.ReviewedAt, &it.Placement.RejectionReason, &it.Placement.PlacementWeight,
		&it.CreatedAt,
		&it.RestaurantRating, &it.RestaurantReviewCount,
		&it.MatchesCuisinePreference, &it.HasCuisinePreferences,
	); err != nil {
		return nil, err
	}
	it.RestaurantNameI18n = i18nFromDB(nameI18n)
	it.TitleI18n = i18nFromDB(titleI18n)
	it.DescriptionI18n = i18nFromDB(descI18n)
	return &it, nil
}

func statusStrings(statuses []domain.FeedStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, string(s))
	}
	return out
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

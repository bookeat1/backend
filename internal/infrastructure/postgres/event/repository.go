// Package event is the Postgres implementation of domain.EventRepository.
package event

import (
	"context"
	"encoding/json"
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

const foreignKeyViolation = "23503"

// Repository implements domain.EventRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the event repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.EventRepository = (*Repository)(nil)

const selectCols = `id, restaurant_id, title, title_i18n, description, description_i18n,
	starts_at, ends_at, venue, cover_image_url, status, ticketed,
	ticket_price_minor, capacity, tags, tickets_refundable, ticket_refund_cutoff_minutes,
	recurrence_id, created_at, updated_at, city, action_label, action_url, content_overrides`

// Create inserts a new event. An unknown restaurant_id (FK violation) maps to
// ErrNotFound, same convention as reviews/favorites.
func (r *Repository) Create(ctx context.Context, e *domain.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO events (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, venue, cover_image_url, status, ticketed, ticket_price_minor, capacity,
			tags, tickets_refundable, ticket_refund_cutoff_minutes, city, action_label, action_url)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		 RETURNING created_at, updated_at, city`,
		e.ID, e.RestaurantID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity,
		tagsToDB(e.Tags), e.TicketsRefundable, e.TicketRefundCutoffMinutes, e.City,
		actionLabelToDB(e.Action), actionURLToDB(e.Action)).
		Scan(&e.CreatedAt, &e.UpdatedAt, &e.City)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create event: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// GetByID returns an event by its id regardless of status.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Event, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selectCols+` FROM events WHERE id = $1`, id)
	return scanEvent(row, "get event")
}

// Update overwrites the mutable fields of an existing event, content_overrides
// among them: a date's content and the record of which fields it now owns are
// one fact and are written in one statement, so they can never drift apart. A
// zero-rows UPDATE means the id is absent.
func (r *Repository) Update(ctx context.Context, e *domain.Event) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE events SET title = $2, title_i18n = $3, description = $4, description_i18n = $5,
			starts_at = $6, ends_at = $7, venue = $8, cover_image_url = $9, status = $10,
			ticketed = $11, ticket_price_minor = $12, capacity = $13, tags = $14,
			tickets_refundable = $15, ticket_refund_cutoff_minutes = $16, city = $17,
			action_label = $18, action_url = $19, content_overrides = $20, updated_at = now()
		 WHERE id = $1`,
		e.ID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity,
		tagsToDB(e.Tags), e.TicketsRefundable, e.TicketRefundCutoffMinutes, e.City,
		actionLabelToDB(e.Action), actionURLToDB(e.Action), overridesToDB(e.ContentOverrides))
	if err != nil {
		return fmt.Errorf("update event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update event: %w", domain.ErrNotFound)
	}
	return nil
}

// Delete removes an event. A zero-rows DELETE means the id is absent.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, `DELETE FROM events WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete event: %w", domain.ErrNotFound)
	}
	return nil
}

// ListByRestaurant returns a restaurant's events for the admin cabinet,
// optionally status-filtered, newest start first with id as a stable
// tie-breaker. statuses is passed as a text[] and matched with = ANY when
// non-empty (an empty array means "all statuses").
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)
	statusArg := statusStrings(statuses)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM events
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))`,
		restaurantID, statusArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM events
		 WHERE restaurant_id = $1
		   AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))
		 ORDER BY starts_at DESC, id DESC
		 LIMIT $3 OFFSET $4`,
		restaurantID, statusArg, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list events: %w", err)
	}
	return collect(rows, total)
}

// ListPlatform returns the PLATFORM's own events — the ones with no host venue
// (restaurant_id IS NULL) — for the platform cabinet. Same ordering, status
// filter and pagination contract as ListByRestaurant; `IS NULL` rather than a
// parameter, because a bound uuid can never mean "no venue" and passing
// uuid.Nil would silently match nothing.
func (r *Repository) ListPlatform(ctx context.Context, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)
	statusArg := statusStrings(statuses)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM events
		 WHERE restaurant_id IS NULL
		   AND (cardinality($1::text[]) = 0 OR status = ANY($1::text[]))`,
		statusArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count platform events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM events
		 WHERE restaurant_id IS NULL
		   AND (cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
		 ORDER BY starts_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		statusArg, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list platform events: %w", err)
	}
	return collect(rows, total)
}

// ListPublishedUpcoming returns a restaurant's published, not-yet-ended events,
// soonest first with id as a stable tie-breaker. Matches idx_events_published_upcoming.
func (r *Repository) ListPublishedUpcoming(ctx context.Context, restaurantID uuid.UUID, now time.Time, page, perPage int) ([]domain.Event, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM events
		 WHERE restaurant_id = $1 AND status = 'published' AND ends_at > $2`,
		restaurantID, now).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count published events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM events
		 WHERE restaurant_id = $1 AND status = 'published' AND ends_at > $2
		 ORDER BY starts_at ASC, id ASC
		 LIMIT $3 OFFSET $4`,
		restaurantID, now, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list published events: %w", err)
	}
	return collect(rows, total)
}

// listCols is selectCols qualified with the `e` alias, plus the host venue's
// identity, for the cross-venue listing's join. It must stay in the same order
// as selectCols — scanListItemRow reuses that order.
const listCols = `e.id, e.restaurant_id, e.title, e.title_i18n, e.description, e.description_i18n,
	e.starts_at, e.ends_at, e.venue, e.cover_image_url, e.status, e.ticketed,
	e.ticket_price_minor, e.capacity, e.tags, e.tickets_refundable, e.ticket_refund_cutoff_minutes,
	e.recurrence_id, e.created_at, e.updated_at, e.city AS event_city,
	e.action_label, e.action_url, e.content_overrides,
	r.name, r.name_i18n, r.city`

// collapsedCols reads listCols back out of the derived table the collapse
// builds (the `e.`/`r.` prefixes are gone there, and `rn` must not reach the
// scanner). Same columns, same order — scanListItemRow depends on it.
//
// The event's own city arrives as `event_city`, not `city`: inside the derived
// table it would otherwise collide with the venue's `city` and every reference
// to it would be ambiguous. That is why listCols aliases it at the source.
const collapsedCols = `c.id, c.restaurant_id, c.title, c.title_i18n, c.description, c.description_i18n,
	c.starts_at, c.ends_at, c.venue, c.cover_image_url, c.status, c.ticketed,
	c.ticket_price_minor, c.capacity, c.tags, c.tickets_refundable, c.ticket_refund_cutoff_minutes,
	c.recurrence_id, c.created_at, c.updated_at, c.event_city,
	c.action_label, c.action_url, c.content_overrides,
	c.name, c.name_i18n, c.city`

// ListPublicUpcoming implements the cross-venue public listing. Visibility is
// enforced in SQL and is not negotiable by a filter: published, not yet ended,
// and hosted by an active restaurant — a deactivated venue disappears from the
// guest app together with its events, the same rule the catalog listing keeps
// (restaurants.ListActive). hidden_from_home is deliberately NOT applied: it
// hides a venue from the main screen only, not from the catalog, and this is a
// catalog-style listing.
//
// ONE CARD PER SERIES. A recurring event appears exactly once — as its nearest
// upcoming occurrence — while one-off events are listed as they always were.
// Without this a single daily rule ("Живая музыка в ресторане INZHU") filled
// the Афиша with 55 identical cards in a row and buried every other venue.
//
// This is a PRESENTATION rule of the guest catalog and nothing more: the detail
// endpoint, tickets and bookings still work per individual occurrence (a guest
// books a specific date), and the cabinet listing (ListByRestaurant) still
// shows every generated date, because that is where a venue edits or cancels
// one of them. Grouping by recurrence_id and not by title is deliberate — two
// venues may both run "Живая музыка", and a rule is the only thing that
// actually says "these rows are the same happening".
func (r *Repository) ListPublicUpcoming(ctx context.Context, f domain.PublicEventFilter, now time.Time) ([]domain.EventListItem, int, error) {
	// A PLATFORM event (e.restaurant_id IS NULL) has no venue whose is_active
	// flag could hide it, and the unconditional `r.is_active = true` this
	// predicate used to carry would have hidden every one of them — with a LEFT
	// JOIN r.is_active is NULL there, and NULL = true is not true. COALESCE
	// states the intent: no venue means nothing to deactivate.
	where := []string{"e.status = 'published'", "COALESCE(r.is_active, true) = true"}
	args := []any{now}
	where = append(where, "e.ends_at > $1")
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		// The EFFECTIVE city of an event: its own override when it has one,
		// otherwise the host venue's. Same predicate shape as the gastroguide
		// collections (migration 0061): a row with no effective city at all is
		// shown for EVERY city rather than for none.
		//
		// Since migration 0085 the venue is LEFT-joined, so the COALESCE is
		// NULL exactly for a platform event with no override — the "shown in
		// every city" branch this predicate was written for.
		add("(COALESCE(e.city, r.city) IS NULL OR COALESCE(e.city, r.city) = $%d)", string(*f.City))
	}
	if f.RestaurantID != nil {
		add("e.restaurant_id = $%d", *f.RestaurantID)
	}
	if f.From != nil {
		add("e.starts_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("e.starts_at <= $%d", *f.To)
	}
	whereSQL := strings.Join(where, " AND ")
	from := ` FROM events e LEFT JOIN restaurants r ON r.id = e.restaurant_id WHERE ` + whereSQL

	// The collapse rule (see the doc comment): inside the visible set, a
	// recurring series contributes exactly ONE row — its nearest upcoming
	// occurrence. row_number() over the same ordering the listing uses picks it;
	// one-off events all share recurrence_id IS NULL and would land in a single
	// meaningless partition, so they are let through by the explicit
	// `recurrence_id IS NULL` branch instead of by their row number.
	//
	// The window function runs over the already-filtered set, so "nearest"
	// means nearest AMONG WHAT THE GUEST ASKED FOR: a from/to filter picks the
	// first occurrence inside that range, not the first one overall.
	collapsed := func(cols string) string {
		return `SELECT ` + cols + ` FROM (
			SELECT ` + listCols + `, row_number() OVER (
				PARTITION BY e.recurrence_id ORDER BY e.starts_at ASC, e.id ASC) AS rn` +
			from + `
		) c WHERE c.recurrence_id IS NULL OR c.rn = 1`
	}

	q := sqltx.From(ctx, r.pool)
	var total int
	// The total is counted over the COLLAPSED set, not the raw one: a list that
	// shows 3 cards must not claim 57, or the client pages into emptiness.
	if err := q.QueryRow(ctx, `SELECT count(*) FROM (`+collapsed("c.id")+`) t`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count public events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	page, perPage := normalizePage(f.Page, f.PerPage)
	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx,
		collapsed(collapsedCols)+`
		 ORDER BY starts_at ASC, id ASC
		 LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list public events: %w", err)
	}
	defer rows.Close()

	var items []domain.EventListItem
	for rows.Next() {
		it, err := scanListItemRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan public event: %w", err)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate public events: %w", err)
	}
	return items, total, nil
}

// GetPublicByID reads ONE event by its own id for the guest-facing detail page,
// with exactly the visibility rule the cross-venue listing enforces: published,
// not yet ended, and — when it has a venue — at an active one. Whoever hosts it,
// including nobody.
//
// The rule is repeated in SQL rather than delegated to a Go filter over GetByID
// so that "what a guest may see" cannot drift between the list and the detail:
// the two would then disagree about the same event, and the app would show a
// card that 404s when tapped.
//
// The recurrence collapse of the listing is deliberately NOT applied here — the
// detail page addresses ONE occurrence, which is the whole reason the collapse
// is documented as a listing-only presentation rule.
func (r *Repository) GetPublicByID(ctx context.Context, id uuid.UUID, now time.Time) (*domain.EventListItem, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+listCols+`
		 FROM events e LEFT JOIN restaurants r ON r.id = e.restaurant_id
		 WHERE e.id = $1
		   AND e.status = 'published'
		   AND e.ends_at > $2
		   AND COALESCE(r.is_active, true) = true`, id, now)
	it, err := scanListItemRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get public event: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get public event: %w", err)
	}
	return it, nil
}

func collect(rows pgx.Rows, total int) ([]domain.Event, int, error) {
	defer rows.Close()
	var items []domain.Event
	for rows.Next() {
		e, err := scanEventRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan event: %w", err)
		}
		items = append(items, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate events: %w", err)
	}
	return items, total, nil
}

func scanEvent(row pgx.Row, op string) (*domain.Event, error) {
	e, err := scanEventRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return e, nil
}

func scanEventRow(row pgx.Row) (*domain.Event, error) {
	var e domain.Event
	var titleI18n, descI18n []byte
	var actionLabel, actionURL *string
	var overrides []string
	if err := row.Scan(&e.ID, &e.RestaurantID, &e.Title, &titleI18n, &e.Description, &descI18n,
		&e.StartsAt, &e.EndsAt, &e.Venue, &e.CoverImageURL, &e.Status, &e.Ticketed,
		&e.TicketPriceMinor, &e.Capacity, &e.Tags, &e.TicketsRefundable, &e.TicketRefundCutoffMinutes,
		&e.RecurrenceID, &e.CreatedAt, &e.UpdatedAt, &e.City, &actionLabel, &actionURL,
		&overrides); err != nil {
		return nil, err
	}
	e.TitleI18n = i18nFromDB(titleI18n)
	e.DescriptionI18n = i18nFromDB(descI18n)
	e.Tags = tagsFromDB(e.Tags)
	e.Action = actionFromDB(actionLabel, actionURL)
	e.ContentOverrides = overridesFromDB(overrides)
	return &e, nil
}

// scanListItemRow reads an event plus its host venue. Every venue column is
// scanned into a POINTER: the listing joins the venue with a LEFT JOIN now, so
// a platform event (events.restaurant_id IS NULL) brings three SQL NULLs back,
// and scanning those into strings would fail the whole query — the read path of
// the main Афиша — instead of producing a card without a venue.
func scanListItemRow(row pgx.Row) (*domain.EventListItem, error) {
	var it domain.EventListItem
	e := &it.Event
	var titleI18n, descI18n, venueNameI18n []byte
	var actionLabel, actionURL, venueName *string
	var venueCity *domain.City
	var overrides []string
	if err := row.Scan(&e.ID, &e.RestaurantID, &e.Title, &titleI18n, &e.Description, &descI18n,
		&e.StartsAt, &e.EndsAt, &e.Venue, &e.CoverImageURL, &e.Status, &e.Ticketed,
		&e.TicketPriceMinor, &e.Capacity, &e.Tags, &e.TicketsRefundable, &e.TicketRefundCutoffMinutes,
		&e.RecurrenceID, &e.CreatedAt, &e.UpdatedAt, &e.City, &actionLabel, &actionURL,
		&overrides, &venueName, &venueNameI18n, &venueCity); err != nil {
		return nil, err
	}
	e.TitleI18n = i18nFromDB(titleI18n)
	e.DescriptionI18n = i18nFromDB(descI18n)
	e.Tags = tagsFromDB(e.Tags)
	e.Action = actionFromDB(actionLabel, actionURL)
	e.ContentOverrides = overridesFromDB(overrides)
	it.Restaurant = venueFromDB(e.RestaurantID, venueName, venueNameI18n, venueCity)
	return &it, nil
}

// venueFromDB builds the card's venue block, or nil when the event has no host
// venue. The event's own restaurant_id is the authority on WHICH case this is:
// the joined columns can only ever be NULL together with it.
func venueFromDB(restaurantID *uuid.UUID, name *string, nameI18n []byte, city *domain.City) *domain.EventRestaurant {
	if restaurantID == nil {
		return nil
	}
	v := domain.EventRestaurant{ID: *restaurantID, NameI18n: i18nFromDB(nameI18n)}
	if name != nil {
		v.Name = *name
	}
	if city != nil {
		v.City = *city
	}
	return &v
}

// actionFromDB rebuilds the optional call-to-action button. No label = no
// button, whatever the url column says (a DB CHECK forbids that combination
// anyway); a label with no url means the button opens the event's own page.
func actionFromDB(label, actionURL *string) *domain.EventAction {
	if label == nil {
		return nil
	}
	return &domain.EventAction{Label: *label, URL: actionURL}
}

// actionLabelToDB / actionURLToDB write the button back, "no button" being two
// NULLs rather than empty strings — the CHECK constraints and every read here
// treat NULL as the single representation of absence.
func actionLabelToDB(a *domain.EventAction) *string {
	if a == nil {
		return nil
	}
	label := a.Label
	return &label
}

func actionURLToDB(a *domain.EventAction) *string {
	if a == nil {
		return nil
	}
	return a.URL
}

func statusStrings(statuses []domain.EventStatus) []string {
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

// tagsToDB guarantees the value bound to the text[] NOT NULL column is a real
// array, never SQL NULL: pgx encodes a nil slice as NULL, which the column
// refuses. A nil (or empty) tags list writes as the empty array '{}'.
func tagsToDB(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// tagsFromDB normalizes nil→[] on read so no consumer meets a nil-surprise. A
// text[] '{}' already scans into a non-nil empty slice; this only guards the
// defensive case.
func tagsFromDB(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

// overridesToDB renders the per-date override markers for the text[] NOT NULL
// column. Same nil-is-not-NULL rule as tagsToDB, and the same reason: pgx
// encodes a nil slice as SQL NULL and the column refuses it. An event with no
// overrides — every one-off event, and every date that follows its series —
// writes the empty array.
func overridesToDB(fields []domain.EventContentField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, string(f))
	}
	return out
}

// overridesFromDB reads the markers back. An unknown value cannot appear (a DB
// CHECK closes the vocabulary — migration 0097), so this is a plain retyping;
// a nil array becomes an empty slice so "no overrides" is never a nil-surprise.
func overridesFromDB(fields []string) []domain.EventContentField {
	out := make([]domain.EventContentField, 0, len(fields))
	for _, f := range fields {
		out = append(out, domain.EventContentField(f))
	}
	return out
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

// ReplaceImages rewrites an event's gallery in one transaction-visible pass:
// the old rows go, the new ones land with their index as `position`. Delete +
// insert rather than a diff because the set is small (a handful of photos) and
// the editor sends the whole ordered list — reconciling it row by row would be
// more code for the same result.
func (r *Repository) ReplaceImages(ctx context.Context, eventID uuid.UUID, urls []string) error {
	q := sqltx.From(ctx, r.pool)
	if _, err := q.Exec(ctx, `DELETE FROM event_images WHERE event_id=$1`, eventID); err != nil {
		return fmt.Errorf("clear event images: %w", err)
	}
	for i, url := range urls {
		if _, err := q.Exec(ctx,
			`INSERT INTO event_images (id, event_id, image_url, position) VALUES ($1,$2,$3,$4)`,
			uuid.New(), eventID, url, i); err != nil {
			return fmt.Errorf("insert event image: %w", err)
		}
	}
	return nil
}

// ImagesByEvent loads the galleries of several events in ONE query. The map has
// no entry for an event without photos — an absent key and an empty slice mean
// the same thing to every caller, and building empty slices for the majority
// would be waste.
func (r *Repository) ImagesByEvent(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT event_id, image_url FROM event_images
		 WHERE event_id = ANY($1)
		 ORDER BY event_id, position, created_at`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("list event images: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			return nil, fmt.Errorf("scan event image: %w", err)
		}
		out[id] = append(out[id], url)
	}
	return out, rows.Err()
}

// ListColumns is the `e.`-qualified event column list followed by the host
// venue's identity (`r.name, r.name_i18n, r.city`) — exactly what
// ScanListItem expects, in that order. Exported together with ScanListItem so a
// sibling package that joins through `events e JOIN restaurants r` (the
// favorites read) selects the same shape instead of duplicating a 23-column
// SELECT that would then drift from this one.
const ListColumns = listCols

// ScanListItem scans one row shaped like ListColumns into an EventListItem.
// Exported for the same reason as ListColumns.
func ScanListItem(row pgx.Row) (*domain.EventListItem, error) { return scanListItemRow(row) }

// RenameCityString rewrites ONLY the city-override string of every event linked
// to a city, after the dictionary entry was renamed (see usecase/cities). It is
// the events-side twin of restaurant.Repository.RenameCityString and exists for
// the same reason: the listing compares cities as exact strings, so an override
// left at the previous spelling would keep pointing at a city that no longer
// answers to that name — the event would silently vanish from every filter.
//
// Scoped by city_id, not by the old string: an override whose string was
// already out of step still gets fixed, and an event in another city can never
// be touched. updated_at is deliberately NOT bumped — nothing about the event
// changed for a guest, only the platform's spelling of a city, and touching it
// would make every event in a renamed city look freshly edited to the cabinet.
func (r *Repository) RenameCityString(ctx context.Context, cityID uuid.UUID, name string) (int64, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE events SET city=$2 WHERE city_id=$1 AND city IS DISTINCT FROM $2`, cityID, name)
	if err != nil {
		return 0, fmt.Errorf("rename event city string: %w", err)
	}
	return tag.RowsAffected(), nil
}

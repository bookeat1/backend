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
	created_at, updated_at`

// Create inserts a new event. An unknown restaurant_id (FK violation) maps to
// ErrNotFound, same convention as reviews/favorites.
func (r *Repository) Create(ctx context.Context, e *domain.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO events (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, venue, cover_image_url, status, ticketed, ticket_price_minor, capacity,
			tags, tickets_refundable, ticket_refund_cutoff_minutes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		 RETURNING created_at, updated_at`,
		e.ID, e.RestaurantID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity,
		tagsToDB(e.Tags), e.TicketsRefundable, e.TicketRefundCutoffMinutes).
		Scan(&e.CreatedAt, &e.UpdatedAt)
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

// Update overwrites the mutable fields of an existing event. A zero-rows UPDATE
// means the id is absent.
func (r *Repository) Update(ctx context.Context, e *domain.Event) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE events SET title = $2, title_i18n = $3, description = $4, description_i18n = $5,
			starts_at = $6, ends_at = $7, venue = $8, cover_image_url = $9, status = $10,
			ticketed = $11, ticket_price_minor = $12, capacity = $13, tags = $14,
			tickets_refundable = $15, ticket_refund_cutoff_minutes = $16, updated_at = now()
		 WHERE id = $1`,
		e.ID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity,
		tagsToDB(e.Tags), e.TicketsRefundable, e.TicketRefundCutoffMinutes)
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
	e.created_at, e.updated_at,
	r.name, r.name_i18n, r.city`

// ListPublicUpcoming implements the cross-venue public listing. Visibility is
// enforced in SQL and is not negotiable by a filter: published, not yet ended,
// and hosted by an active restaurant — a deactivated venue disappears from the
// guest app together with its events, the same rule the catalog listing keeps
// (restaurants.ListActive). hidden_from_home is deliberately NOT applied: it
// hides a venue from the main screen only, not from the catalog, and this is a
// catalog-style listing.
func (r *Repository) ListPublicUpcoming(ctx context.Context, f domain.PublicEventFilter, now time.Time) ([]domain.EventListItem, int, error) {
	where := []string{"e.status = 'published'", "r.is_active = true"}
	args := []any{now}
	where = append(where, "e.ends_at > $1")
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.City != nil {
		add("r.city = $%d", string(*f.City))
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
	from := ` FROM events e JOIN restaurants r ON r.id = e.restaurant_id WHERE ` + whereSQL

	q := sqltx.From(ctx, r.pool)
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count public events: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	page, perPage := normalizePage(f.Page, f.PerPage)
	args = append(args, perPage, (page-1)*perPage)
	rows, err := q.Query(ctx,
		`SELECT `+listCols+from+`
		 ORDER BY e.starts_at ASC, e.id ASC
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
	if err := row.Scan(&e.ID, &e.RestaurantID, &e.Title, &titleI18n, &e.Description, &descI18n,
		&e.StartsAt, &e.EndsAt, &e.Venue, &e.CoverImageURL, &e.Status, &e.Ticketed,
		&e.TicketPriceMinor, &e.Capacity, &e.Tags, &e.TicketsRefundable, &e.TicketRefundCutoffMinutes,
		&e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.TitleI18n = i18nFromDB(titleI18n)
	e.DescriptionI18n = i18nFromDB(descI18n)
	e.Tags = tagsFromDB(e.Tags)
	return &e, nil
}

func scanListItemRow(row pgx.Row) (*domain.EventListItem, error) {
	var it domain.EventListItem
	e := &it.Event
	var titleI18n, descI18n, venueNameI18n []byte
	if err := row.Scan(&e.ID, &e.RestaurantID, &e.Title, &titleI18n, &e.Description, &descI18n,
		&e.StartsAt, &e.EndsAt, &e.Venue, &e.CoverImageURL, &e.Status, &e.Ticketed,
		&e.TicketPriceMinor, &e.Capacity, &e.Tags, &e.TicketsRefundable, &e.TicketRefundCutoffMinutes,
		&e.CreatedAt, &e.UpdatedAt,
		&it.Restaurant.Name, &venueNameI18n, &it.Restaurant.City); err != nil {
		return nil, err
	}
	e.TitleI18n = i18nFromDB(titleI18n)
	e.DescriptionI18n = i18nFromDB(descI18n)
	e.Tags = tagsFromDB(e.Tags)
	it.Restaurant.ID = e.RestaurantID
	it.Restaurant.NameI18n = i18nFromDB(venueNameI18n)
	return &it, nil
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

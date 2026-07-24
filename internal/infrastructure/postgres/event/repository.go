// Package event is the Postgres implementation of domain.EventRepository.
package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ticket_price_minor, capacity, created_at, updated_at`

// Create inserts a new event. An unknown restaurant_id (FK violation) maps to
// ErrNotFound, same convention as reviews/favorites.
func (r *Repository) Create(ctx context.Context, e *domain.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO events (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, venue, cover_image_url, status, ticketed, ticket_price_minor, capacity)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING created_at, updated_at`,
		e.ID, e.RestaurantID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity).
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
			ticketed = $11, ticket_price_minor = $12, capacity = $13, updated_at = now()
		 WHERE id = $1`,
		e.ID, e.Title, i18nToDB(e.TitleI18n), e.Description, i18nToDB(e.DescriptionI18n),
		e.StartsAt, e.EndsAt, e.Venue, e.CoverImageURL, e.Status, e.Ticketed, e.TicketPriceMinor, e.Capacity)
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
		&e.TicketPriceMinor, &e.Capacity, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, err
	}
	e.TitleI18n = i18nFromDB(titleI18n)
	e.DescriptionI18n = i18nFromDB(descI18n)
	return &e, nil
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

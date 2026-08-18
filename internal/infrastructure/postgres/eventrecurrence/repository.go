// Package eventrecurrence is the Postgres implementation of
// domain.EventRecurrenceRepository: the recurrence rules themselves, the
// idempotent materialisation of their occurrences into `events`, and the
// tombstones that keep a removed occurrence removed.
package eventrecurrence

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
	"backend-core/internal/infrastructure/sqltx"
)

const foreignKeyViolation = "23503"

// Repository implements domain.EventRecurrenceRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the event-recurrence repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.EventRecurrenceRepository = (*Repository)(nil)

const selectCols = `id, restaurant_id, title, title_i18n, description, description_i18n,
	venue, cover_image_url, tags, occurrence_status, ticketed, ticket_price_minor, capacity,
	tickets_refundable, ticket_refund_cutoff_minutes,
	frequency, weekdays, month_day, start_minutes, duration_minutes, timezone,
	starts_on, until_date, is_active, created_at, updated_at`

// Create inserts a new rule. An unknown restaurant_id (FK violation) maps to
// ErrNotFound, same convention as the events repository.
func (r *Repository) Create(ctx context.Context, rec *domain.EventRecurrence) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO event_recurrences (id, restaurant_id, title, title_i18n, description, description_i18n,
			venue, cover_image_url, tags, occurrence_status, ticketed, ticket_price_minor, capacity,
			tickets_refundable, ticket_refund_cutoff_minutes,
			frequency, weekdays, month_day, start_minutes, duration_minutes, timezone,
			starts_on, until_date, is_active)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		 RETURNING created_at, updated_at`,
		rec.ID, rec.RestaurantID, rec.Title, i18nToDB(rec.TitleI18n), rec.Description, i18nToDB(rec.DescriptionI18n),
		rec.Venue, rec.CoverImageURL, tagsToDB(rec.Tags), rec.OccurrenceStatus, rec.Ticketed,
		rec.TicketPriceMinor, rec.Capacity, rec.TicketsRefundable, rec.TicketRefundCutoffMinutes,
		rec.Frequency, weekdaysToDB(rec.Weekdays), rec.MonthDay, rec.StartMinutes, rec.DurationMinutes,
		timezoneToDB(rec.Timezone), dateToDB(&rec.StartsOn), dateToDB(rec.UntilDate), rec.IsActive).
		Scan(&rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create event recurrence: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create event recurrence: %w", err)
	}
	return nil
}

// GetByID returns a rule regardless of its active flag.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.EventRecurrence, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selectCols+` FROM event_recurrences WHERE id = $1`, id)
	rec, err := scanRecurrence(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get event recurrence: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get event recurrence: %w", err)
	}
	return rec, nil
}

// Update overwrites the mutable fields of a rule. restaurant_id is NOT among
// them: a rule never migrates to another venue, and allowing it would silently
// re-tenant every occurrence it has already generated.
func (r *Repository) Update(ctx context.Context, rec *domain.EventRecurrence) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_recurrences SET title=$2, title_i18n=$3, description=$4, description_i18n=$5,
			venue=$6, cover_image_url=$7, tags=$8, occurrence_status=$9, ticketed=$10,
			ticket_price_minor=$11, capacity=$12, tickets_refundable=$13, ticket_refund_cutoff_minutes=$14,
			frequency=$15, weekdays=$16, month_day=$17, start_minutes=$18, duration_minutes=$19,
			timezone=$20, starts_on=$21, until_date=$22, is_active=$23, updated_at=now()
		 WHERE id=$1`,
		rec.ID, rec.Title, i18nToDB(rec.TitleI18n), rec.Description, i18nToDB(rec.DescriptionI18n),
		rec.Venue, rec.CoverImageURL, tagsToDB(rec.Tags), rec.OccurrenceStatus, rec.Ticketed,
		rec.TicketPriceMinor, rec.Capacity, rec.TicketsRefundable, rec.TicketRefundCutoffMinutes,
		rec.Frequency, weekdaysToDB(rec.Weekdays), rec.MonthDay, rec.StartMinutes, rec.DurationMinutes,
		timezoneToDB(rec.Timezone), dateToDB(&rec.StartsOn), dateToDB(rec.UntilDate), rec.IsActive)
	if err != nil {
		return fmt.Errorf("update event recurrence: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update event recurrence: %w", domain.ErrNotFound)
	}
	return nil
}

// SetActive flips the active flag and nothing else. Occurrences already
// generated are untouched — deactivating stops the FUTURE, it does not rewrite
// the past.
func (r *Repository) SetActive(ctx context.Context, id uuid.UUID, active bool) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_recurrences SET is_active=$2, updated_at=now() WHERE id=$1`, id, active)
	if err != nil {
		return fmt.Errorf("set event recurrence active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set event recurrence active: %w", domain.ErrNotFound)
	}
	return nil
}

// ListByRestaurant returns a venue's rules for the cabinet, newest first with
// id as a stable tie-breaker.
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.EventRecurrence, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM event_recurrences WHERE restaurant_id = $1`, restaurantID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count event recurrences: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM event_recurrences
		 WHERE restaurant_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		restaurantID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list event recurrences: %w", err)
	}
	defer rows.Close()

	var items []domain.EventRecurrence
	for rows.Next() {
		rec, err := scanRecurrence(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan event recurrence: %w", err)
		}
		items = append(items, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate event recurrences: %w", err)
	}
	return items, total, nil
}

// ListActive is the generator's scan: active rules at ACTIVE venues, with the
// venue's own timezone joined in so the worker resolves the zone without a
// second query per rule.
//
// A rule at a deactivated venue is skipped on purpose: the whole venue is
// invisible to guests (restaurants.ListActive, ListPublicUpcoming), so quietly
// filling its Афиша weeks ahead would only pile up rows nobody can see.
// Keyset-paginated by id — the pass reads every active rule, and an offset scan
// would re-read what it already processed if a rule were added mid-pass.
func (r *Repository) ListActive(ctx context.Context, afterID uuid.UUID, limit int) ([]domain.ActiveEventRecurrence, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+prefixed(selectCols, "er.")+`, coalesce(rest.timezone, '')
		 FROM event_recurrences er
		 JOIN restaurants rest ON rest.id = er.restaurant_id
		 WHERE er.is_active AND rest.is_active AND er.id > $1
		 ORDER BY er.id
		 LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list active event recurrences: %w", err)
	}
	defer rows.Close()

	var items []domain.ActiveEventRecurrence
	for rows.Next() {
		var it domain.ActiveEventRecurrence
		if err := scanRecurrenceInto(rows, &it.EventRecurrence, &it.VenueTimezone); err != nil {
			return nil, fmt.Errorf("scan active event recurrence: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active event recurrences: %w", err)
	}
	return items, nil
}

// InsertOccurrences materialises slots as `events` rows in ONE statement, and
// returns how many rows were actually inserted.
//
// Idempotency and concurrency safety are the DATABASE's job here, not the
// worker's:
//
//   - ON CONFLICT (recurrence_id, starts_at) DO NOTHING against the partial
//     unique index means a re-run inserts nothing, and two workers materialising
//     the same slot at the same instant produce exactly one row — the loser
//     simply gets rowsAffected 0. A read-then-insert check could not promise
//     that.
//   - the NOT EXISTS against event_recurrence_skips is what keeps a DELETED
//     occurrence deleted. An occurrence that still exists — cancelled (hidden),
//     rescheduled, retitled, whatever the venue did to it — is protected by the
//     unique index instead: this statement only ever INSERTs, it never updates
//     an existing row, so a single edited date is never overwritten by the
//     template.
func (r *Repository) InsertOccurrences(ctx context.Context, rec *domain.EventRecurrence, slots []time.Time) (int, error) {
	if len(slots) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(slots))
	starts := make([]time.Time, len(slots))
	ends := make([]time.Time, len(slots))
	for i, s := range slots {
		ids[i] = uuid.New()
		starts[i] = s
		ends[i] = rec.EndOf(s)
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, venue, cover_image_url, status, ticketed, ticket_price_minor,
			capacity, tags, tickets_refundable, ticket_refund_cutoff_minutes, recurrence_id)
		 SELECT s.id, $1, $2, $3, $4, $5,
			s.starts_at, s.ends_at, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		 FROM unnest($16::uuid[], $17::timestamptz[], $18::timestamptz[]) AS s(id, starts_at, ends_at)
		 WHERE NOT EXISTS (
			SELECT 1 FROM event_recurrence_skips k
			WHERE k.recurrence_id = $15 AND k.slot_starts_at = s.starts_at)
		 ON CONFLICT (recurrence_id, starts_at) WHERE recurrence_id IS NOT NULL DO NOTHING`,
		rec.RestaurantID, rec.Title, i18nToDB(rec.TitleI18n), rec.Description, i18nToDB(rec.DescriptionI18n),
		rec.Venue, rec.CoverImageURL, rec.OccurrenceStatus, rec.Ticketed, rec.TicketPriceMinor,
		rec.Capacity, tagsToDB(rec.Tags), rec.TicketsRefundable, rec.TicketRefundCutoffMinutes,
		rec.ID, ids, starts, ends)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return 0, fmt.Errorf("insert occurrences: %w", domain.ErrNotFound)
		}
		return 0, fmt.Errorf("insert occurrences: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordSkip tombstones one slot. Idempotent: recording the same skip twice is
// a no-op, so the caller never has to check first.
func (r *Repository) RecordSkip(ctx context.Context, recurrenceID uuid.UUID, slot time.Time) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO event_recurrence_skips (recurrence_id, slot_starts_at)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`, recurrenceID, slot)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			// The rule is gone: nothing generates this slot any more, so there
			// is nothing to protect it from.
			return nil
		}
		return fmt.Errorf("record occurrence skip: %w", err)
	}
	return nil
}

// --- scanning / encoding helpers ---

func scanRecurrence(row pgx.Row) (*domain.EventRecurrence, error) {
	var rec domain.EventRecurrence
	if err := scanRecurrenceInto(row, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// scanRecurrenceInto reads selectCols into rec, appending any extra
// destinations (ListActive adds the venue timezone) after them.
func scanRecurrenceInto(row pgx.Row, rec *domain.EventRecurrence, extra ...any) error {
	var titleI18n, descI18n []byte
	var timezone *string
	var weekdays []int16
	var startsOn time.Time
	var until *time.Time

	dest := []any{
		&rec.ID, &rec.RestaurantID, &rec.Title, &titleI18n, &rec.Description, &descI18n,
		&rec.Venue, &rec.CoverImageURL, &rec.Tags, &rec.OccurrenceStatus, &rec.Ticketed,
		&rec.TicketPriceMinor, &rec.Capacity, &rec.TicketsRefundable, &rec.TicketRefundCutoffMinutes,
		&rec.Frequency, &weekdays, &rec.MonthDay, &rec.StartMinutes, &rec.DurationMinutes, &timezone,
		&startsOn, &until, &rec.IsActive, &rec.CreatedAt, &rec.UpdatedAt,
	}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	rec.TitleI18n = i18nFromDB(titleI18n)
	rec.DescriptionI18n = i18nFromDB(descI18n)
	rec.Tags = tagsFromDB(rec.Tags)
	rec.Weekdays = weekdaysFromDB(weekdays)
	if timezone != nil {
		rec.Timezone = *timezone
	}
	rec.StartsOn = dateFromDB(startsOn)
	if until != nil {
		d := dateFromDB(*until)
		rec.UntilDate = &d
	}
	return nil
}

// dateToDB renders a calendar date for a `date` column. It goes through a
// time.Time at UTC midnight because that is what pgx binds, and a `date` column
// stores no zone — the value is a day on a wall calendar, exactly what
// domain.CalendarDate means. nil (no until-date) binds as SQL NULL.
func dateToDB(d *domain.CalendarDate) any {
	if d == nil {
		return nil
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// dateFromDB reads a `date` column back. pgx hands over UTC midnight of that
// day; only the calendar fields are kept, so the value can never be mistaken
// for an instant later on.
func dateFromDB(t time.Time) domain.CalendarDate {
	u := t.UTC()
	return domain.CalendarDate{Year: u.Year(), Month: u.Month(), Day: u.Day()}
}

// timezoneToDB stores "no override" as SQL NULL, never as the empty string:
// time.LoadLocation("") silently returns UTC, so an empty zone in the database
// would look valid and move every occurrence by the venue's offset. The column
// carries a CHECK for the same reason (migration 0074).
func timezoneToDB(tz string) any {
	if tz == "" {
		return nil
	}
	return tz
}

func weekdaysToDB(ws []domain.ISOWeekday) []int16 {
	out := make([]int16, 0, len(ws))
	for _, w := range ws {
		out = append(out, int16(w))
	}
	return out
}

func weekdaysFromDB(ws []int16) []domain.ISOWeekday {
	out := make([]domain.ISOWeekday, 0, len(ws))
	for _, w := range ws {
		out = append(out, domain.ISOWeekday(w))
	}
	return out
}

// prefixed qualifies a comma-separated column list with a table alias so the
// joined query in ListActive can reuse selectCols instead of duplicating it —
// a second hand-maintained copy is how a column silently drops out of one read
// path and not the other.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		trimmed := strings.TrimLeft(p, " \t\r\n")
		parts[i] = p[:len(p)-len(trimmed)] + alias + trimmed
	}
	return strings.Join(parts, ",")
}

func tagsToDB(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

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

func normalizePage(page, perPage int) (int, int) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	return page, perPage
}

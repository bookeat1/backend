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
	starts_on, until_date, is_active,
	occurrence_feed_status, feed_submitted_at, feed_reviewed_by, feed_reviewed_at, feed_rejection_reason,
	created_at, updated_at`

// Create inserts a new rule. An unknown restaurant_id (FK violation) maps to
// ErrNotFound, same convention as the events repository.
//
// The series-level feed decision is NOT taken from rec: a rule is always born
// out of the feed (not_submitted) and moves from there through the moderation
// flow (TransitionFeedStatus), the same way a promo or an event does. There is
// deliberately no code path in which a create call carries "approved".
func (r *Repository) Create(ctx context.Context, rec *domain.EventRecurrence) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	rec.OccurrenceFeedStatus = domain.FeedNotSubmitted
	rec.FeedSubmittedAt, rec.FeedReviewedBy, rec.FeedReviewedAt, rec.FeedRejectionReason = nil, nil, nil, nil
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
//
// The feed columns are not among them either, and for a stronger reason: this
// statement is driven by the cabinet's full-replace payload, so writing
// occurrence_feed_status here would let a venue set its own moderation state —
// and would silently withdraw an approved rule every time an older cabinet
// build sent an event edit without the field. The series-level decision moves
// only through TransitionFeedStatus / DemoteFeedAfterContentEdit.
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
	// The occurrence's feed_status is decided by domain.OccurrenceFeedStatusOf,
	// never copied verbatim from the rule: only an APPROVED series produces
	// approved occurrences (see that function for why a pending series must not
	// push its dates into the item queue). The reviewer stamp travels with it so
	// a card on the main screen can name the human who allowed it, and
	// feed_submitted_at is the moment the venue asked about the series.
	feedStatus := domain.OccurrenceFeedStatusOf(rec.OccurrenceFeedStatus)
	var submittedAt, reviewedAt *time.Time
	var reviewedBy *uuid.UUID
	if feedStatus == domain.FeedApproved {
		submittedAt, reviewedBy, reviewedAt = rec.FeedSubmittedAt, rec.FeedReviewedBy, rec.FeedReviewedAt
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO events (id, restaurant_id, title, title_i18n, description, description_i18n,
			starts_at, ends_at, venue, cover_image_url, status, ticketed, ticket_price_minor,
			capacity, tags, tickets_refundable, ticket_refund_cutoff_minutes, recurrence_id,
			feed_status, feed_submitted_at, feed_reviewed_by, feed_reviewed_at)
		 SELECT s.id, $1, $2, $3, $4, $5,
			s.starts_at, s.ends_at, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
			$19::varchar, $20, $21, $22
		 FROM unnest($16::uuid[], $17::timestamptz[], $18::timestamptz[]) AS s(id, starts_at, ends_at)
		 WHERE NOT EXISTS (
			SELECT 1 FROM event_recurrence_skips k
			WHERE k.recurrence_id = $15 AND k.slot_starts_at = s.starts_at)
		 ON CONFLICT (recurrence_id, starts_at) WHERE recurrence_id IS NOT NULL DO NOTHING`,
		rec.RestaurantID, rec.Title, i18nToDB(rec.TitleI18n), rec.Description, i18nToDB(rec.DescriptionI18n),
		rec.Venue, rec.CoverImageURL, rec.OccurrenceStatus, rec.Ticketed, rec.TicketPriceMinor,
		rec.Capacity, tagsToDB(rec.Tags), rec.TicketsRefundable, rec.TicketRefundCutoffMinutes,
		rec.ID, ids, starts, ends,
		string(feedStatus), submittedAt, reviewedBy, reviewedAt)
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

// TransitionFeedStatus is the CAS the series-level moderation rests on, the
// twin of feed.Repository.TransitionFeedStatus: the expected current status
// travels in the WHERE clause, so two concurrent decisions cannot both apply —
// the loser gets ErrInvalidStatus instead of overwriting the winner.
func (r *Repository) TransitionFeedStatus(ctx context.Context, id uuid.UUID, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) error {
	if len(from) == 0 {
		return fmt.Errorf("%w: a feed transition needs an expected current status", domain.ErrValidation)
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_recurrences SET
			occurrence_feed_status = $2::varchar,
			feed_submitted_at = $3,
			feed_reviewed_by = $4,
			feed_reviewed_at = $5,
			feed_rejection_reason = $6,
			updated_at = now()
		 WHERE id = $1 AND occurrence_feed_status = ANY($7::text[])`,
		id, string(upd.Status), upd.SubmittedAt, upd.ReviewedBy, upd.ReviewedAt,
		upd.RejectionReason, feedStatusStrings(from))
	if err != nil {
		return fmt.Errorf("transition recurrence feed status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.explainNoRows(ctx, id, "transition recurrence feed status")
	}
	return nil
}

// DemoteFeedAfterContentEdit implements domain.FeedStatusAfterContentEdit for a
// rule: a decision made about specific words stops being valid when the
// template changes. A missing id is not an error — the caller resolved the rule
// already and turning a benign race into a 404 would only mask the real edit
// error, exactly as in feed.Repository.DemoteAfterContentEdit.
func (r *Repository) DemoteFeedAfterContentEdit(ctx context.Context, id uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE event_recurrences SET
			occurrence_feed_status = $2::varchar,
			feed_reviewed_by = NULL,
			feed_reviewed_at = NULL,
			feed_rejection_reason = NULL,
			updated_at = now()
		 WHERE id = $1 AND occurrence_feed_status = ANY($3::text[])`,
		id, string(domain.FeedPendingReview),
		[]string{string(domain.FeedApproved), string(domain.FeedRejected)})
	if err != nil {
		return fmt.Errorf("demote recurrence feed status after edit: %w", err)
	}
	return nil
}

// SyncOccurrenceFeedStatus carries a decision about the SERIES down to the
// occurrences that were already materialised.
//
// Three bounds make it safe to run on a live table:
//
//   - ends_at > $2 — a past occurrence is history and is never rewritten;
//   - feed_status = ANY(from) — an occurrence a moderator decided on
//     individually keeps that decision; the series verdict only moves the
//     undecided ones;
//   - recurrence_id = $1 — one rule at a time, which is also the index
//     (idx_events_recurrence_feed_sync).
//
// The placement weight is deliberately untouched: a sold placement on one date
// is not lost because the series was re-approved.
func (r *Repository) SyncOccurrenceFeedStatus(ctx context.Context, recurrenceID uuid.UUID, notEndedBefore time.Time, from []domain.FeedStatus, upd domain.FeedPlacementUpdate) (int, error) {
	if len(from) == 0 {
		return 0, fmt.Errorf("%w: a feed sync needs an expected current status", domain.ErrValidation)
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE events SET
			feed_status = $3::varchar,
			feed_submitted_at = $4,
			feed_reviewed_by = $5,
			feed_reviewed_at = $6,
			feed_rejection_reason = $7,
			updated_at = now()
		 WHERE recurrence_id = $1 AND ends_at > $2 AND feed_status = ANY($8::text[])`,
		recurrenceID, notEndedBefore, string(upd.Status), upd.SubmittedAt, upd.ReviewedBy,
		upd.ReviewedAt, upd.RejectionReason, feedStatusStrings(from))
	if err != nil {
		return 0, fmt.Errorf("sync occurrence feed status: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SyncOccurrenceContent pushes the series' editorial content down onto the
// occurrences already generated — the statement that makes «заполнил один раз»
// true for all eighteen dates of «Афиша Greek Party».
//
// It is one statement on purpose. The CTE decides, per row and per FIELD,
// whether that field is BOTH inherited (not listed in content_overrides) and
// actually different from the series; the UPDATE then writes only those fields,
// only on rows where at least one of them changed. Consequences worth naming:
//
//   - a date that owns its poster keeps it while still following the series
//     text — the override is per field, not per row;
//   - a row that is already identical is not written at all, so updated_at is
//     not churned and RowsAffected answers "how many dates actually changed";
//   - ends_at > $2 keeps history out of it: a date that already happened is
//     never retitled, whatever the venue does to the rule afterwards.
//
// The feed columns move with the content, and only for the rows being
// rewritten: an APPROVED occurrence goes back to not_submitted with its
// reviewer stamp cleared, because the platform approved the old words. It goes
// to not_submitted rather than pending_review deliberately — see
// domain.OccurrenceFeedStatusOf: the object under review is the series (which
// DemoteFeedAfterContentEdit has just moved to pending_review), and eighteen
// identical dates must never land in the item queue.
func (r *Repository) SyncOccurrenceContent(ctx context.Context, recurrenceID uuid.UUID, notEndedBefore time.Time, c domain.EventContent) (int, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`WITH target AS (
			SELECT e.id,
				NOT ('title' = ANY (e.content_overrides))
					AND (e.title IS DISTINCT FROM $3::varchar
						OR e.title_i18n IS DISTINCT FROM $4::jsonb)                AS chg_title,
				NOT ('description' = ANY (e.content_overrides))
					AND (e.description IS DISTINCT FROM $5::text
						OR e.description_i18n IS DISTINCT FROM $6::jsonb)          AS chg_description,
				NOT ('venue' = ANY (e.content_overrides))
					AND e.venue IS DISTINCT FROM $7::varchar                       AS chg_venue,
				NOT ('cover_image_url' = ANY (e.content_overrides))
					AND e.cover_image_url IS DISTINCT FROM $8::varchar             AS chg_cover,
				NOT ('tags' = ANY (e.content_overrides))
					AND e.tags IS DISTINCT FROM $9::text[]                         AS chg_tags
			FROM events e
			WHERE e.recurrence_id = $1 AND e.ends_at > $2
		)
		UPDATE events e SET
			title            = CASE WHEN t.chg_title THEN $3::varchar ELSE e.title END,
			title_i18n       = CASE WHEN t.chg_title THEN $4::jsonb ELSE e.title_i18n END,
			description      = CASE WHEN t.chg_description THEN $5::text ELSE e.description END,
			description_i18n = CASE WHEN t.chg_description THEN $6::jsonb ELSE e.description_i18n END,
			venue            = CASE WHEN t.chg_venue THEN $7::varchar ELSE e.venue END,
			cover_image_url  = CASE WHEN t.chg_cover THEN $8::varchar ELSE e.cover_image_url END,
			tags             = CASE WHEN t.chg_tags THEN $9::text[] ELSE e.tags END,
			feed_status      = CASE WHEN e.feed_status = $10::varchar THEN $11::varchar ELSE e.feed_status END,
			feed_reviewed_by = CASE WHEN e.feed_status = $10::varchar THEN NULL ELSE e.feed_reviewed_by END,
			feed_reviewed_at = CASE WHEN e.feed_status = $10::varchar THEN NULL ELSE e.feed_reviewed_at END,
			updated_at       = now()
		FROM target t
		WHERE e.id = t.id
		  AND (t.chg_title OR t.chg_description OR t.chg_venue OR t.chg_cover OR t.chg_tags)`,
		recurrenceID, notEndedBefore,
		c.Title, i18nToDB(c.TitleI18n), c.Description, i18nToDB(c.DescriptionI18n),
		c.Venue, c.CoverImageURL, tagsToDB(c.Tags),
		string(domain.FeedApproved), string(domain.FeedNotSubmitted))
	if err != nil {
		return 0, fmt.Errorf("sync occurrence content: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListByFeedStatus is the superadmin's rule queue: oldest submission first, id
// as the stable tie-break (same FIFO shape as the item queue).
func (r *Repository) ListByFeedStatus(ctx context.Context, status domain.FeedStatus, page, perPage int) ([]domain.EventRecurrence, int, error) {
	page, perPage = normalizePage(page, perPage)
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM event_recurrences WHERE occurrence_feed_status = $1::varchar`,
		string(status)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count event recurrences by feed status: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM event_recurrences
		 WHERE occurrence_feed_status = $1::varchar
		 ORDER BY feed_submitted_at ASC, id ASC
		 LIMIT $2 OFFSET $3`,
		string(status), perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list event recurrences by feed status: %w", err)
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

// explainNoRows turns a zero-row CAS into the right sentinel: ErrNotFound when
// the rule is gone, ErrInvalidStatus when it is simply in another state.
func (r *Repository) explainNoRows(ctx context.Context, id uuid.UUID, op string) error {
	var exists bool
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM event_recurrences WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return fmt.Errorf("%s: %w: the rule is not in the expected feed status", op, domain.ErrInvalidStatus)
}

func feedStatusStrings(statuses []domain.FeedStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, string(s))
	}
	return out
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
		&startsOn, &until, &rec.IsActive,
		&rec.OccurrenceFeedStatus, &rec.FeedSubmittedAt, &rec.FeedReviewedBy, &rec.FeedReviewedAt,
		&rec.FeedRejectionReason,
		&rec.CreatedAt, &rec.UpdatedAt,
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

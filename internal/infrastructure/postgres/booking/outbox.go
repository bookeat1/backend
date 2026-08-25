package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Outbox implements domain.BookingOutboxRepository.
type Outbox struct{ pool sqltx.Querier }

// NewOutbox builds the transactional-outbox repository.
func NewOutbox(pool sqltx.Querier) *Outbox { return &Outbox{pool: pool} }

var _ domain.BookingOutboxRepository = (*Outbox)(nil)

// outboxCols is the INSERT column list. Retry bookkeeping (attempts,
// next_attempt_at, last_error, abandoned_at — migration 0083) is owned by the
// dispatcher and never written at creation time, so it stays out of it.
const outboxCols = `id, booking_id, event_type, payload, created_at, published_at`

// outboxSelectCols adds the retry bookkeeping the dispatcher reads back.
const outboxSelectCols = outboxCols + `, attempts, next_attempt_at, last_error, abandoned_at`

func (r *Outbox) Create(ctx context.Context, e *domain.BookingOutboxEvent) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	q := `INSERT INTO booking_outbox (` + outboxCols + `) VALUES ($1,$2,$3,$4,$5,$6)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, e.ID, e.BookingID,
		string(e.EventType), []byte(payload), e.CreatedAt, e.PublishedAt); err != nil {
		return mapWrite(err, "create booking outbox event")
	}
	return nil
}

// ClaimDue locks undelivered, not-abandoned, currently-due events with FOR
// UPDATE SKIP LOCKED. It must run inside a TxManager transaction, otherwise the
// locks are dropped at once and two workers publish the same event twice.
//
// The ORDER BY is the fairness rule, and it is the whole point of migration
// 0083: `attempts > 0` sorts false (fresh) before true (retry), so a retry can
// only occupy batch capacity that no fresh event needed. Within each group the
// oldest due event goes first. A channel that is down for hours therefore
// cannot re-fill the batch with its own failures and starve every other
// channel's newer events — the failures are also invisible until
// next_attempt_at, which is what stops the every-tick re-read.
func (r *Outbox) ClaimDue(ctx context.Context, limit int, now time.Time) ([]domain.BookingOutboxEvent, error) {
	limit, _ = window(limit, 0)
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+outboxSelectCols+` FROM booking_outbox
		 WHERE published_at IS NULL
		   AND abandoned_at IS NULL
		   AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
		 ORDER BY (attempts > 0), COALESCE(next_attempt_at, created_at)
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit, now)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()
	var out []domain.BookingOutboxEvent
	for rows.Next() {
		var e domain.BookingOutboxEvent
		var eventType string
		var payload []byte
		var lastErr *string
		if err := rows.Scan(&e.ID, &e.BookingID, &eventType, &payload, &e.CreatedAt, &e.PublishedAt,
			&e.Attempts, &e.NextAttemptAt, &lastErr, &e.AbandonedAt); err != nil {
			return nil, fmt.Errorf("claim outbox events: %w", err)
		}
		e.EventType = domain.BookingEventType(eventType)
		e.Payload = json.RawMessage(payload)
		if lastErr != nil {
			e.LastError = *lastErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Reschedule bumps the attempt counter and pushes each event to the back of the
// queue until its own next_attempt_at. One statement per batch: the ids, the
// deadlines and the errors travel as three parallel arrays, each bound once and
// cast explicitly (a parameter reused under two types is how 42P08 bites here).
func (r *Outbox) Reschedule(ctx context.Context, failures []domain.BookingOutboxFailure) error {
	if len(failures) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(failures))
	next := make([]time.Time, len(failures))
	errs := make([]string, len(failures))
	for i, f := range failures {
		ids[i], next[i], errs[i] = f.ID, f.NextAttemptAt, f.LastError
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_outbox AS o
		 SET attempts = o.attempts + 1, next_attempt_at = v.next_attempt_at, last_error = v.last_error
		 FROM (SELECT unnest($1::uuid[])        AS id,
		              unnest($2::timestamptz[]) AS next_attempt_at,
		              unnest($3::text[])        AS last_error) AS v
		 WHERE o.id = v.id`, ids, next, errs); err != nil {
		return fmt.Errorf("reschedule outbox events: %w", err)
	}
	return nil
}

// Abandon stamps abandoned_at on events whose attempt budget ran out. They stop
// being claimed but are deliberately left unpublished, so
// `WHERE abandoned_at IS NOT NULL` is an exact dead-letter query rather than a
// guess at which published rows never actually went anywhere.
func (r *Outbox) Abandon(ctx context.Context, failures []domain.BookingOutboxFailure, at time.Time) error {
	if len(failures) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(failures))
	errs := make([]string, len(failures))
	for i, f := range failures {
		ids[i], errs[i] = f.ID, f.LastError
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_outbox AS o
		 SET attempts = o.attempts + 1, abandoned_at = $2, next_attempt_at = NULL, last_error = v.last_error
		 FROM (SELECT unnest($1::uuid[]) AS id,
		              unnest($3::text[]) AS last_error) AS v
		 WHERE o.id = v.id`, ids, at, errs); err != nil {
		return fmt.Errorf("abandon outbox events: %w", err)
	}
	return nil
}

// ExistsForBooking reports whether an event of that type already exists for the
// booking (served by idx_booking_outbox_booking).
func (r *Outbox) ExistsForBooking(ctx context.Context, bookingID uuid.UUID, eventType domain.BookingEventType) (bool, error) {
	var exists bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM booking_outbox WHERE booking_id=$1 AND event_type=$2)`,
		bookingID, string(eventType)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check outbox event: %w", err)
	}
	return exists, nil
}

func (r *Outbox) MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE booking_outbox SET published_at=$2 WHERE id = ANY($1)`, ids, at); err != nil {
		return fmt.Errorf("mark outbox events published: %w", err)
	}
	return nil
}

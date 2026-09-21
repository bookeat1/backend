// Package pushcampaign is the Postgres implementation of
// domain.PushCampaignRepository, domain.PushCampaignRecipientRepository,
// domain.PushCampaignSubjectRepository and domain.PushCampaignAudienceRepository
// (migration 0111).
package pushcampaign

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

// ---------------------------------------------------------------------------
// PushCampaignRepository
// ---------------------------------------------------------------------------

// Repository implements domain.PushCampaignRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the campaign-queue repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.PushCampaignRepository = (*Repository)(nil)

const campaignCols = `id, kind, subject_id, restaurant_id, city_id, city, status, cancel_reason,
	force_quiet_hours, estimated_recipients, attempts, next_attempt_at, lease_until, last_error,
	sent_count, skipped_count, failed_count, created_by, created_at, started_at, finished_at`

// uniqueViolation is Postgres's SQLSTATE for a violated unique constraint —
// here, the partial index on (kind, subject_id) WHERE status IN
// (queued,sending), which is the double-click / two-admins guard (criterion 2).
const uniqueViolation = "23505"

// Create inserts a new queued campaign. A collision with the partial unique
// index (an active campaign already exists for this subject) maps to
// ErrAlreadyExists — the transport layer turns that into 409, never a 500.
func (r *Repository) Create(ctx context.Context, c *domain.PushCampaign) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Status == "" {
		c.Status = domain.PushCampaignQueued
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO push_campaigns
		    (id, kind, subject_id, restaurant_id, city_id, city, status,
		     force_quiet_hours, estimated_recipients, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		 RETURNING created_at`,
		c.ID, string(c.Kind), c.SubjectID, c.RestaurantID, c.CityID, c.City, string(c.Status),
		c.ForceQuietHours, c.EstimatedRecipients, c.CreatedBy,
	).Scan(&c.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return fmt.Errorf("%w: an active campaign already exists for this subject", domain.ErrAlreadyExists)
		}
		return fmt.Errorf("create push campaign: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.PushCampaign, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+campaignCols+` FROM push_campaigns WHERE id = $1`, id)
	c, err := scanCampaign(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get push campaign: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get push campaign: %w", err)
	}
	return c, nil
}

// LatestBySubjects returns the most recent campaign per subject id, using
// DISTINCT ON — the same "collapse an append-style log to its newest row per
// key" idiom postgres/consent.CurrentState uses.
func (r *Repository) LatestBySubjects(ctx context.Context, kind domain.PushCampaignKind, subjectIDs []uuid.UUID) (map[uuid.UUID]domain.PushCampaign, error) {
	out := make(map[uuid.UUID]domain.PushCampaign, len(subjectIDs))
	if len(subjectIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+campaignCols+` FROM (
		    SELECT DISTINCT ON (subject_id) `+campaignCols+`
		    FROM push_campaigns
		    WHERE kind = $1 AND subject_id = ANY($2)
		    ORDER BY subject_id, created_at DESC, id DESC
		 ) latest`, string(kind), subjectIDs)
	if err != nil {
		return nil, fmt.Errorf("latest push campaigns by subject: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, fmt.Errorf("latest push campaigns by subject: %w", err)
		}
		out[c.SubjectID] = *c
	}
	return out, rows.Err()
}

// CountToday counts campaigns created since `since` (the caller passes
// midnight of the venue-facing "today", in whatever zone the modal renders —
// BOOKING_TIMEZONE_FALLBACK) in cityID. A nil cityID counts platform-wide
// ("every city") campaigns only — `city_id IS NULL`, not "any city" — mirroring
// how nil means "everywhere" everywhere else in this feature.
func (r *Repository) CountToday(ctx context.Context, cityID *uuid.UUID, since time.Time) (int, error) {
	var n int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM push_campaigns
		  WHERE created_at >= $1 AND city_id IS NOT DISTINCT FROM $2`, since, cityID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count push campaigns today: %w", err)
	}
	return n, nil
}

// Claim implements the claim+lease pattern (criterion 10): lock candidates
// with FOR UPDATE SKIP LOCKED, then, for each locked row, either expire it in
// place (a `queued` row older than maxQueueAge, criterion 17) or hand it to
// the caller with a fresh lease. Both the SELECT and every UPDATE below MUST
// run in the same transaction the caller opened with TxManager.WithinTx — the
// row lock is what makes a second concurrent Claim() skip these exact rows;
// once this transaction commits, the LEASE (not the lock) is what keeps a
// second worker process off them while the actual send happens outside any
// transaction (same discipline as payments.Reconciler's ClaimStale callers).
func (r *Repository) Claim(ctx context.Context, now time.Time, leaseFor, maxQueueAge time.Duration, limit int) ([]domain.PushCampaign, []uuid.UUID, error) {
	q := sqltx.From(ctx, r.pool)
	rows, err := q.Query(ctx,
		`SELECT `+campaignCols+` FROM push_campaigns
		  WHERE status = 'queued'
		     OR (status = 'sending' AND lease_until <= $1 AND (next_attempt_at IS NULL OR next_attempt_at <= $1))
		  ORDER BY created_at
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("claim push campaigns: %w", err)
	}
	var candidates []domain.PushCampaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("claim push campaigns: %w", err)
		}
		candidates = append(candidates, *c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("claim push campaigns: %w", err)
	}
	rows.Close()

	var claimed []domain.PushCampaign
	var expired []uuid.UUID
	lease := now.Add(leaseFor)
	for _, c := range candidates {
		if c.Status == domain.PushCampaignQueued && now.Sub(c.CreatedAt) > maxQueueAge {
			if _, err := q.Exec(ctx,
				`UPDATE push_campaigns SET status='expired', finished_at=$2 WHERE id=$1`,
				c.ID, now); err != nil {
				return nil, nil, fmt.Errorf("expire stale push campaign: %w", err)
			}
			expired = append(expired, c.ID)
			continue
		}
		if _, err := q.Exec(ctx,
			`UPDATE push_campaigns
			    SET status='sending', lease_until=$2, started_at=COALESCE(started_at, $3)
			  WHERE id=$1`, c.ID, lease, now); err != nil {
			return nil, nil, fmt.Errorf("lease push campaign: %w", err)
		}
		c.Status = domain.PushCampaignSending
		c.LeaseUntil = &lease
		claimed = append(claimed, c)
	}
	return claimed, expired, nil
}

// Cancel is a no-op (never an error) once the campaign is already terminal —
// a concurrent Finish/Reschedule/expiry winning that race is success, not a
// conflict, exactly like the payments reconciler's CAS-then-ignore-conflict
// discipline.
func (r *Repository) Cancel(ctx context.Context, id uuid.UUID, reason domain.PushCampaignCancelReason, at time.Time) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_campaigns SET status='cancelled', cancel_reason=$2, finished_at=$3
		  WHERE id=$1 AND status IN ('queued','sending')`, id, string(reason), at)
	if err != nil {
		return fmt.Errorf("cancel push campaign: %w", err)
	}
	return nil
}

// Reschedule applies the exponential backoff after a transient send failure,
// or moves the campaign to `failed` once attempts reaches maxAttempts
// (criterion 16). Guarded to `sending` rows only: a campaign a concurrent pass
// already finished/cancelled must not be resurrected into another retry.
func (r *Repository) Reschedule(ctx context.Context, id uuid.UUID, lastError string, nextAttemptAt time.Time, maxAttempts int) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_campaigns
		    SET attempts = attempts + 1,
		        last_error = $2,
		        next_attempt_at = $3,
		        status = CASE WHEN attempts + 1 >= $4 THEN 'failed' ELSE status END,
		        finished_at = CASE WHEN attempts + 1 >= $4 THEN $5 ELSE finished_at END
		  WHERE id = $1 AND status = 'sending'`,
		id, lastError, nextAttemptAt, maxAttempts, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("reschedule push campaign: %w", err)
	}
	return nil
}

// Finish records the campaign's terminal success, counters included
// (criterion 18).
func (r *Repository) Finish(ctx context.Context, id uuid.UUID, sent, skipped, failed int, at time.Time) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_campaigns
		    SET status='done', sent_count=$2, skipped_count=$3, failed_count=$4, finished_at=$5
		  WHERE id=$1 AND status = 'sending'`, id, sent, skipped, failed, at)
	if err != nil {
		return fmt.Errorf("finish push campaign: %w", err)
	}
	return nil
}

func scanCampaign(row pgx.Row) (*domain.PushCampaign, error) {
	var c domain.PushCampaign
	var kind, status string
	var cancelReason, lastError *string
	if err := row.Scan(
		&c.ID, &kind, &c.SubjectID, &c.RestaurantID, &c.CityID, &c.City, &status, &cancelReason,
		&c.ForceQuietHours, &c.EstimatedRecipients, &c.Attempts, &c.NextAttemptAt, &c.LeaseUntil, &lastError,
		&c.SentCount, &c.SkippedCount, &c.FailedCount, &c.CreatedBy, &c.CreatedAt, &c.StartedAt, &c.FinishedAt,
	); err != nil {
		return nil, err
	}
	c.Kind = domain.PushCampaignKind(kind)
	c.Status = domain.PushCampaignStatus(status)
	if cancelReason != nil {
		reason := domain.PushCampaignCancelReason(*cancelReason)
		c.CancelReason = &reason
	}
	if lastError != nil {
		c.LastError = *lastError
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// PushCampaignRecipientRepository
// ---------------------------------------------------------------------------

// RecipientRepository implements domain.PushCampaignRecipientRepository.
type RecipientRepository struct{ pool sqltx.Querier }

// NewRecipients builds the per-guest decision repository.
func NewRecipients(pool sqltx.Querier) *RecipientRepository { return &RecipientRepository{pool: pool} }

var _ domain.PushCampaignRecipientRepository = (*RecipientRepository)(nil)

// ClaimSending inserts a `sending` row for every userID not already decided
// for this campaign and returns exactly the ones that were newly inserted —
// the at-most-once guard (criterion 14): `RETURNING user_id` on an
// `ON CONFLICT DO NOTHING` reports only the rows THIS call actually created,
// so a userID a previous, crashed attempt already claimed is silently
// excluded rather than sent to twice.
func (r *RecipientRepository) ClaimSending(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, at time.Time) ([]uuid.UUID, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`INSERT INTO push_campaign_recipients (campaign_id, user_id, status, decided_at)
		 SELECT $1, u, 'sending', $3 FROM unnest($2::uuid[]) AS u
		 ON CONFLICT (campaign_id, user_id) DO NOTHING
		 RETURNING user_id`, campaignID, userIDs, at)
	if err != nil {
		return nil, fmt.Errorf("claim sending push recipients: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("claim sending push recipients: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UnclaimSending deletes the `sending` row for each userID, but only while it
// is still exactly that — a row already resolved (by this same in-process
// retry loop, or by an earlier pass) is left alone. See the domain interface
// doc comment for the safety argument.
func (r *RecipientRepository) UnclaimSending(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM push_campaign_recipients
		  WHERE campaign_id = $1 AND user_id = ANY($2) AND status = 'sending'`,
		campaignID, userIDs)
	if err != nil {
		return fmt.Errorf("unclaim sending push recipients: %w", err)
	}
	return nil
}

// Resolve finalizes one guest's outcome. Idempotent by construction: an
// UPDATE of an already-`sent`/`failed` row is a harmless no-op write of the
// same-shaped values a retry would produce.
func (r *RecipientRepository) Resolve(ctx context.Context, campaignID, userID uuid.UUID, status domain.PushCampaignRecipientStatus, at time.Time) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE push_campaign_recipients SET status=$3, decided_at=$4
		  WHERE campaign_id=$1 AND user_id=$2`, campaignID, userID, string(status), at)
	if err != nil {
		return fmt.Errorf("resolve push recipient: %w", err)
	}
	return nil
}

// RecordSkipped writes every skipped_* decision in one batch.
// `ON CONFLICT DO NOTHING` means a userID already decided this campaign (a
// retry pass) is left exactly as it was.
func (r *RecipientRepository) RecordSkipped(ctx context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, status domain.PushCampaignRecipientStatus, at time.Time) error {
	if len(userIDs) == 0 {
		return nil
	}
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO push_campaign_recipients (campaign_id, user_id, status, decided_at)
		 SELECT $1, u, $3, $4 FROM unnest($2::uuid[]) AS u
		 ON CONFLICT (campaign_id, user_id) DO NOTHING`,
		campaignID, userIDs, string(status), at)
	if err != nil {
		return fmt.Errorf("record skipped push recipients: %w", err)
	}
	return nil
}

func (r *RecipientRepository) CountByStatus(ctx context.Context, campaignID uuid.UUID) (map[domain.PushCampaignRecipientStatus]int, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT status, count(*) FROM push_campaign_recipients WHERE campaign_id=$1 GROUP BY status`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("count push recipients by status: %w", err)
	}
	defer rows.Close()
	out := make(map[domain.PushCampaignRecipientStatus]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("count push recipients by status: %w", err)
		}
		out[domain.PushCampaignRecipientStatus(status)] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PushCampaignSubjectRepository — a UNION-style read model over events and
// promos, the same shape postgres/feed uses for the same reason (see
// domain.PushCampaignSubject's doc comment).
// ---------------------------------------------------------------------------

// SubjectRepository implements domain.PushCampaignSubjectRepository.
type SubjectRepository struct{ pool sqltx.Querier }

// NewSubjects builds the subject resolver.
func NewSubjects(pool sqltx.Querier) *SubjectRepository { return &SubjectRepository{pool: pool} }

var _ domain.PushCampaignSubjectRepository = (*SubjectRepository)(nil)

// Resolve reads the CURRENT state of one event or promo, including its
// effective city (COALESCE(subject.city_id, restaurant.city_id)) and venue
// name, in one round trip. `kind` selects which of two closed, literal SQL
// strings runs — never string-built from caller input, exactly the
// discipline postgres/feed.tableFor documents for the same shape of query.
func (r *SubjectRepository) Resolve(ctx context.Context, kind domain.PushCampaignKind, subjectID uuid.UUID) (*domain.PushCampaignSubject, error) {
	var q string
	switch kind {
	case domain.PushCampaignKindEvent:
		q = `SELECT e.id, e.restaurant_id, COALESCE(r.is_active, true), e.status,
		            e.starts_at, e.ends_at, COALESCE(e.city_id, r.city_id),
		            e.title, e.title_i18n, COALESCE(r.name, '')
		     FROM events e LEFT JOIN restaurants r ON r.id = e.restaurant_id
		     WHERE e.id = $1`
	case domain.PushCampaignKindPromo:
		q = `SELECT p.id, p.restaurant_id, COALESCE(r.is_active, true), p.status,
		            p.starts_at, p.ends_at, COALESCE(p.city_id, r.city_id),
		            p.title, p.title_i18n, COALESCE(r.name, '')
		     FROM promos p LEFT JOIN restaurants r ON r.id = p.restaurant_id
		     WHERE p.id = $1`
	default:
		return nil, fmt.Errorf("resolve push campaign subject: %w: unknown kind %q", domain.ErrValidation, kind)
	}

	var s domain.PushCampaignSubject
	s.Kind = kind
	var titleI18n []byte
	err := sqltx.From(ctx, r.pool).QueryRow(ctx, q, subjectID).Scan(
		&s.SubjectID, &s.RestaurantID, &s.RestaurantIsActive, &s.Status,
		&s.StartsAt, &s.EndsAt, &s.CityID, &s.Title, &titleI18n, &s.VenueName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve push campaign subject: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("resolve push campaign subject: %w", err)
	}
	if len(titleI18n) > 0 {
		if err := json.Unmarshal(titleI18n, &s.TitleI18n); err != nil {
			return nil, fmt.Errorf("resolve push campaign subject: decode title_i18n: %w", err)
		}
	}
	if s.CityID != nil {
		var name string
		if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
			`SELECT name FROM cities WHERE id = $1`, *s.CityID).Scan(&name); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("resolve push campaign subject: read city name: %w", err)
			}
		} else {
			s.CityName = &name
		}
	}
	return &s, nil
}

// ListIDsByRestaurant lists every subject id of `kind` at restaurantID.
// `kind` again selects a literal query string, never string-built input.
func (r *SubjectRepository) ListIDsByRestaurant(ctx context.Context, kind domain.PushCampaignKind, restaurantID uuid.UUID) ([]uuid.UUID, error) {
	table, err := subjectTable(kind)
	if err != nil {
		return nil, err
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id FROM `+table+` WHERE restaurant_id = $1`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list push campaign subject ids by restaurant: %w", err)
	}
	return scanUUIDs(rows)
}

// ListPlatformIDs lists every subject id of `kind` with no host venue.
func (r *SubjectRepository) ListPlatformIDs(ctx context.Context, kind domain.PushCampaignKind) ([]uuid.UUID, error) {
	table, err := subjectTable(kind)
	if err != nil {
		return nil, err
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id FROM `+table+` WHERE restaurant_id IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list platform push campaign subject ids: %w", err)
	}
	return scanUUIDs(rows)
}

// subjectTable maps a validated PushCampaignKind to its literal table name —
// never built from caller input (same discipline as postgres/feed.tableFor).
func subjectTable(kind domain.PushCampaignKind) (string, error) {
	switch kind {
	case domain.PushCampaignKindEvent:
		return "events", nil
	case domain.PushCampaignKindPromo:
		return "promos", nil
	default:
		return "", fmt.Errorf("push campaign subject table: %w: unknown kind %q", domain.ErrValidation, kind)
	}
}

func scanUUIDs(rows pgx.Rows) ([]uuid.UUID, error) {
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan uuid: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// LanguageRepository — batched preferred_language lookup for the text
// renderer (usecase/pushcampaigns.languageReader).
// ---------------------------------------------------------------------------

// LanguageRepository implements the usecase's languageReader port.
type LanguageRepository struct{ pool sqltx.Querier }

// NewLanguages builds the batched language reader.
func NewLanguages(pool sqltx.Querier) *LanguageRepository { return &LanguageRepository{pool: pool} }

// PreferredLanguages returns each user's preferred_language in one round
// trip. A userID with an empty preferred_language, or one absent from the
// result entirely, is left for the caller to default to Russian.
func (r *LanguageRepository) PreferredLanguages(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, preferred_language FROM users WHERE id = ANY($1)`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("read preferred languages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var lang string
		if err := rows.Scan(&id, &lang); err != nil {
			return nil, fmt.Errorf("read preferred languages: %w", err)
		}
		if lang != "" {
			out[id] = lang
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PushCampaignAudienceRepository
// ---------------------------------------------------------------------------

// AudienceRepository implements domain.PushCampaignAudienceRepository.
type AudienceRepository struct{ pool sqltx.Querier }

// NewAudience builds the audience classifier.
func NewAudience(pool sqltx.Querier) *AudienceRepository { return &AudienceRepository{pool: pool} }

var _ domain.PushCampaignAudienceRepository = (*AudienceRepository)(nil)

// Classify is the ONE query behind both the estimate breakdown and the
// worker's send decisions (spec §5.4). It resolves the city-scoped audience
// (`city_key(users.city)` against `city_aliases`, or every guest when cityID
// is nil — the platform "everywhere" subject, criterion 12) and, for each
// guest, whether their own opt-out settings allow a promo push, whether they
// are already at the daily/weekly send cap, whether they already received
// THIS subject from a past campaign, and whether they hold an active device
// token. The caller (usecase/pushcampaigns.Classify) turns these four
// booleans into exactly one status, in the fixed priority order the spec
// pins down (opt-out > cap > duplicate > no-device).
func (r *AudienceRepository) Classify(
	ctx context.Context, cityID *uuid.UUID, kind domain.PushCampaignKind, subjectID uuid.UUID,
	now time.Time, dailyCap, weeklyCap int,
) ([]domain.PushAudienceRow, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, `
		WITH audience AS (
		    SELECT u.id AS user_id
		    FROM users u
		    WHERE u.deleted_at IS NULL
		      AND ($1::uuid IS NULL
		           OR city_key(u.city) IN (SELECT alias FROM city_aliases WHERE city_id = $1))
		),
		pref AS (
		    SELECT p.user_id, p.notifications_enabled, p.push_enabled, p.promo_push_enabled
		    FROM user_notification_preferences p
		    WHERE p.user_id IN (SELECT user_id FROM audience)
		),
		capped AS (
		    SELECT r.user_id,
		           count(*) FILTER (WHERE r.decided_at >= $2) >= $4 AS hit_daily,
		           count(*) FILTER (WHERE r.decided_at >= $3) >= $5 AS hit_weekly
		    FROM push_campaign_recipients r
		    WHERE r.status = 'sent' AND r.user_id IN (SELECT user_id FROM audience)
		    GROUP BY r.user_id
		),
		dup AS (
		    SELECT DISTINCT r.user_id
		    FROM push_campaign_recipients r
		    JOIN push_campaigns c ON c.id = r.campaign_id
		    WHERE r.status = 'sent' AND c.kind = $6 AND c.subject_id = $7
		      AND r.user_id IN (SELECT user_id FROM audience)
		),
		devices AS (
		    SELECT DISTINCT d.user_id
		    FROM device_push_tokens d
		    WHERE d.is_active AND d.user_id IN (SELECT user_id FROM audience)
		)
		SELECT a.user_id,
		       COALESCE(p.notifications_enabled, true)
		           AND COALESCE(p.push_enabled, true)
		           AND COALESCE(p.promo_push_enabled, true) AS allowed,
		       COALESCE(cp.hit_daily, false) OR COALESCE(cp.hit_weekly, false) AS capped,
		       (dp.user_id IS NOT NULL) AS duplicate,
		       (dv.user_id IS NOT NULL) AS has_device
		FROM audience a
		LEFT JOIN pref p ON p.user_id = a.user_id
		LEFT JOIN capped cp ON cp.user_id = a.user_id
		LEFT JOIN dup dp ON dp.user_id = a.user_id
		LEFT JOIN devices dv ON dv.user_id = a.user_id`,
		cityID, now.Add(-24*time.Hour), now.Add(-7*24*time.Hour), dailyCap, weeklyCap, string(kind), subjectID)
	if err != nil {
		return nil, fmt.Errorf("classify push audience: %w", err)
	}
	defer rows.Close()
	var out []domain.PushAudienceRow
	for rows.Next() {
		var row domain.PushAudienceRow
		if err := rows.Scan(&row.UserID, &row.Allowed, &row.Capped, &row.Duplicate, &row.HasDevice); err != nil {
			return nil, fmt.Errorf("classify push audience: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

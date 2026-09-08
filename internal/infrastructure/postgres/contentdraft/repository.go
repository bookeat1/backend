// Package contentdraft is the Postgres implementation of
// domain.ContentDraftRepository — the content-draft review queue.
package contentdraft

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

// Repository implements domain.ContentDraftRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the content-draft repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.ContentDraftRepository = (*Repository)(nil)

const selectCols = `id, restaurant_id, kind, source, source_ref, source_url, raw_payload,
	suggested_title, suggested_title_i18n, suggested_description, suggested_description_i18n,
	suggested_starts_at, suggested_ends_at, suggested_venue, suggested_terms,
	status, reviewed_by, reviewed_at, created_event_id, created_promo_id, created_at, updated_at`

// Create inserts a new draft in pending_review. An unknown restaurant_id (FK
// violation) maps to ErrNotFound.
func (r *Repository) Create(ctx context.Context, d *domain.ContentDraft) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	payload := d.RawPayload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO content_drafts (id, restaurant_id, kind, source, source_ref, source_url, raw_payload,
			suggested_title, suggested_title_i18n, suggested_description, suggested_description_i18n,
			suggested_starts_at, suggested_ends_at, suggested_venue, suggested_terms, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'pending_review')
		 RETURNING created_at, updated_at`,
		d.ID, d.RestaurantID, d.Kind, d.Source, d.SourceRef, d.SourceURL, []byte(payload),
		d.SuggestedTitle, i18nToDB(d.SuggestedTitleI18n), d.SuggestedDescription, i18nToDB(d.SuggestedDescriptionI18n),
		d.SuggestedStartsAt, d.SuggestedEndsAt, d.SuggestedVenue, d.SuggestedTerms).
		Scan(&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return fmt.Errorf("create content draft: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("create content draft: %w", err)
	}
	d.Status = domain.DraftPendingReview
	d.RawPayload = payload
	return nil
}

// GetByID returns a draft by its id.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.ContentDraft, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+selectCols+` FROM content_drafts WHERE id = $1`, id)
	return scanDraft(row, "get content draft")
}

// ListPendingByRestaurant returns a restaurant's pending drafts oldest first
// (FIFO) with id as a stable tie-breaker. Matches idx_content_drafts_pending.
func (r *Repository) ListPendingByRestaurant(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.ContentDraft, int, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	}
	q := sqltx.From(ctx, r.pool)

	var total int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM content_drafts
		 WHERE restaurant_id = $1 AND status = 'pending_review'`,
		restaurantID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count pending drafts: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := q.Query(ctx,
		`SELECT `+selectCols+` FROM content_drafts
		 WHERE restaurant_id = $1 AND status = 'pending_review'
		 ORDER BY created_at ASC, id ASC
		 LIMIT $2 OFFSET $3`,
		restaurantID, perPage, (page-1)*perPage)
	if err != nil {
		return nil, 0, fmt.Errorf("list pending drafts: %w", err)
	}
	defer rows.Close()

	var items []domain.ContentDraft
	for rows.Next() {
		d, err := scanDraftRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan content draft: %w", err)
		}
		items = append(items, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate content drafts: %w", err)
	}
	return items, total, nil
}

// MarkApproved flips a pending draft to approved, recording the reviewer, the
// time and the created entity id. One UPDATE ... WHERE status='pending_review'
// (a CAS): a zero-rows result is disambiguated with a single follow-up read
// into ErrNotFound (absent id) vs ErrInvalidStatus (already reviewed). The
// write already happened atomically first — this read never gates the write, so
// it is not the check-then-write anti-pattern.
func (r *Repository) MarkApproved(ctx context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time, eventID, promoID *uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE content_drafts
		 SET status = 'approved', reviewed_by = $2, reviewed_at = $3,
		     created_event_id = $4, created_promo_id = $5, updated_at = now()
		 WHERE id = $1 AND status = 'pending_review'`,
		id, reviewedBy, at, eventID, promoID)
	if err != nil {
		return fmt.Errorf("approve content draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id, "approve content draft")
	}
	return nil
}

// MarkRejected flips a pending draft to rejected. Same CAS semantics as
// MarkApproved.
func (r *Repository) MarkRejected(ctx context.Context, id uuid.UUID, reviewedBy uuid.UUID, at time.Time) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE content_drafts
		 SET status = 'rejected', reviewed_by = $2, reviewed_at = $3, updated_at = now()
		 WHERE id = $1 AND status = 'pending_review'`,
		id, reviewedBy, at)
	if err != nil {
		return fmt.Errorf("reject content draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return r.classifyMiss(ctx, id, "reject content draft")
	}
	return nil
}

// classifyMiss reports whether a zero-rows CAS missed because the id is absent
// (ErrNotFound) or because the draft was no longer pending (ErrInvalidStatus).
func (r *Repository) classifyMiss(ctx context.Context, id uuid.UUID, op string) error {
	var exists bool
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM content_drafts WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("%s: classify miss: %w", op, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, domain.ErrInvalidStatus)
}

func scanDraft(row pgx.Row, op string) (*domain.ContentDraft, error) {
	d, err := scanDraftRow(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: %w", op, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}

func scanDraftRow(row pgx.Row) (*domain.ContentDraft, error) {
	var d domain.ContentDraft
	var titleI18n, descI18n, raw []byte
	if err := row.Scan(&d.ID, &d.RestaurantID, &d.Kind, &d.Source, &d.SourceRef, &d.SourceURL, &raw,
		&d.SuggestedTitle, &titleI18n, &d.SuggestedDescription, &descI18n,
		&d.SuggestedStartsAt, &d.SuggestedEndsAt, &d.SuggestedVenue, &d.SuggestedTerms,
		&d.Status, &d.ReviewedBy, &d.ReviewedAt, &d.CreatedEventID, &d.CreatedPromoID,
		&d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.SuggestedTitleI18n = i18nFromDB(titleI18n)
	d.SuggestedDescriptionI18n = i18nFromDB(descI18n)
	if len(raw) > 0 {
		d.RawPayload = json.RawMessage(raw)
	}
	return &d, nil
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

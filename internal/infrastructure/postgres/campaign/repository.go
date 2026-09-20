// Package campaign is the Postgres implementation of
// domain.CampaignLinkRepository: the QR-flyer resolver's slug lookup and hit
// counter (migration 0112).
package campaign

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

type Repository struct{ pool sqltx.Querier }

func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.CampaignLinkRepository = (*Repository)(nil)

const linkCols = `id, slug, campaign, placement, promotion_id, target_url, status, created_at`

func (r *Repository) FindBySlug(ctx context.Context, slug string) (*domain.CampaignLink, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+linkCols+` FROM campaign_links WHERE slug = $1`, slug)

	var l domain.CampaignLink
	var status string
	if err := row.Scan(&l.ID, &l.Slug, &l.Campaign, &l.Placement, &l.PromotionID,
		&l.TargetURL, &status, &l.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown slug is NOT an error — see
			// domain.CampaignLinkRepository.FindBySlug's doc.
			return nil, nil
		}
		return nil, fmt.Errorf("find campaign link by slug: %w", err)
	}
	l.Status = domain.CampaignLinkStatus(status)
	return &l, nil
}

func (r *Repository) RecordHit(ctx context.Context, linkID uuid.UUID, platform string) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO campaign_link_hits (id, link_id, platform, created_at)
		 VALUES ($1, $2, $3, now())`, uuid.New(), linkID, platform)
	if err != nil {
		return fmt.Errorf("record campaign link hit: %w", err)
	}
	return nil
}

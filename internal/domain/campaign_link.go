package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CampaignLinkStatus is the visibility of a printed QR slug. See
// CampaignLink's doc for what each value means to a scanning guest.
type CampaignLinkStatus string

const (
	// CampaignLinkActive resolves to the mini-landing and records a hit.
	CampaignLinkActive CampaignLinkStatus = "active"
	// CampaignLinkDisabled still answers 200 (never 404 — the QR is already
	// printed and out in the world) but shows "campaign is over" and records
	// no hit.
	CampaignLinkDisabled CampaignLinkStatus = "disabled"
)

// CampaignLink is one printed QR slug (`GET /m/:slug`). It exists so a
// flyer's destination can change (a Detour outage, a corrected link) with a
// plain UPDATE, never a redeploy — see migration 0112.
type CampaignLink struct {
	ID   uuid.UUID
	Slug string
	// Campaign groups links belonging to the same push (e.g.
	// "marathon-almaty-2026"). Free text, not a dictionary: campaigns are
	// seeded ad hoc by migration, not managed through an admin screen yet.
	Campaign string
	// Placement is where the QR was physically printed (flyer, stand,
	// t-shirt, …) — the `utm_content` equivalent for this campaign.
	Placement string
	// PromotionID tags the second landing button
	// (`<WEB_BASE_URL>/?promo=<PromotionID>`) and is informational only: it
	// carries NO foreign key (same posture as bookings.promotion_id,
	// migration 0004), so a promo row being retired never breaks this link.
	PromotionID uuid.UUID
	TargetURL   string
	Status      CampaignLinkStatus
	CreatedAt   time.Time
}

// CampaignLinkRepository resolves a printed slug and records a scan.
type CampaignLinkRepository interface {
	// FindBySlug returns (nil, nil) when the slug is unknown — NOT
	// ErrNotFound — so the transport layer's 404 branch stays a single,
	// obvious `if link == nil` rather than an errors.Is check shared with
	// every other failure mode.
	FindBySlug(ctx context.Context, slug string) (*CampaignLink, error)
	// RecordHit inserts one campaign_link_hits row. Called only for an
	// active link (see usecase/campaign): a disabled or unknown slug is
	// never counted as a scan.
	RecordHit(ctx context.Context, linkID uuid.UUID, platform string) error
}

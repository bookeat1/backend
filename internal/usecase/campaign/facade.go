// Package campaign is the QR-flyer resolver's application logic (spec
// marathon-remainder-plan-20260914.md, M1): look up a printed slug, decide
// whether it is a hit worth counting, and hand the transport layer enough to
// render the mini-landing.
package campaign

import (
	"context"
	"fmt"
	"strings"

	"backend-core/internal/domain"
)

// Outcome tells the transport layer which of the three responses to render.
type Outcome string

const (
	// OutcomeNotFound: the slug is not in campaign_links at all -> 404.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeDisabled: the slug exists but the campaign has ended -> 200,
	// "campaign is over" (never 404 — the QR is already printed and out in
	// the world, see domain.CampaignLinkDisabled's doc).
	OutcomeDisabled Outcome = "disabled"
	// OutcomeActive: the slug resolves and a hit was recorded -> 200,
	// mini-landing.
	OutcomeActive Outcome = "active"
)

// Facade is the resolver's whole surface: one call per scan.
type Facade interface {
	// Resolve looks up slug and, only when it is active, records a hit
	// tagged with the platform guessed from userAgent. link is non-nil for
	// OutcomeDisabled and OutcomeActive, nil for OutcomeNotFound.
	Resolve(ctx context.Context, slug, userAgent string) (Outcome, *domain.CampaignLink, error)
}

type facade struct {
	links domain.CampaignLinkRepository
}

// NewFacade builds the resolver's usecase.
func NewFacade(links domain.CampaignLinkRepository) Facade {
	return &facade{links: links}
}

func (f *facade) Resolve(ctx context.Context, slug, userAgent string) (Outcome, *domain.CampaignLink, error) {
	link, err := f.links.FindBySlug(ctx, slug)
	if err != nil {
		return "", nil, fmt.Errorf("resolve campaign link: %w", err)
	}
	if link == nil {
		return OutcomeNotFound, nil, nil
	}
	if link.Status != domain.CampaignLinkActive {
		return OutcomeDisabled, link, nil
	}
	// The hit is recorded ONLY for an active link — a disabled or unknown
	// slug is never counted as a scan (spec criterion 1).
	if err := f.links.RecordHit(ctx, link.ID, PlatformFromUserAgent(userAgent)); err != nil {
		return "", nil, fmt.Errorf("record campaign link hit: %w", err)
	}
	return OutcomeActive, link, nil
}

// PlatformFromUserAgent buckets a raw User-Agent header into "ios", "android"
// or "other". Deliberately coarse — this is a scan counter, not device
// analytics, and the spec only asks for these three buckets.
func PlatformFromUserAgent(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"),
		strings.Contains(ua, "ios"):
		return "ios"
	case strings.Contains(ua, "android"):
		return "android"
	default:
		return "other"
	}
}

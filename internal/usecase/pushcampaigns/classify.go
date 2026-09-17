package pushcampaigns

import (
	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Classify turns one guest's raw audience signals (domain.PushAudienceRow,
// produced by ONE shared SQL query — domain.PushCampaignAudienceRepository)
// into exactly one classification: eligible, or the single skip reason that
// applies. It is the ONE place the priority order lives, called identically
// by Estimate (for the breakdown counts) and Sender (for the actual send
// decision), so the two can never disagree about the same guest.
//
// Priority order (spec §4 criterion 13, pinned by its own worked example — a
// guest with NO device AND an opted-out toggle must classify as
// skipped_optout, not skipped_no_device):
//
//  1. opted out (their own notification settings say no)
//  2. capped (already at the daily/weekly send budget)
//  3. duplicate (already received THIS exact subject from a past campaign)
//  4. no device (nothing to actually send to)
//
// Because each guest resolves to exactly one bucket, the categories never
// overlap and their counts always sum to the audience size minus eligible —
// exactly what the estimate response's arithmetic promises.
func Classify(row domain.PushAudienceRow) (status domain.PushCampaignRecipientStatus, eligible bool) {
	switch {
	case !row.Allowed:
		return domain.RecipientSkippedOptOut, false
	case row.Capped:
		return domain.RecipientSkippedCap, false
	case row.Duplicate:
		return domain.RecipientSkippedDuplicate, false
	case !row.HasDevice:
		return domain.RecipientSkippedNoDevice, false
	default:
		return "", true
	}
}

// Breakdown is the full per-guest classification of one audience, computed by
// running Classify over every row once. Estimate reads only the LENGTHS
// (InCity/NoDevice/OptedOut/Capped/AlreadyReceived/Eligible — the modal never
// needs the ids); Sender reads the ID SLICES directly to write
// push_campaign_recipients and to know who to actually message. Both derive
// from the exact same Classify calls, so they can never disagree about one
// guest.
type Breakdown struct {
	InCity int

	Eligible         []uuid.UUID
	SkippedOptOut    []uuid.UUID
	SkippedCap       []uuid.UUID
	SkippedDuplicate []uuid.UUID
	SkippedNoDevice  []uuid.UUID
}

func (b Breakdown) NoDevice() int        { return len(b.SkippedNoDevice) }
func (b Breakdown) OptedOut() int        { return len(b.SkippedOptOut) }
func (b Breakdown) Capped() int          { return len(b.SkippedCap) }
func (b Breakdown) AlreadyReceived() int { return len(b.SkippedDuplicate) }
func (b Breakdown) EligibleCount() int   { return len(b.Eligible) }

// classifyAll runs Classify over every audience row and buckets the ids.
func classifyAll(rows []domain.PushAudienceRow) Breakdown {
	b := Breakdown{InCity: len(rows)}
	for _, row := range rows {
		status, eligible := Classify(row)
		if eligible {
			b.Eligible = append(b.Eligible, row.UserID)
			continue
		}
		switch status {
		case domain.RecipientSkippedOptOut:
			b.SkippedOptOut = append(b.SkippedOptOut, row.UserID)
		case domain.RecipientSkippedCap:
			b.SkippedCap = append(b.SkippedCap, row.UserID)
		case domain.RecipientSkippedDuplicate:
			b.SkippedDuplicate = append(b.SkippedDuplicate, row.UserID)
		case domain.RecipientSkippedNoDevice:
			b.SkippedNoDevice = append(b.SkippedNoDevice, row.UserID)
		}
	}
	return b
}

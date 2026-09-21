package pushcampaigns

import (
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// TestClassifyPriorityOrder pins criterion 13's worked example: a guest who is
// BOTH opted out AND has no device classifies as skipped_optout, never
// skipped_no_device — opt-out sits first in the priority chain.
func TestClassifyPriorityOrder(t *testing.T) {
	cases := []struct {
		name string
		row  domain.PushAudienceRow
		want domain.PushCampaignRecipientStatus
	}{
		{"opted out and no device -> optout wins", domain.PushAudienceRow{Allowed: false, HasDevice: false}, domain.RecipientSkippedOptOut},
		{"opted out and capped -> optout wins", domain.PushAudienceRow{Allowed: false, Capped: true}, domain.RecipientSkippedOptOut},
		{"opted out and duplicate -> optout wins", domain.PushAudienceRow{Allowed: false, Duplicate: true}, domain.RecipientSkippedOptOut},
		{"allowed, capped and duplicate -> cap wins", domain.PushAudienceRow{Allowed: true, Capped: true, Duplicate: true}, domain.RecipientSkippedCap},
		{"allowed, duplicate and no device -> duplicate wins", domain.PushAudienceRow{Allowed: true, Duplicate: true, HasDevice: false}, domain.RecipientSkippedDuplicate},
		{"allowed, not capped, not duplicate, no device -> no_device", domain.PushAudienceRow{Allowed: true, HasDevice: false}, domain.RecipientSkippedNoDevice},
		{"allowed, has device, nothing else -> eligible", domain.PushAudienceRow{Allowed: true, HasDevice: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, eligible := Classify(c.row)
			if c.want == "" {
				if !eligible {
					t.Fatalf("want eligible, got status=%q", status)
				}
				return
			}
			if eligible {
				t.Fatalf("want status=%q, got eligible", c.want)
			}
			if status != c.want {
				t.Fatalf("status = %q, want %q", status, c.want)
			}
		})
	}
}

// TestClassifyAllCategoriesDoNotOverlap pins criterion 3: five guests, one per
// category (plus one eligible), and InCity equals the sum of every bucket.
func TestClassifyAllCategoriesDoNotOverlap(t *testing.T) {
	rows := []domain.PushAudienceRow{
		{UserID: uuid.New(), Allowed: false, HasDevice: true},                 // opted out
		{UserID: uuid.New(), Allowed: true, Capped: true, HasDevice: true},    // capped
		{UserID: uuid.New(), Allowed: true, Duplicate: true, HasDevice: true}, // duplicate
		{UserID: uuid.New(), Allowed: true, HasDevice: false},                 // no device
		{UserID: uuid.New(), Allowed: true, HasDevice: true},                  // eligible
	}
	b := classifyAll(rows)
	if b.InCity != 5 {
		t.Fatalf("InCity = %d, want 5", b.InCity)
	}
	if b.OptedOut() != 1 || b.Capped() != 1 || b.AlreadyReceived() != 1 || b.NoDevice() != 1 || b.EligibleCount() != 1 {
		t.Fatalf("breakdown = %+v, want exactly one guest per category", b)
	}
	sum := b.OptedOut() + b.Capped() + b.AlreadyReceived() + b.NoDevice() + b.EligibleCount()
	if sum != b.InCity {
		t.Fatalf("categories sum to %d, want %d (InCity)", sum, b.InCity)
	}
}

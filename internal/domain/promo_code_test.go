package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNormalizePromoCodeFoldsEverySpelling is spec criterion 5: the four ways
// a guest types the marathon code must be ONE code. If this ever stops
// holding, a guest who typed the code off a poster gets "не найден" while the
// same code works for somebody who typed it off the website.
func TestNormalizePromoCodeFoldsEverySpelling(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lower case", "marathon26", "MARATHON26"},
		{"already normalized", "MARATHON26", "MARATHON26"},
		{"padded with spaces", "  marathon 26  ", "MARATHON26"},
		{"hyphenated", "marathon-26", "MARATHON26"},
		{"em dash from autocorrect", "marathon—26", "MARATHON26"},
		{"non-breaking space from a paste", "marathon 26", "MARATHON26"},
		{"underscore", "marathon_26", "MARATHON26"},
		{"mixed case and tabs", "\tMaRaThOn\t26\n", "MARATHON26"},
		{"empty", "", ""},
		// A character that is NOT a separator survives, so it can be refused
		// by ValidatePromoCode instead of being silently folded into another
		// code.
		{"foreign letter survives", "марафон26", "МАРАФОН26"},
		{"punctuation survives", "marathon.26", "MARATHON.26"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizePromoCode(tt.in); got != tt.want {
				t.Errorf("NormalizePromoCode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidatePromoCode(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		wantErr bool
	}{
		{"normalized code", "MARATHON26", false},
		{"minimum length", "AB1", false},
		{"maximum length", "A123456789012345678901234567890B", false}, // 32
		{"too short", "AB", true},
		{"too long", "A1234567890123456789012345678901B", true}, // 33
		{"lower case is not normalized", "marathon26", true},
		{"cyrillic", "МАРАФОН26", true},
		{"punctuation", "MARATHON.26", true},
		{"inner space", "MARATHON 26", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePromoCode(tt.code)
			if tt.wantErr && !errors.Is(err, ErrValidation) {
				t.Fatalf("ValidatePromoCode(%q) = %v, want ErrValidation", tt.code, err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidatePromoCode(%q) = %v, want nil", tt.code, err)
			}
		})
	}
}

func TestPromoCodeStatusTransitions(t *testing.T) {
	tests := []struct {
		from PromoCodeStatus
		to   PromoCodeStatus
		want bool
	}{
		{PromoCodeDraft, PromoCodeActive, true},
		{PromoCodeDraft, PromoCodeArchived, true},
		{PromoCodeDraft, PromoCodePaused, false},
		{PromoCodeActive, PromoCodePaused, true},
		{PromoCodePaused, PromoCodeActive, true},
		{PromoCodeActive, PromoCodeArchived, true},
		{PromoCodePaused, PromoCodeArchived, true},
		{PromoCodeActive, PromoCodeDraft, false},
		{PromoCodePaused, PromoCodeDraft, false},
		// archived is terminal — there is no way back out of it.
		{PromoCodeArchived, PromoCodeActive, false},
		{PromoCodeArchived, PromoCodePaused, false},
		{PromoCodeArchived, PromoCodeDraft, false},
		// Re-sending the current status is a no-op PATCH, not an escape.
		{PromoCodeActive, PromoCodeActive, true},
		{PromoCodeArchived, PromoCodeArchived, true},
		// Unknown statuses are refused on either side.
		{PromoCodeActive, PromoCodeStatus("deleted"), false},
		{PromoCodeStatus(""), PromoCodeActive, false},
	}
	for _, tt := range tests {
		if got := tt.from.CanTransitionTo(tt.to); got != tt.want {
			t.Errorf("%q.CanTransitionTo(%q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestPromoCodeStatusValid(t *testing.T) {
	for _, s := range PromoCodeStatuses {
		if !s.Valid() {
			t.Errorf("%q must be a valid status", s)
		}
	}
	for _, s := range []PromoCodeStatus{"", "published", "hidden", "ACTIVE"} {
		if PromoCodeStatus(s).Valid() {
			t.Errorf("%q must not be a valid status", s)
		}
	}
}

func TestPromoCodeRedeemableAt(t *testing.T) {
	start := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	base := PromoCode{Status: PromoCodeActive, StartsAt: start, ExpiresAt: end, MaxUsesPerUser: 1}

	tests := []struct {
		name   string
		status PromoCodeStatus
		at     time.Time
		want   bool
	}{
		{"active inside the window", PromoCodeActive, start.Add(time.Hour), true},
		{"active exactly at the start", PromoCodeActive, start, true},
		// The window is half-open: the expiry instant itself is already out.
		{"active exactly at the expiry", PromoCodeActive, end, false},
		{"active before the window", PromoCodeActive, start.Add(-time.Second), false},
		{"active after the window", PromoCodeActive, end.Add(time.Second), false},
		{"draft inside the window", PromoCodeDraft, start.Add(time.Hour), false},
		{"paused inside the window", PromoCodePaused, start.Add(time.Hour), false},
		{"archived inside the window", PromoCodeArchived, start.Add(time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			c.Status = tt.status
			if got := c.RedeemableAt(tt.at); got != tt.want {
				t.Errorf("RedeemableAt(%v) = %v, want %v", tt.at, got, tt.want)
			}
		})
	}

	if !base.Expired(end) || base.Expired(end.Add(-time.Second)) {
		t.Error("Expired must be true from the expiry instant on")
	}
	if !base.NotStarted(start.Add(-time.Second)) || base.NotStarted(start) {
		t.Error("NotStarted must be false from the start instant on")
	}
}

func TestPromoCodeValidate(t *testing.T) {
	start := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	valid := func() PromoCode {
		return PromoCode{
			ID:             uuid.New(),
			Code:           "MARATHON26",
			PromotionID:    uuid.New(),
			StartsAt:       start,
			ExpiresAt:      start.Add(24 * time.Hour),
			MaxUsesPerUser: 1,
			Status:         PromoCodeDraft,
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a well-formed code must validate: %v", err)
	}

	zero := 0
	negative := -1
	tests := []struct {
		name  string
		spoil func(*PromoCode)
	}{
		{"unnormalized code", func(c *PromoCode) { c.Code = "marathon 26" }},
		{"no promo behind it", func(c *PromoCode) { c.PromotionID = uuid.Nil }},
		{"unknown status", func(c *PromoCode) { c.Status = "published" }},
		{"window closes before it opens", func(c *PromoCode) { c.ExpiresAt = c.StartsAt.Add(-time.Second) }},
		{"empty window", func(c *PromoCode) { c.ExpiresAt = c.StartsAt }},
		// Zero total uses is not "switched off" — paused is.
		{"zero total limit", func(c *PromoCode) { c.MaxUsesTotal = &zero }},
		{"negative total limit", func(c *PromoCode) { c.MaxUsesTotal = &negative }},
		{"zero per-user limit", func(c *PromoCode) { c.MaxUsesPerUser = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.spoil(&c)
			if err := c.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v, want ErrValidation", err)
			}
		})
	}

	t.Run("nil total limit means unlimited", func(t *testing.T) {
		c := valid()
		c.MaxUsesTotal = nil
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})
}

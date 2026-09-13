package promocodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func codeFixture(promoID uuid.UUID, now time.Time) domain.PromoCode {
	return domain.PromoCode{
		ID: uuid.New(), Code: "MARATHON26", PromotionID: promoID,
		StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(30 * 24 * time.Hour),
		MaxUsesPerUser: 1, Status: domain.PromoCodeActive,
	}
}

func livePromo(id uuid.UUID, restaurantID *uuid.UUID, now time.Time) domain.Promo {
	return domain.Promo{
		ID: id, RestaurantID: restaurantID, Title: "Марафон Алматы",
		Terms: "Покажите бронь на финише", StartsAt: now.Add(-24 * time.Hour),
		EndsAt: now.Add(60 * 24 * time.Hour), Status: domain.PromoPublished,
	}
}

// TestResolveForBookingRefusals walks every refusal a guest can hit before the
// transaction opens. The assertion is on the MACHINE code, not the message:
// the client picks the sentence by code, and changing one is a breaking API
// change while changing a message is not.
func TestResolveForBookingRefusals(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	restaurantID, otherVenue, promoID := uuid.New(), uuid.New(), uuid.New()

	cases := []struct {
		name    string
		mutate  func(*domain.PromoCode)
		promos  func() *fakePromos
		typed   string
		want    domain.ErrorCode
		wantErr error
	}{
		{
			name: "unknown code", typed: "NOSUCHCODE",
			want: domain.CodePromoCodeNotFound, wantErr: domain.ErrNotFound,
		},
		{
			// A string that cannot be a code at all answers like an unknown
			// one: no oracle for the shape of our codes.
			name: "malformed code", typed: "??",
			want: domain.CodePromoCodeNotFound, wantErr: domain.ErrNotFound,
		},
		{
			name:   "paused code",
			mutate: func(c *domain.PromoCode) { c.Status = domain.PromoCodePaused },
			want:   domain.CodePromoCodeInactive, wantErr: domain.ErrValidation,
		},
		{
			name:   "draft code",
			mutate: func(c *domain.PromoCode) { c.Status = domain.PromoCodeDraft },
			want:   domain.CodePromoCodeInactive, wantErr: domain.ErrValidation,
		},
		{
			name:   "window not open yet",
			mutate: func(c *domain.PromoCode) { c.StartsAt = now.Add(time.Hour) },
			want:   domain.CodePromoCodeNotStarted, wantErr: domain.ErrValidation,
		},
		{
			name:   "window closed",
			mutate: func(c *domain.PromoCode) { c.ExpiresAt = now.Add(-time.Minute) },
			want:   domain.CodePromoCodeExpired, wantErr: domain.ErrValidation,
		},
		{
			// The code is alive, the campaign behind it is not: two objects,
			// two lifecycles (spec risk 5).
			name:   "campaign not live",
			promos: func() *fakePromos { return newFakePromos() },
			want:   domain.CodePromoCodeInactive, wantErr: domain.ErrValidation,
		},
		{
			name: "campaign runs at another venue",
			promos: func() *fakePromos {
				return newFakePromos(livePromo(promoID, &otherVenue, now))
			},
			want: domain.CodePromoCodeWrongVenue, wantErr: domain.ErrValidation,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := codeFixture(promoID, now)
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			promos := newFakePromos(livePromo(promoID, nil, now))
			if tc.promos != nil {
				promos = tc.promos()
			}
			f := NewFacade(newFakeCodes(c), promos, fakeUsage{}).(*facade)
			f.now = func() time.Time { return now }

			typed := tc.typed
			if typed == "" {
				typed = "marathon 26"
			}
			_, err := f.ResolveForBooking(context.Background(), typed, restaurantID, uuid.New())
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("sentinel = %v, want %v", err, tc.wantErr)
			}
			got, ok := domain.CodeOf(err)
			if !ok || got != tc.want {
				t.Fatalf("code = %q (%v), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestResolveForBookingAcceptsEverySpelling is spec criterion 5: the four
// spellings of one code are one code.
func TestResolveForBookingAcceptsEverySpelling(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	restaurantID, promoID := uuid.New(), uuid.New()
	c := codeFixture(promoID, now)
	promo := livePromo(promoID, &restaurantID, now)
	// The code stops being accepted before the campaign ends: ValidUntil must
	// be the EARLIER of the two, or a client promises a window that is shut.
	c.ExpiresAt = now.Add(10 * 24 * time.Hour)

	for _, typed := range []string{"marathon26", "MARATHON26", " marathon 26 ", "marathon-26"} {
		f := NewFacade(newFakeCodes(c), newFakePromos(promo), fakeUsage{}).(*facade)
		f.now = func() time.Time { return now }

		res, err := f.ResolveForBooking(context.Background(), typed, restaurantID, uuid.New())
		if err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
		if res.Code != "MARATHON26" || res.PromoCodeID != c.ID || res.PromotionID != promoID {
			t.Fatalf("%q resolved to %+v", typed, res)
		}
		if !res.ValidUntil.Equal(c.ExpiresAt) {
			t.Fatalf("%q: valid_until = %v, want the code's own expiry %v", typed, res.ValidUntil, c.ExpiresAt)
		}
		if res.Title != promo.Title || res.Terms != promo.Terms {
			t.Fatalf("%q: text must come from the campaign, got %q/%q", typed, res.Title, res.Terms)
		}
	}
}

// TestConsumeTxLimits is the limit rule itself. The per-guest limit wins over
// the campaign-wide one, and a guest already counted among the participants
// does not take a SECOND place when they book again.
func TestConsumeTxLimits(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	promoID, userID := uuid.New(), uuid.New()
	limit := func(n int) *int { return &n }

	cases := []struct {
		name     string
		maxTotal *int
		perUser  int
		usage    domain.PromoCodeUsage
		want     domain.ErrorCode
	}{
		{name: "first guest, no limits", perUser: 1},
		{
			name: "campaign full", maxTotal: limit(2), perUser: 1,
			usage: domain.PromoCodeUsage{DistinctUsers: 2},
			want:  domain.CodePromoCodeLimitReached,
		},
		{
			name: "this guest already joined", maxTotal: limit(100), perUser: 1,
			usage: domain.PromoCodeUsage{DistinctUsers: 5, ByUser: 1},
			want:  domain.CodePromoCodeAlreadyUsed,
		},
		{
			// max_uses_total counts GUESTS, so a returning guest inside their
			// own per-user allowance passes even at a full campaign.
			name:     "returning guest inside their allowance at a full campaign",
			maxTotal: limit(2), perUser: 2,
			usage: domain.PromoCodeUsage{DistinctUsers: 2, ByUser: 1},
		},
		{
			// Both limits are spent for this guest: the per-guest answer wins,
			// because "you already joined" is true and "the campaign is full"
			// would send them to try again later for nothing.
			name: "both limits spent", maxTotal: limit(1), perUser: 1,
			usage: domain.PromoCodeUsage{DistinctUsers: 1, ByUser: 1},
			want:  domain.CodePromoCodeAlreadyUsed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := codeFixture(promoID, now)
			c.MaxUsesTotal, c.MaxUsesPerUser = tc.maxTotal, tc.perUser
			codes := newFakeCodes(c)
			var calls []string
			codes.calls = calls
			f := NewFacade(codes, newFakePromos(livePromo(promoID, nil, now)),
				fakeUsage{usage: tc.usage, calls: &codes.calls}).(*facade)
			f.now = func() time.Time { return now }

			err := f.ConsumeTx(context.Background(), c.ID, userID)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected the booking to fit, got %v", err)
				}
			} else {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("sentinel = %v, want ErrValidation", err)
				}
				if got, _ := domain.CodeOf(err); got != tc.want {
					t.Fatalf("code = %q, want %q", got, tc.want)
				}
			}
			// The count must be taken AFTER the row lock — the other order is
			// a count nobody is holding still (ADR-047).
			if len(codes.calls) < 2 || codes.calls[0] != "lock" || codes.calls[1] != "count" {
				t.Fatalf("call order = %v, want lock before count", codes.calls)
			}
		})
	}
}

// TestConsumeTxRechecksTheWindow: an admin may pause a code between the
// pre-transaction resolve and the lock, and that pause has to stop the very
// next booking.
func TestConsumeTxRechecksTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	promoID := uuid.New()
	c := codeFixture(promoID, now)
	codes := newFakeCodes(c)
	f := NewFacade(codes, newFakePromos(livePromo(promoID, nil, now)), fakeUsage{}).(*facade)
	f.now = func() time.Time { return now }

	paused := c
	paused.Status = domain.PromoCodePaused
	if err := codes.Update(context.Background(), &paused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	err := f.ConsumeTx(context.Background(), c.ID, uuid.New())
	if got, _ := domain.CodeOf(err); got != domain.CodePromoCodeInactive {
		t.Fatalf("code = %q, want %q (err %v)", got, domain.CodePromoCodeInactive, err)
	}
}

// TestPrecheckSpendsNothing: the light endpoint answers the same verdict as
// the booking path would, and leaves no trace — no lock, no write.
func TestPrecheckReportsLimitWithoutSpending(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	restaurantID, promoID, userID := uuid.New(), uuid.New(), uuid.New()
	c := codeFixture(promoID, now)
	one := 1
	c.MaxUsesTotal = &one

	codes := newFakeCodes(c)
	f := NewFacade(codes, newFakePromos(livePromo(promoID, &restaurantID, now)),
		fakeUsage{usage: domain.PromoCodeUsage{DistinctUsers: 1}}).(*facade)
	f.now = func() time.Time { return now }

	_, err := f.Precheck(context.Background(), "MARATHON26", restaurantID, userID)
	if got, _ := domain.CodeOf(err); got != domain.CodePromoCodeLimitReached {
		t.Fatalf("code = %q, want %q (err %v)", got, domain.CodePromoCodeLimitReached, err)
	}
	if len(codes.calls) != 0 {
		t.Fatalf("precheck took a row lock (%v) — it must not", codes.calls)
	}
}

package promocodes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Precheck answers the client's "would this code work?" without spending
// anything. It runs the same checks as ResolveForBooking and then, as the one
// extra thing a guest wants to know before typing nothing else, reads the
// limits — WITHOUT a lock, because taking one outside a booking transaction
// would hold it for nothing and still guarantee nothing.
func (f *facade) Precheck(ctx context.Context, code string, restaurantID uuid.UUID, userID uuid.UUID) (*domain.PromoCodeResolution, error) {
	res, err := f.ResolveForBooking(ctx, code, restaurantID, userID)
	if err != nil {
		return nil, err
	}
	c, err := f.codes.GetByID(ctx, res.PromoCodeID)
	if err != nil {
		return nil, err
	}
	usage, err := f.bookings.CountPromoCodeUsage(ctx, res.PromoCodeID, userID)
	if err != nil {
		return nil, err
	}
	if err := checkLimits(*c, usage); err != nil {
		return nil, err
	}
	return res, nil
}

// ResolveForBooking turns the string a guest typed into the campaign a booking
// would join. Everything it checks is stable for the length of a request; the
// one thing that is not — the limit — is left to ConsumeTx under the lock.
func (f *facade) ResolveForBooking(ctx context.Context, code string, restaurantID uuid.UUID, userID uuid.UUID) (*domain.PromoCodeResolution, error) {
	normalized := domain.NormalizePromoCode(code)
	if err := domain.ValidatePromoCode(normalized); err != nil {
		// A string that cannot BE a code answers exactly like a code that does
		// not exist. Telling the difference only helps somebody probing for
		// the shape of our codes, and the guest's next action ("book without
		// it") is the same either way.
		return nil, domain.WithCode(domain.CodePromoCodeNotFound,
			fmt.Errorf("promo code %q: %w", normalized, domain.ErrNotFound))
	}
	c, err := f.codes.GetByCode(ctx, normalized)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.WithCode(domain.CodePromoCodeNotFound, err)
		}
		return nil, err
	}
	now := f.now()
	if err := checkWindow(*c, now); err != nil {
		return nil, err
	}

	promo, err := f.promos.GetPublicDetail(ctx, c.PromotionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// The campaign is not published, or its own window is closed. The
			// code may well be active — two objects, two lifecycles — so the
			// honest answer is "not being accepted", not "expired".
			return nil, domain.WithCode(domain.CodePromoCodeInactive,
				fmt.Errorf("promo code %s: campaign is not live: %w", c.Code, domain.ErrValidation))
		}
		return nil, err
	}
	// A campaign with no venue is the PLATFORM's and runs everywhere
	// (domain.Promo.RestaurantID) — only a real mismatch is refused.
	if promo.RestaurantID != nil && *promo.RestaurantID != restaurantID {
		return nil, domain.WithCode(domain.CodePromoCodeWrongVenue,
			fmt.Errorf("promo code %s belongs to another restaurant: %w", c.Code, domain.ErrValidation))
	}

	return &domain.PromoCodeResolution{
		PromoCodeID: c.ID,
		Code:        c.Code,
		PromotionID: c.PromotionID,
		Title:       promo.Title,
		TitleI18n:   promo.TitleI18n,
		Terms:       promo.Terms,
		TermsI18n:   promo.TermsI18n,
		ValidUntil:  earlier(c.ExpiresAt, promo.EndsAt),
	}, nil
}

// ConsumeTx decides whether one more booking of userID fits the code, holding
// the code's row lock while it decides.
//
// LOCK ORDER — promo_code FIRST, venue (capacity.LockVenue) SECOND. Call this
// as the first statement of the booking transaction; any path that takes the
// venue lock first deadlocks against a concurrent booking that came this way.
//
// The window and status are re-read under the lock rather than trusted from
// ResolveForBooking: the admin may have paused the code between the two, and
// "paused" has to stop the very next booking, not the one after it.
func (f *facade) ConsumeTx(ctx context.Context, promoCodeID uuid.UUID, userID uuid.UUID) error {
	c, err := f.codes.LockByID(ctx, promoCodeID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.WithCode(domain.CodePromoCodeNotFound, err)
		}
		return err
	}
	if err := checkWindow(*c, f.now()); err != nil {
		return err
	}
	usage, err := f.bookings.CountPromoCodeUsage(ctx, promoCodeID, userID)
	if err != nil {
		return err
	}
	return checkLimits(*c, usage)
}

// checkWindow maps the code's own status and acceptance window to the three
// refusals a client tells apart: "we stopped accepting it" (reversible),
// "too late" and "too early".
func checkWindow(c domain.PromoCode, now time.Time) error {
	if c.Status != domain.PromoCodeActive {
		return domain.WithCode(domain.CodePromoCodeInactive,
			fmt.Errorf("promo code %s is %s: %w", c.Code, c.Status, domain.ErrValidation))
	}
	if c.NotStarted(now) {
		return domain.WithCode(domain.CodePromoCodeNotStarted,
			fmt.Errorf("promo code %s is not accepted yet: %w", c.Code, domain.ErrValidation))
	}
	if c.Expired(now) {
		return domain.WithCode(domain.CodePromoCodeExpired,
			fmt.Errorf("promo code %s has expired: %w", c.Code, domain.ErrValidation))
	}
	return nil
}

// checkLimits is the whole limit rule, in one place so the lock-holding path
// and the lock-free precheck can never disagree about what "full" means.
//
// The per-guest limit is checked FIRST because it is the more specific
// answer: a guest who has already used up their own allowance is told so
// instead of being told the campaign is full, which would be false.
func checkLimits(c domain.PromoCode, usage domain.PromoCodeUsage) error {
	if usage.ByUser >= c.MaxUsesPerUser {
		return domain.WithCode(domain.CodePromoCodeAlreadyUsed,
			fmt.Errorf("promo code %s already used %d time(s) by this guest: %w",
				c.Code, usage.ByUser, domain.ErrValidation))
	}
	// MaxUsesTotal counts DISTINCT GUESTS, so a guest who is already among
	// them does not consume a second place by booking again — otherwise a code
	// with max_uses_total=100 and max_uses_per_user=2 would fit 50 people, not
	// 100, and nobody would be able to say why.
	if c.MaxUsesTotal != nil && usage.ByUser == 0 && usage.DistinctUsers >= *c.MaxUsesTotal {
		return domain.WithCode(domain.CodePromoCodeLimitReached,
			fmt.Errorf("promo code %s reached its limit of %d guests: %w",
				c.Code, *c.MaxUsesTotal, domain.ErrValidation))
	}
	return nil
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

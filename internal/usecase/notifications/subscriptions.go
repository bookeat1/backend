package notifications

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// SubscriptionUseCase registers and unregisters a staff member's own browser
// push subscriptions. Every mutation is scoped to the caller's user id, and a
// registration is authorized against the caller's staff membership of the
// target restaurant — a staff member can only wire push for a venue they
// actually work at (a superadmin may register for any venue).
type SubscriptionUseCase struct {
	subs     domain.PushSubscriptionRepository
	managers domain.RestaurantManagerRepository
}

// NewSubscriptionUseCase builds the subscription usecase.
func NewSubscriptionUseCase(
	subs domain.PushSubscriptionRepository,
	managers domain.RestaurantManagerRepository,
) *SubscriptionUseCase {
	return &SubscriptionUseCase{subs: subs, managers: managers}
}

// RegisterInput is a browser PushSubscription the frontend service worker
// obtained (PushSubscription.toJSON()) plus the venue it wants alerts for.
type RegisterInput struct {
	RestaurantID uuid.UUID
	Endpoint     string
	P256dh       string
	Auth         string
}

// Register stores (or refreshes) the caller's push subscription for a venue.
// It is idempotent on the endpoint: re-registering the same device overwrites
// its keys in place. isSuperadmin bypasses the staff-membership check.
func (u *SubscriptionUseCase) Register(ctx context.Context, userID uuid.UUID, isSuperadmin bool, in RegisterInput) error {
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	in.P256dh = strings.TrimSpace(in.P256dh)
	in.Auth = strings.TrimSpace(in.Auth)
	if in.RestaurantID == uuid.Nil || in.Endpoint == "" || in.P256dh == "" || in.Auth == "" {
		return fmt.Errorf("%w: restaurant_id, endpoint, p256dh and auth are required", domain.ErrValidation)
	}

	if !isSuperadmin {
		ok, err := u.isStaffOf(ctx, userID, in.RestaurantID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: not staff of this restaurant", domain.ErrForbidden)
		}
	}

	return u.subs.Upsert(ctx, &domain.PushSubscription{
		UserID:       userID,
		RestaurantID: in.RestaurantID,
		Endpoint:     in.Endpoint,
		P256dh:       in.P256dh,
		Auth:         in.Auth,
	})
}

// Unregister removes the caller's subscription by endpoint. Idempotent: an
// unknown or not-owned endpoint is a no-op success, never an error — the
// repository's user_id predicate makes it impossible to delete another user's
// subscription.
func (u *SubscriptionUseCase) Unregister(ctx context.Context, userID uuid.UUID, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("%w: endpoint is required", domain.ErrValidation)
	}
	return u.subs.DeleteByEndpointForUser(ctx, userID, endpoint)
}

func (u *SubscriptionUseCase) isStaffOf(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	memberships, err := u.managers.ListByUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("resolve staff membership: %w", err)
	}
	for _, m := range memberships {
		if m.RestaurantID == restaurantID {
			return true, nil
		}
	}
	return false, nil
}

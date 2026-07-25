package notifications

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// maxDeviceTokenLen bounds what is accepted as a push token. An Expo token
// ("ExponentPushToken[...]") is ~40 chars and an FCM registration token ~180,
// so 512 is generous headroom while still refusing a body that is trying to
// stuff a payload into the column.
const maxDeviceTokenLen = 512

// DeviceTokenUseCase registers and unregisters a signed-in GUEST's own mobile
// push tokens. Every mutation is scoped to the caller's user id — unlike the
// staff SubscriptionUseCase there is no restaurant to authorize against: a guest
// device is notified about the guest's own bookings, so owning the account IS
// the authorization.
type DeviceTokenUseCase struct {
	tokens domain.DevicePushTokenRepository
}

// NewDeviceTokenUseCase builds the guest device-token usecase.
func NewDeviceTokenUseCase(tokens domain.DevicePushTokenRepository) *DeviceTokenUseCase {
	return &DeviceTokenUseCase{tokens: tokens}
}

// RegisterDeviceInput is the token the mobile app obtained from the push
// provider plus the platform it runs on.
type RegisterDeviceInput struct {
	Token    string
	Platform domain.DevicePlatform
}

// Register stores (or re-points) the caller's device token. It is idempotent on
// the token value: re-registering an existing token moves it to the CALLING
// account and reactivates it instead of creating a second row — a device that
// changed hands (or a guest signing in on someone else's phone) must never keep
// delivering to the previous owner.
//
// The token's internal format is deliberately NOT validated beyond length: the
// provider owns that shape, and hardcoding "ExponentPushToken[...]" here would
// have to be edited the day the sender is swapped for FCM/APNs.
func (u *DeviceTokenUseCase) Register(ctx context.Context, userID uuid.UUID, in RegisterDeviceInput) (*domain.DevicePushToken, error) {
	token := strings.TrimSpace(in.Token)
	if token == "" || len(token) > maxDeviceTokenLen {
		return nil, fmt.Errorf("%w: token is required and must be at most %d characters",
			domain.ErrValidation, maxDeviceTokenLen)
	}
	if !domain.ValidDevicePlatform(in.Platform) {
		return nil, fmt.Errorf("%w: platform must be one of ios, android, web", domain.ErrValidation)
	}
	t := &domain.DevicePushToken{
		UserID:   userID,
		Token:    token,
		Platform: in.Platform,
	}
	if err := u.tokens.Upsert(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// Unregister silences one of the caller's own devices (sign-out, or the guest
// revoking the OS permission). Idempotent: an unknown or not-owned token is a
// no-op success, never an error — the repository's user_id predicate makes it
// impossible to silence another guest's device.
func (u *DeviceTokenUseCase) Unregister(ctx context.Context, userID uuid.UUID, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("%w: token is required", domain.ErrValidation)
	}
	return u.tokens.DeactivateForUser(ctx, userID, token)
}

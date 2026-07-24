package payouts

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// DestinationInput is the venue's payout destination, set by an owner/manager.
// Increment 1 supports a FreedomPay saved-card TOKEN only: BookEat never
// receives or stores a raw PAN (PCI). The token + masked identifier come from
// an out-of-band tokenization step (see the package docs / PR) — this endpoint
// only records the already-tokenized handle.
type DestinationInput struct {
	Method              domain.PayoutMethod
	Token               string
	ProviderCustomerRef string
	MaskedIdentifier    string
}

// SetDestination records where a restaurant's owed money is sent. RBAC:
// restaurant.manage (owner/manager), tenant-scoped; a hostess is forbidden. The
// PCI guard lives in domain.PayoutDestination.Validate — a raw PAN in any field
// is rejected before anything is written.
func (u *UseCase) SetDestination(ctx context.Context, actor Actor, restaurantID uuid.UUID, in DestinationInput) (*domain.PayoutDestination, error) {
	if err := u.authorizeRestaurant(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	method := in.Method
	if method == "" {
		method = domain.PayoutMethodFreedomPayCardToken
	}
	d := &domain.PayoutDestination{
		RestaurantID:        restaurantID,
		Provider:            domain.ProviderFreedomPay,
		Method:              method,
		Token:               strings.TrimSpace(in.Token),
		ProviderCustomerRef: strings.TrimSpace(in.ProviderCustomerRef),
		MaskedIdentifier:    strings.TrimSpace(in.MaskedIdentifier),
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid payout destination (a raw card number is never accepted)", domain.ErrValidation)
	}
	if err := u.destinations.Upsert(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// GetDestination returns a restaurant's payout destination. Same RBAC as
// SetDestination.
func (u *UseCase) GetDestination(ctx context.Context, actor Actor, restaurantID uuid.UUID) (*domain.PayoutDestination, error) {
	if err := u.authorizeRestaurant(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return nil, err
	}
	return u.destinations.Get(ctx, restaurantID)
}

package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// PaymentMethodsSettings is the platform panel's view of a venue's payment
// switches (migration 0116).
type PaymentMethodsSettings struct {
	// PaymentsEnabled is the venue's master switch; nil = inherits the global
	// PAYMENTS_ENABLED.
	PaymentsEnabled *bool
	// Methods are the methods switched on for the venue (kaspi, card order).
	Methods []domain.PaymentMethod
	// KaspiAccountBound reports whether the venue has an active Kaspi account
	// with a non-empty reference — without it the kaspi method stays unavailable
	// to guests even when switched on. Managed via the acquirer-account endpoints.
	KaspiAccountBound bool
}

// PaymentMethodsInput is what the platform admin writes.
type PaymentMethodsInput struct {
	PaymentsEnabled *bool
	Methods         []domain.PaymentMethod
}

func requirePlatformAdmin(actor Actor) error {
	if actor.UserID == uuid.Nil {
		return fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: payment methods are a platform action", domain.ErrForbidden)
	}
	return nil
}

// GetPaymentMethods reads a venue's payments switch and enabled methods.
// SUPERADMIN ONLY.
func (u *UseCase) GetPaymentMethods(ctx context.Context, actor Actor, restaurantID uuid.UUID) (PaymentMethodsSettings, error) {
	if err := requirePlatformAdmin(actor); err != nil {
		return PaymentMethodsSettings{}, err
	}
	o, err := u.paySettings.GetPaymentOverride(ctx, restaurantID)
	if err != nil {
		return PaymentMethodsSettings{}, err
	}
	out := PaymentMethodsSettings{PaymentsEnabled: o.PaymentsEnabled, Methods: []domain.PaymentMethod{}}
	if o.KaspiEnabled != nil && *o.KaspiEnabled {
		out.Methods = append(out.Methods, domain.MethodKaspi)
	}
	if o.CardEnabled != nil && *o.CardEnabled {
		out.Methods = append(out.Methods, domain.MethodCard)
	}
	if u.acquirerAccounts != nil {
		acc, err := u.acquirerAccounts.GetActive(ctx, domain.ProviderKaspi, restaurantID)
		switch {
		case err == nil:
			out.KaspiAccountBound = acc != nil && strings.TrimSpace(acc.AccountRef) != ""
		case errors.Is(err, domain.ErrNotFound):
		default:
			return PaymentMethodsSettings{}, err
		}
	}
	return out, nil
}

// SetPaymentMethods writes a venue's payments switch and enabled methods
// (full replace: a method absent from the list is switched off). SUPERADMIN
// ONLY. Enabling kaspi without a bound account is allowed — the method simply
// stays unavailable to guests until the account is bound.
func (u *UseCase) SetPaymentMethods(ctx context.Context, actor Actor, restaurantID uuid.UUID, in PaymentMethodsInput) (PaymentMethodsSettings, error) {
	if err := requirePlatformAdmin(actor); err != nil {
		return PaymentMethodsSettings{}, err
	}
	var kaspi, card bool
	for _, m := range in.Methods {
		switch m {
		case domain.MethodKaspi:
			kaspi = true
		case domain.MethodCard:
			card = true
		default:
			return PaymentMethodsSettings{}, fmt.Errorf("%w: unknown payment method %q", domain.ErrValidation, m)
		}
	}
	if err := u.paySettings.UpdatePaymentMethods(ctx, restaurantID, in.PaymentsEnabled, kaspi, card); err != nil {
		return PaymentMethodsSettings{}, err
	}
	return u.GetPaymentMethods(ctx, actor, restaurantID)
}

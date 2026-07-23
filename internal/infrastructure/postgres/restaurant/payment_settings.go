package restaurant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// paymentSettingsCols are the venue's payment-policy overrides (migration
// 0007), all NULLABLE — NULL means "use the global PAYMENTS_* default"
// (same convention as policyCols for the booking policy). Kept in their own
// constant, deliberately absent from cols, for the same reason policyCols is:
// adding a column here must never shift the Create/Update placeholder
// numbering above.
const paymentSettingsCols = `payments_enabled, deposit_required, deposit_amount_minor,
	preorder_payment_required, service_fee_bps, payment_provider`

// GetPaymentOverride reads one restaurant's payment-settings override. It
// implements usecase/payments' restaurantPaymentSettings port (a minimal local
// interface there, satisfied structurally — this package is never imported by
// usecase/payments, only wired to it from bootstrap/deps.go).
//
// A restaurant that does not exist reports domain.ErrNotFound rather than a
// silent zero-value override: usecase/payments.CreateForBooking would
// otherwise treat "restaurant deleted or the id was never valid" the same as
// "restaurant runs on every global default", which is not the same failure.
func (r *Repository) GetPaymentOverride(ctx context.Context, restaurantID uuid.UUID) (domain.PaymentSettingsOverride, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+paymentSettingsCols+` FROM restaurants WHERE id=$1`, restaurantID)

	var (
		paymentsEnabled *bool
		depositRequired *bool
		depositMinor    *int64
		preorderPay     *bool
		feeBps          *int
		provider        *string
	)
	err := row.Scan(&paymentsEnabled, &depositRequired, &depositMinor, &preorderPay, &feeBps, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PaymentSettingsOverride{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.PaymentSettingsOverride{}, fmt.Errorf("read payment override: %w", err)
	}

	out := domain.PaymentSettingsOverride{
		PaymentsEnabled:         paymentsEnabled,
		DepositRequired:         depositRequired,
		DepositAmountMinor:      depositMinor,
		PreorderPaymentRequired: preorderPay,
		ServiceFeeBps:           feeBps,
	}
	// Only a known, valid provider code is trusted as an override — an unknown
	// value (should never happen behind the admin panel, but this column has
	// no FK/CHECK to payment_providers) falls back to the global default
	// instead of resolving to a provider the registry cannot find, same
	// defensive posture as resolveSettings' other override fields.
	if provider != nil {
		p := domain.PaymentProvider(*provider)
		if p.Valid() {
			out.Provider = &p
		}
	}
	return out, nil
}

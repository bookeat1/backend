package payments

import (
	"time"

	"backend-core/internal/domain"
)

// paymentResponse is the public shape of domain.Payment. Deliberately excludes
// ProviderPaymentID (the acquirer's own id — an internal detail the anti-
// corruption layer keeps behind PaymentGateway) and IdempotencyKey (ours, not
// the client's concern). No card data ever reaches domain.Payment in the
// first place (spec §8), so there is nothing to redact here.
type paymentResponse struct {
	ID              string  `json:"id"`
	BookingID       string  `json:"booking_id"`
	RestaurantID    string  `json:"restaurant_id"`
	UserID          *string `json:"user_id"`
	Provider        string  `json:"provider"`
	Purpose         string  `json:"purpose"`
	Status          string  `json:"status"`
	AmountMinor     int64   `json:"amount_minor"`
	BaseAmountMinor int64   `json:"base_amount_minor"`
	FeeMinor        int64   `json:"fee_minor"`
	Currency        string  `json:"currency"`
	// PaymentURL is where the guest is redirected to actually pay. Present
	// once the acquirer answered Authorize; nil for a payment that never got
	// that far (should not normally be observable — CreateForBooking only
	// ever stores a row after a successful Authorize).
	PaymentURL     *string    `json:"payment_url"`
	AuthorizedAt   *time.Time `json:"authorized_at"`
	CapturedAt     *time.Time `json:"captured_at"`
	VoidedAt       *time.Time `json:"voided_at"`
	FailedAt       *time.Time `json:"failed_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	FailureCode    *string    `json:"failure_code"`
	FailureMessage *string    `json:"failure_message"`
	SettledAt      *time.Time `json:"settled_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func paymentToResponse(p *domain.Payment) paymentResponse {
	return paymentResponse{
		ID: p.ID.String(), BookingID: p.BookingID.String(), RestaurantID: p.RestaurantID.String(),
		UserID: idPtr(p.UserID), Provider: string(p.Provider), Purpose: string(p.Purpose),
		Status: string(p.Status), AmountMinor: p.AmountMinor, BaseAmountMinor: p.BaseAmountMinor,
		FeeMinor: p.FeeMinor, Currency: string(p.Currency), PaymentURL: p.PaymentURL,
		AuthorizedAt: p.AuthorizedAt, CapturedAt: p.CapturedAt, VoidedAt: p.VoidedAt,
		FailedAt: p.FailedAt, ExpiresAt: p.ExpiresAt, FailureCode: p.FailureCode,
		FailureMessage: p.FailureMessage, SettledAt: p.SettledAt,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// idPtr renders an optional uuid as an optional string.
func idPtr[T interface{ String() string }](v *T) *string {
	if v == nil {
		return nil
	}
	s := (*v).String()
	return &s
}

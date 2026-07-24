package payouts

import (
	"time"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/payouts"
)

// destinationRequest sets a restaurant's payout destination. A raw PAN is never
// accepted — token is a provider card token (UUID); the usecase rejects
// anything PAN-shaped.
type destinationRequest struct {
	Method              string `json:"method"`
	Token               string `json:"token"`
	ProviderCustomerRef string `json:"provider_customer_ref"`
	MaskedIdentifier    string `json:"masked_identifier"`
}

func (r destinationRequest) toInput() uc.DestinationInput {
	return uc.DestinationInput{
		Method:              domain.PayoutMethod(r.Method),
		Token:               r.Token,
		ProviderCustomerRef: r.ProviderCustomerRef,
		MaskedIdentifier:    r.MaskedIdentifier,
	}
}

type destinationResponse struct {
	RestaurantID     string    `json:"restaurant_id"`
	Provider         string    `json:"provider"`
	Method           string    `json:"method"`
	MaskedIdentifier string    `json:"masked_identifier"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// destinationToResponse deliberately OMITS the token and the provider customer
// ref: they are the address of the money and never need to leave the server in
// a read response. The masked identifier is the venue-facing hint.
func destinationToResponse(d *domain.PayoutDestination) destinationResponse {
	return destinationResponse{
		RestaurantID:     d.RestaurantID.String(),
		Provider:         string(d.Provider),
		Method:           string(d.Method),
		MaskedIdentifier: d.MaskedIdentifier,
		CreatedAt:        d.CreatedAt,
		UpdatedAt:        d.UpdatedAt,
	}
}

type payoutResponse struct {
	ID                string     `json:"id"`
	RestaurantID      string     `json:"restaurant_id"`
	AmountMinor       int64      `json:"amount_minor"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"`
	ProviderRef       *string    `json:"provider_ref,omitempty"`
	FailureReason     *string    `json:"failure_reason,omitempty"`
	NeedsManualReview bool       `json:"needs_manual_review"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	PaidAt            *time.Time `json:"paid_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

func payoutToResponse(p domain.Payout) payoutResponse {
	return payoutResponse{
		ID:                p.ID.String(),
		RestaurantID:      p.RestaurantID.String(),
		AmountMinor:       p.AmountMinor,
		Currency:          string(p.Currency),
		Status:            string(p.Status),
		ProviderRef:       p.ProviderRef,
		FailureReason:     p.FailureReason,
		NeedsManualReview: p.NeedsManualReview,
		SentAt:            p.SentAt,
		PaidAt:            p.PaidAt,
		CreatedAt:         p.CreatedAt,
	}
}

func payoutsToResponse(list []domain.Payout) []payoutResponse {
	out := make([]payoutResponse, 0, len(list))
	for _, p := range list {
		out = append(out, payoutToResponse(p))
	}
	return out
}

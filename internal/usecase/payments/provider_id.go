package payments

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// placeholderProviderID is an OPTIONAL acquirer capability. TipTopPay's
// Authorize can only return the ORDER id (POST /orders/create); the numeric
// TransactionId that /payments/confirm, /void, /refund and /get require exists
// only once the guest pays, and reaches us in the Pay/Check notification. Until
// then payments.provider_payment_id holds a placeholder that no follow-up call
// accepts. IsPlaceholderProviderID reports whether a stored id is such a
// placeholder. Same type-assertion pattern as oneStagePurpose.
type placeholderProviderID interface {
	IsPlaceholderProviderID(providerPaymentID string) bool
}

// merchantIDFinder mirrors payment.MerchantIDFinder (the usecase layer does not
// import infrastructure).
type merchantIDFinder interface {
	FindByMerchantPaymentID(ctx context.Context, merchantPaymentID string) (*domain.GatewayPayment, error)
}

func isPlaceholderProviderID(gw domain.PaymentGateway, stored *string) bool {
	g, ok := gw.(placeholderProviderID)
	if !ok || stored == nil {
		return false
	}
	return g.IsPlaceholderProviderID(*stored)
}

// adoptTransactionID replaces a placeholder provider_payment_id with the id the
// acquirer reported in a verified authorized/captured callback, so the later
// refund / void / capture addresses the real transaction. It is a no-op for
// acquirers without the placeholder capability, for events that are not a
// successful charge (a declined attempt must never overwrite the id of a good
// one), and when the stored id is already the real one. p is updated in place.
func adoptTransactionID(ctx context.Context, repo domain.PaymentRepository, gw domain.PaymentGateway, p *domain.Payment, event *domain.WebhookEvent) error {
	if event.Type != domain.WebhookPaymentAuthorized && event.Type != domain.WebhookPaymentCaptured {
		return nil
	}
	id := strings.TrimSpace(event.ProviderPaymentID)
	if id == "" || !isPlaceholderProviderID(gw, p.ProviderPaymentID) {
		return nil
	}
	if g := gw.(placeholderProviderID); g.IsPlaceholderProviderID(id) {
		return nil // the callback carries no real transaction id either
	}
	if err := repo.SetProviderPaymentID(ctx, p.ID, id); err != nil {
		return fmt.Errorf("store provider transaction id for payment %s: %w", p.ID, err)
	}
	p.ProviderPaymentID = &id
	return nil
}

// ensureTransactionID heals a payment that already sits in the database with a
// placeholder provider_payment_id (captured before adoptTransactionID existed,
// or whose callback was lost): it asks the acquirer for the transaction of OUR
// payment id (MerchantIDFinder) and stores it. A payment that is not a
// placeholder returns immediately without touching the acquirer. An acquirer
// that cannot find the transaction (the guest never paid) returns its error
// unchanged, so the caller fails loudly exactly as before.
func ensureTransactionID(ctx context.Context, repo domain.PaymentRepository, gw domain.PaymentGateway, p *domain.Payment) error {
	if !isPlaceholderProviderID(gw, p.ProviderPaymentID) {
		return nil
	}
	finder, ok := gw.(merchantIDFinder)
	if !ok {
		return nil
	}
	found, err := finder.FindByMerchantPaymentID(ctx, p.ID.String())
	if err != nil {
		return fmt.Errorf("resolve transaction id of payment %s: %w", p.ID, err)
	}
	id := strings.TrimSpace(found.ProviderPaymentID)
	if id == "" || gw.(placeholderProviderID).IsPlaceholderProviderID(id) {
		return fmt.Errorf("resolve transaction id of payment %s: acquirer returned no transaction: %w", p.ID, domain.ErrInvalidStatus)
	}
	if err := repo.SetProviderPaymentID(ctx, p.ID, id); err != nil {
		return fmt.Errorf("store provider transaction id for payment %s: %w", p.ID, err)
	}
	logging.FromContext(ctx).Info("payment.provider_transaction_id_healed",
		slog.String("payment_id", p.ID.String()), slog.String("provider", string(p.Provider)))
	p.ProviderPaymentID = &id
	return nil
}

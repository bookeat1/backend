package partnerspay

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// ErrContractUnknown is returned by every operation below. It wraps
// payment.ErrProviderNotConfigured on purpose: the transport layer and every
// usecase already know how to handle "this provider exists as a code but
// cannot be used right now" (that is exactly what an acquirer with missing
// env credentials looks like today), and "the API contract is not known yet"
// is the same caller-visible situation — nothing can be configured because
// nothing is documented. Callers must not treat this as a panic-worthy bug: an
// admin trying to route a payment through Partners Pay before it is finished
// is an expected, ordinary mistake to guard against, not a programming error.
var ErrContractUnknown = fmt.Errorf(
	"partnerspay: adapter is a scaffold, not implemented until the API contract is known: %w",
	payment.ErrProviderNotConfigured,
)

// Gateway is the Partners Pay implementation of domain.PaymentGateway.
//
// It has the same shape as freedompay.Gateway and tiptoppay.Gateway
// (cfg + shared HTTP client + logger + clock) so that filling in the real
// protocol later is a matter of writing method bodies, not redesigning the
// type. cfg and http are unused by every method today — TODO(contract): once
// Authorize is implemented for real, it will be the first method to actually
// call g.http.Do.
type Gateway struct {
	cfg  Config
	http *payment.Client
	log  *slog.Logger
	now  func() time.Time
}

var _ domain.PaymentGateway = (*Gateway)(nil)

// New builds the adapter. client may be nil, in which case a default one is
// created; tests pass an httptest-backed client the same way the other two
// adapters do, so that whoever fills in the real HTTP calls later has a
// working test harness on day one.
func New(cfg Config, client *payment.Client, log *slog.Logger) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = payment.NewClient(nil, payment.DefaultConfig(), log)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Gateway{cfg: cfg, http: client, log: log, now: time.Now}, nil
}

// Name reports the provider code this adapter serves.
func (g *Gateway) Name() domain.PaymentProvider { return domain.ProviderPartnersPay }

// ---------------------------------------------------------------------------
// domain.PaymentGateway — every method below is a scaffold
// ---------------------------------------------------------------------------

// Authorize would place a hold (or, if Partners Pay turns out to be a
// one-stage-only acquirer — see the integration questions doc — create and
// capture in a single call, which the usecase layer would need to know about
// via a different capability than this interface currently expresses).
//
// TODO(contract): everything about the real call is open — see
// docs/payments/partnerspay-integration-questions.md, groups 2 ("does card
// acquiring / a hosted payment page exist at all") and 3 ("two-stage vs
// one-stage"). Once answered, this method must:
//   - build the create-payment request from req (amount in the right unit —
//     see payment.FormatMinor/ParseMinor and group 8 on currency/units —
//     description, return/callback URLs, req.IdempotencyKey passed through
//     however Partners Pay expects idempotency to be signalled, group 5);
//   - call it via g.http.Do, exactly like freedompay.Gateway.call /
//     tiptoppay.Gateway do, so retries, logging and secret redaction come for
//     free from the shared client (internal/infrastructure/payment);
//   - translate the answer into *domain.GatewayPayment via mapPaymentStatus in
//     mapping.go, never invert the "unknown status ≠ paid" rule enforced
//     there.
func (g *Gateway) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("partnerspay: Authorize: %w", ErrContractUnknown)
}

// Capture would clear a two-stage hold. TODO(contract): confirm two-stage
// support exists at all (group 3) and whether a partial capture (less than
// the held amount) is accepted, the way FreedomPay's /g2g/clearing and
// TipTopPay's /payments/confirm both allow.
func (g *Gateway) Capture(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("partnerspay: capture amount must be positive: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("partnerspay: Capture: %w", ErrContractUnknown)
}

// Void would release a hold that was never captured. TODO(contract): confirm
// this operation exists for a two-stage payment and what it is called.
func (g *Gateway) Void(ctx context.Context, providerPaymentID string) error {
	if err := requireID(providerPaymentID); err != nil {
		return err
	}
	return fmt.Errorf("partnerspay: Void: %w", ErrContractUnknown)
}

// Refund would return money from a captured payment. TODO(contract): confirm
// partial refunds are supported (group 4) and, if they are, which field
// carries the partial amount — FreedomPay's own /g2g/refund answer to this
// exact question ("pg_amount" vs a dedicated field) was only settled by a
// sandbox run, never by reading the documentation; do not assume Partners Pay
// follows either existing adapter's convention without checking.
func (g *Gateway) Refund(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayRefund, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("partnerspay: refund amount must be positive: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("partnerspay: Refund: %w", ErrContractUnknown)
}

// Get would read Partners Pay's own view of a payment — the reconciliation
// path (spec §5), used when a webhook never arrives. TODO(contract): confirm
// the read-back endpoint, its field names, and in particular whether a
// refund already applied is reported with the sign convention FreedomPay uses
// (a NEGATIVE pg_refund_amount from the merchant's point of view — see
// docs/payments/freedompay-sandbox-checklist.md, "Defect found and fixed").
// This bit the FreedomPay adapter once already; assume nothing here without
// checking a real response.
func (g *Gateway) Get(ctx context.Context, providerPaymentID string) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("partnerspay: Get: %w", ErrContractUnknown)
}

// ---------------------------------------------------------------------------
// helpers — same validation shape as freedompay/gateway.go and
// tiptoppay/gateway.go, so real bodies can be dropped in without a redesign.
// ---------------------------------------------------------------------------

func validateAuthorize(req domain.AuthorizeRequest) error {
	switch {
	case req.PaymentID == uuid.Nil:
		return fmt.Errorf("partnerspay: authorize without a payment id: %w", domain.ErrValidation)
	case req.IdempotencyKey == "":
		return fmt.Errorf("partnerspay: authorize without an idempotency key: %w", domain.ErrValidation)
	case req.Amount.AmountMinor <= 0:
		return fmt.Errorf("partnerspay: authorize amount must be positive: %w", domain.ErrValidation)
	case !req.Amount.Currency.Valid():
		return fmt.Errorf("partnerspay: unsupported currency %q: %w", req.Amount.Currency, domain.ErrValidation)
	}
	return nil
}

func requireID(providerPaymentID string) error {
	if strings.TrimSpace(providerPaymentID) == "" {
		return fmt.Errorf("partnerspay: empty provider payment id: %w", domain.ErrValidation)
	}
	return nil
}

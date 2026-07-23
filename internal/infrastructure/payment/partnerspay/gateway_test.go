package partnerspay

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{BaseURL: DefaultBaseURL, APIKey: "k", WebhookSecret: "s"}, false},
		{"no api key", Config{BaseURL: DefaultBaseURL, WebhookSecret: "s"}, true},
		{"no webhook secret", Config{BaseURL: DefaultBaseURL, APIKey: "k"}, true},
		{"no base url", Config{APIKey: "k", WebhookSecret: "s"}, true},
		{"empty", Config{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	// New must refuse an incomplete configuration exactly like freedompay.New
	// and tiptoppay.New: this is the mechanism that keeps the adapter out of
	// bootstrap until real credentials exist (spec §8), independent of
	// whether the protocol behind it is implemented yet.
	if _, err := New(Config{}, nil, nil); err == nil {
		t.Error("New must refuse an incomplete configuration")
	}
}

func validConfig() Config {
	return Config{BaseURL: DefaultBaseURL, APIKey: "k", WebhookSecret: "s"}
}

func TestName(t *testing.T) {
	g, err := New(validConfig(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if g.Name() != domain.ProviderPartnersPay {
		t.Errorf("Name() = %s", g.Name())
	}
}

// TestEveryOperationReportsContractUnknown is the whole point of this
// scaffold: nothing must panic, and nothing must succeed by accident — every
// call must fail loudly with an error that traces back to
// payment.ErrProviderNotConfigured, which the registry / transport layer
// already know how to turn into a 404 "provider not configured" answer
// instead of a 500.
func TestEveryOperationReportsContractUnknown(t *testing.T) {
	g, err := New(validConfig(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	amount := domain.Money{AmountMinor: 10000, Currency: domain.CurrencyKZT}

	authReq := domain.AuthorizeRequest{
		PaymentID:      uuid.New(),
		BookingID:      uuid.New(),
		IdempotencyKey: "idem-1",
		Amount:         amount,
		Purpose:        domain.PurposeDeposit,
	}

	_, authErr := g.Authorize(ctx, authReq)
	if !errors.Is(authErr, ErrContractUnknown) {
		t.Errorf("Authorize() error = %v, want ErrContractUnknown", authErr)
	}
	if !errors.Is(authErr, payment.ErrProviderNotConfigured) {
		t.Error("Authorize() error must wrap payment.ErrProviderNotConfigured")
	}
	if _, err := g.Capture(ctx, "pp-1", amount); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("Capture() error = %v, want ErrContractUnknown", err)
	}
	if err := g.Void(ctx, "pp-1"); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("Void() error = %v, want ErrContractUnknown", err)
	}
	if _, err := g.Refund(ctx, "pp-1", amount); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("Refund() error = %v, want ErrContractUnknown", err)
	}
	if _, err := g.Get(ctx, "pp-1"); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("Get() error = %v, want ErrContractUnknown", err)
	}
	if _, err := g.VerifyWebhook([]byte(`{}`), nil); !errors.Is(err, ErrContractUnknown) {
		t.Errorf("VerifyWebhook() error = %v, want ErrContractUnknown", err)
	}
}

// TestOperationsRejectEmptyIDsAndAmounts checks the validation that runs
// BEFORE the ErrContractUnknown fallback — this is what makes the scaffold
// useful today rather than a pure stub: a caller passing an empty provider
// payment id or a non-positive amount gets ErrValidation, not a confusing
// "contract unknown" error about a call that was never going to be well
// formed regardless of the protocol.
func TestOperationsRejectEmptyIDsAndAmounts(t *testing.T) {
	g, err := New(validConfig(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	zero := domain.Money{AmountMinor: 0, Currency: domain.CurrencyKZT}
	amount := domain.Money{AmountMinor: 100, Currency: domain.CurrencyKZT}

	if _, err := g.Authorize(ctx, domain.AuthorizeRequest{}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Authorize(empty request) error = %v, want ErrValidation", err)
	}
	if _, err := g.Capture(ctx, "", amount); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Capture(empty id) error = %v, want ErrValidation", err)
	}
	if _, err := g.Capture(ctx, "pp-1", zero); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Capture(zero amount) error = %v, want ErrValidation", err)
	}
	if err := g.Void(ctx, ""); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Void(empty id) error = %v, want ErrValidation", err)
	}
	if _, err := g.Refund(ctx, "pp-1", zero); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Refund(zero amount) error = %v, want ErrValidation", err)
	}
	if _, err := g.Get(ctx, ""); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Get(empty id) error = %v, want ErrValidation", err)
	}
}

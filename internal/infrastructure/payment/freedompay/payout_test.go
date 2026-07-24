package freedompay

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// payoutGateway builds a PayoutGateway pointed at the fake server.
func (f *fakeGateway) payoutGateway(t *testing.T, log *slog.Logger) *PayoutGateway {
	t.Helper()
	client := payment.NewClient(f.srv.Client(), payment.Config{MaxAttempts: 3, Timeout: 100 * time.Millisecond}, log,
		payment.WithSleep(func(ctx context.Context, d time.Duration) error { return ctx.Err() }),
	)
	g, err := NewPayoutGateway(Config{
		BaseURL:          f.srv.URL,
		MerchantID:       testMerchantID,
		SecretKey:        testSecretKey,
		ResultScriptName: DefaultResultScriptName,
	}, client, log)
	if err != nil {
		t.Fatalf("NewPayoutGateway: %v", err)
	}
	g.now = func() time.Time { return time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC) }
	return g
}

func payoutRequest() domain.PayoutRequest {
	return domain.PayoutRequest{
		PayoutID:               uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		IdempotencyKey:         "payout:33333333-3333-4333-8333-333333333333",
		Amount:                 domain.KZT(700000),
		Method:                 domain.PayoutMethodFreedomPayCardToken,
		DestinationToken:       "44444444-4444-4444-8444-444444444444",
		DestinationCustomerRef: "fp-user-42",
		Description:            "BookEat settlement",
	}
}

func TestPayout_SignsAndReportsPaid(t *testing.T) {
	f := newFakeGateway(t, func(path string, _ int, params url.Values, w http.ResponseWriter) {
		if path != pathReg2Reg {
			t.Fatalf("unexpected path %s", path)
		}
		// The request must be a valid signed message addressing the card TOKEN,
		// never a raw PAN.
		if !verify("reg2reg", params, testSecretKey) {
			t.Fatal("request signature invalid")
		}
		if got := params.Get("pg_card_token_to"); got != "44444444-4444-4444-8444-444444444444" {
			t.Fatalf("card token not sent, got %q", got)
		}
		if got := params.Get("pg_user_id"); got != "fp-user-42" {
			t.Fatalf("pg_user_id not sent, got %q", got)
		}
		if got := params.Get("pg_order_id"); got != "33333333-3333-4333-8333-333333333333" {
			t.Fatalf("pg_order_id must be our payout id, got %q", got)
		}
		respond(w, "reg2reg", map[string]string{
			"pg_status":         "ok",
			"pg_payment_id":     "9000001",
			"pg_payment_status": "success",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))

	out, err := g.Payout(context.Background(), payoutRequest())
	if err != nil {
		t.Fatalf("Payout: %v", err)
	}
	if out.Status != domain.PayoutPaid {
		t.Fatalf("expected paid, got %s", out.Status)
	}
	if out.ProviderRef != "9000001" {
		t.Fatalf("expected provider ref, got %q", out.ProviderRef)
	}
}

func TestPayout_ProcessingStaysSent(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "reg2reg", map[string]string{
			"pg_status":         "ok",
			"pg_payment_id":     "9000002",
			"pg_payment_status": "process",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	out, err := g.Payout(context.Background(), payoutRequest())
	if err != nil {
		t.Fatalf("Payout: %v", err)
	}
	if out.Status != domain.PayoutSent {
		t.Fatalf("a processing payout must be reported as sent, got %s", out.Status)
	}
}

func TestPayout_ProviderErrorIsDecline(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "reg2reg", map[string]string{
			"pg_status":            "error",
			"pg_error_code":        "101",
			"pg_error_description": "insufficient balance",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	_, err := g.Payout(context.Background(), payoutRequest())
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("a provider error envelope must be a definite decline, got %v", err)
	}
}

// TestPayout_EnvelopeOkButStatusErrorIsDecline guards the reviewed bug: reg2reg
// can answer with a SUCCESSFUL envelope (pg_status=ok) but a FAILED money status
// (pg_payment_status=error, e.g. an expired card token). That is a definite
// decline and MUST surface as ErrProviderDeclined — not a nil-error
// GatewayPayout{Failed} that the usecase would swallow into `sent`.
func TestPayout_EnvelopeOkButStatusErrorIsDecline(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "reg2reg", map[string]string{
			"pg_status":            "ok", // envelope accepted…
			"pg_payment_id":        "9000009",
			"pg_payment_status":    "error", // …but the payout itself failed
			"pg_error_description": "card token expired",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	out, err := g.Payout(context.Background(), payoutRequest())
	if out != nil {
		t.Fatalf("a definite decline must not return a GatewayPayout, got %+v", out)
	}
	if !errors.Is(err, domain.ErrProviderDeclined) {
		t.Fatalf("a mapped failed status must be a definite decline, got %v", err)
	}
}

func TestPayout_UnsignedResponseIsUnknownNotPaid(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respondUnsigned(w, map[string]string{
			"pg_status":         "ok",
			"pg_payment_id":     "9000003",
			"pg_payment_status": "success",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	_, err := g.Payout(context.Background(), payoutRequest())
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("an unsigned 'paid' answer must be treated as unknown, got %v", err)
	}
}

func TestPayout_RejectsRawPANToken(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		t.Fatal("the adapter must not call the provider with a raw PAN")
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	req := payoutRequest()
	req.DestinationToken = "4400430000001234" // a PAN, not a token
	_, err := g.Payout(context.Background(), req)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a raw PAN destination must be rejected before any call, got %v", err)
	}
}

func TestGetPayout_MapsStatuses(t *testing.T) {
	cases := map[string]domain.PayoutStatus{
		"success": domain.PayoutPaid,
		"process": domain.PayoutSent,
		"error":   domain.PayoutFailed,
	}
	for word, want := range cases {
		t.Run(word, func(t *testing.T) {
			f := newFakeGateway(t, func(path string, _ int, params url.Values, w http.ResponseWriter) {
				if path != pathPayoutStatus {
					t.Fatalf("unexpected path %s", path)
				}
				if params.Get("pg_order_id") == "" {
					t.Fatal("status must be queried by our order id")
				}
				respond(w, "payout_status2", map[string]string{
					"pg_status":         "ok",
					"pg_payment_id":     "9000001",
					"pg_payment_status": word,
				})
			})
			g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
			out, err := g.GetPayout(context.Background(), "33333333-3333-4333-8333-333333333333")
			if err != nil {
				t.Fatalf("GetPayout: %v", err)
			}
			if out.Status != want {
				t.Fatalf("word %q: expected %s, got %s", word, want, out.Status)
			}
		})
	}
}

func TestGetPayout_UnknownStatusIsUnknown(t *testing.T) {
	f := newFakeGateway(t, func(_ string, _ int, _ url.Values, w http.ResponseWriter) {
		respond(w, "payout_status2", map[string]string{
			"pg_status":         "ok",
			"pg_payment_status": "some_unheard_word",
		})
	})
	g := f.payoutGateway(t, slog.New(slog.DiscardHandler))
	_, err := g.GetPayout(context.Background(), "33333333-3333-4333-8333-333333333333")
	if !errors.Is(err, domain.ErrProviderOutcomeUnknown) {
		t.Fatalf("an unmapped status must be unknown, never paid, got %v", err)
	}
}

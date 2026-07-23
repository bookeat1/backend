// Command fpay-probe drives the FreedomPay Gateway/Sync API SANDBOX through
// the real adapter (internal/infrastructure/payment/freedompay) so the
// TODO(verify) items in that package's config.go can be closed against a live
// answer instead of a guess.
//
// It NEVER reimplements the pg_sig signature or the request shape: every
// operation goes through the adapter's own exported methods (Authorize,
// Capture, Void, Refund, Get). The only thing this command adds is a
// transport-level recorder — a payment.Doer, the same seam the adapter's own
// tests use — that prints the exact bytes sent and the exact raw XML received,
// because the adapter deliberately hides provider-internal wording once it has
// translated a response into a domain.GatewayPayment.
//
// Safety:
//   - pg_testing_mode=1 is forced unconditionally; this binary refuses to
//     "verify" anything by moving real money.
//   - the secret key is read from the environment and is never printed —
//     only its length and first two characters, to confirm the right value
//     loaded.
//   - this command is its own main package under cmd/, so it never ships in
//     any production binary (cmd/http, cmd/worker, cmd/migrate, cmd/etl).
//
// Usage:
//
//	fpay-probe init
//	fpay-probe status   <payment_id>
//	fpay-probe clearing <payment_id> <amount>
//	fpay-probe cancel   <payment_id>
//	fpay-probe refund   <payment_id> <amount>
//
// Env:
//
//	FREEDOMPAY_MERCHANT_ID          required
//	FREEDOMPAY_SECRET_KEY           required
//	FREEDOMPAY_API_URL              optional, default freedompay.DefaultBaseURL
//	FREEDOMPAY_RESULT_SCRIPT_NAME   optional, default freedompay.DefaultResultScriptName
//	FPAY_PROBE_AMOUNT               optional, default "100.00" (major units, KZT)
//	FPAY_PROBE_RETURN_URL           optional, default a non-routable placeholder
//	FPAY_PROBE_CALLBACK_URL         optional, default a non-routable placeholder
//
// See docs/payments/freedompay-sandbox-checklist.md for what to run and where
// to record the answer.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
	"backend-core/internal/infrastructure/payment/freedompay"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		if len(args) == 0 {
			return 2
		}
		return 0
	}

	cfg := freedompay.ConfigFromEnv()
	if !cfg.TestingMode {
		fmt.Fprintln(os.Stderr, "note: FREEDOMPAY_TESTING_MODE was not \"true\" in the environment; forcing pg_testing_mode=1 anyway — this tool never runs live")
	}
	cfg.TestingMode = true

	printHeader(cfg)

	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 2
	}

	rec := newRecorder(os.Stdout)
	client := payment.NewClient(rec, payment.DefaultConfig(), nil)
	gw, err := freedompay.New(cfg, client, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build adapter:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, rest := args[0], args[1:]
	var runErr error
	switch cmd {
	case "init":
		runErr = runInit(ctx, gw)
	case "status":
		runErr = runStatus(ctx, gw, rest)
	case "clearing":
		runErr = runClearing(ctx, gw, rest)
	case "cancel":
		runErr = runCancel(ctx, gw, rest)
	case "refund":
		runErr = runRefund(ctx, gw, rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", cmd)
		usage()
		return 2
	}

	if runErr != nil {
		fmt.Println()
		fmt.Println("=== FAILED ===")
		fmt.Fprintln(os.Stderr, runErr)
		diagnose(runErr)
		return 1
	}
	fmt.Println()
	fmt.Println("=== OK — record the raw fields above in docs/payments/freedompay-sandbox-checklist.md ===")
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, `fpay-probe — FreedomPay sandbox integration check (pg_testing_mode=1 always)

Usage:
  fpay-probe init
  fpay-probe status   <payment_id>
  fpay-probe clearing <payment_id> <amount>
  fpay-probe cancel   <payment_id>
  fpay-probe refund   <payment_id> <amount>

<amount> is decimal major units, e.g. "100.00" (KZT).

Required env: FREEDOMPAY_MERCHANT_ID, FREEDOMPAY_SECRET_KEY
Optional env: FREEDOMPAY_API_URL, FREEDOMPAY_RESULT_SCRIPT_NAME,
              FPAY_PROBE_AMOUNT, FPAY_PROBE_RETURN_URL, FPAY_PROBE_CALLBACK_URL

See docs/payments/freedompay-sandbox-checklist.md.`)
}

// ---------------------------------------------------------------------------
// subcommands
// ---------------------------------------------------------------------------

func runInit(ctx context.Context, gw *freedompay.Gateway) error {
	amountMinor, err := probeAmountMinor()
	if err != nil {
		return fmt.Errorf("FPAY_PROBE_AMOUNT: %w", err)
	}
	amount := domain.Money{AmountMinor: amountMinor, Currency: domain.CurrencyKZT}

	req := domain.AuthorizeRequest{
		PaymentID:      uuid.New(),
		BookingID:      uuid.New(),
		IdempotencyKey: uuid.New().String(),
		Amount:         amount,
		Purpose:        domain.PurposeDeposit,
		Description:    "BookEat sandbox probe - do not charge",
		ReturnURL:      envOr("FPAY_PROBE_RETURN_URL", "https://example.invalid/fpay-probe/return"),
		CallbackURL:    envOr("FPAY_PROBE_CALLBACK_URL", "https://example.invalid/webhooks/payments/freedompay"),
	}

	fmt.Printf("\n=== init_payment: %s, payment_id=%s, booking_id=%s ===\n", amount, req.PaymentID, req.BookingID)
	fmt.Println("closes TODO(verify): pg_auto_clearing semantics, pg_lifetime, pg_redirect_url shape")

	gp, err := gw.Authorize(ctx, req)
	if err != nil {
		return fmt.Errorf("init_payment: %w", err)
	}

	fmt.Println("\n--- adapter's translated result ---")
	printGatewayPayment(gp)
	fmt.Println("\nNext: open payment_url in a browser, pay with a FreedomPay sandbox test card, then run:")
	fmt.Printf("  fpay-probe status %s\n", gp.ProviderPaymentID)
	return nil
}

func runStatus(ctx context.Context, gw *freedompay.Gateway, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: fpay-probe status <payment_id>")
	}
	fmt.Printf("\n=== status_v2: payment_id=%s ===\n", args[0])
	fmt.Println("closes TODO(verify): pg_payment_status / pg_captured vocabulary, pg_payment_date timezone")

	gp, err := gw.Get(ctx, args[0])
	if err != nil {
		return fmt.Errorf("status_v2: %w", err)
	}
	fmt.Println("\n--- adapter's translated result ---")
	printGatewayPayment(gp)
	return nil
}

func runClearing(ctx context.Context, gw *freedompay.Gateway, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: fpay-probe clearing <payment_id> <amount>")
	}
	amountMinor, err := payment.ParseMinor(args[1])
	if err != nil {
		return fmt.Errorf("amount: %w", err)
	}
	amount := domain.Money{AmountMinor: amountMinor, Currency: domain.CurrencyKZT}

	fmt.Printf("\n=== g2g/clearing: payment_id=%s amount=%s ===\n", args[0], amount)
	fmt.Println("closes TODO(verify): whether pg_amount is required for a full clearing, whether a partial clearing is honoured, and the literal pg_status_clearing / repeated-clearing behaviour")

	gp, err := gw.Capture(ctx, args[0], amount)
	if err != nil {
		return fmt.Errorf("clearing: %w", err)
	}
	fmt.Println("\n--- adapter's translated result ---")
	printGatewayPayment(gp)
	return nil
}

func runCancel(ctx context.Context, gw *freedompay.Gateway, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: fpay-probe cancel <payment_id>")
	}
	fmt.Printf("\n=== g2g/cancel: payment_id=%s ===\n", args[0])
	fmt.Println("closes TODO(verify): literal pg_revoke_status vocabulary")

	if err := gw.Void(ctx, args[0]); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	fmt.Println("\n--- adapter's translated result ---")
	fmt.Println("hold released (no error from the adapter)")
	return nil
}

func runRefund(ctx context.Context, gw *freedompay.Gateway, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: fpay-probe refund <payment_id> <amount>")
	}
	amountMinor, err := payment.ParseMinor(args[1])
	if err != nil {
		return fmt.Errorf("amount: %w", err)
	}
	amount := domain.Money{AmountMinor: amountMinor, Currency: domain.CurrencyKZT}

	fmt.Printf("\n=== g2g/refund: payment_id=%s amount=%s ===\n", args[0], amount)
	fmt.Println("closes TODO(verify): pg_amount vs pg_refund_amount for a PARTIAL refund, whether pg_currency is required, literal pg_refund_status vocabulary")
	fmt.Println("run this twice with a smaller amount than the captured total to test the partial case")

	gr, err := gw.Refund(ctx, args[0], amount)
	if err != nil {
		return fmt.Errorf("refund: %w", err)
	}
	fmt.Println("\n--- adapter's translated result ---")
	printGatewayRefund(gr)
	return nil
}

// ---------------------------------------------------------------------------
// printing
// ---------------------------------------------------------------------------

func printHeader(cfg freedompay.Config) {
	fmt.Println("=== FreedomPay sandbox probe ===")
	fmt.Println("mode: SANDBOX, pg_testing_mode=1 (forced — this tool never moves real money)")
	fmt.Printf("base_url: %s\n", cfg.BaseURL)
	fmt.Printf("merchant_id: %s\n", cfg.MerchantID)
	fmt.Printf("result_script_name: %s\n", cfg.ResultScriptName)
	fmt.Printf("secret_key: %s\n", maskSecret(cfg.SecretKey))
	fmt.Println("------------------------------------------------------------")
}

// maskSecret never returns anything that lets the value be reconstructed —
// only its length and its first two characters, enough to tell "the right
// variable loaded" from "empty / truncated / pasted the wrong thing".
func maskSecret(s string) string {
	if s == "" {
		return "(empty)"
	}
	prefix := s
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return fmt.Sprintf("length=%d prefix=%q (rest masked)", len(s), prefix)
}

func printGatewayPayment(gp *domain.GatewayPayment) {
	fmt.Printf("provider_payment_id (pg_payment_id): %s\n", gp.ProviderPaymentID)
	fmt.Printf("status:                              %s\n", gp.Status)
	fmt.Printf("amount:                              %s\n", gp.Amount)
	if gp.PaymentURL != "" {
		fmt.Printf("payment_url (pg_redirect_url):       %s\n", gp.PaymentURL)
	}
	if gp.AuthorizedAt != nil {
		fmt.Printf("authorized_at:                       %s\n", gp.AuthorizedAt.Format(time.RFC3339))
	}
	if gp.CapturedAt != nil {
		fmt.Printf("captured_at:                         %s\n", gp.CapturedAt.Format(time.RFC3339))
	}
	if gp.FailureCode != "" {
		fmt.Printf("failure_code:                        %s\n", gp.FailureCode)
		fmt.Printf("failure_message:                     %s\n", gp.FailureMessage)
	}
	fmt.Printf("raw (redacted, adapter-level):       %s\n", string(gp.Raw))
}

func printGatewayRefund(gr *domain.GatewayRefund) {
	fmt.Printf("provider_refund_id: %s\n", gr.ProviderRefundID)
	fmt.Printf("status:             %s\n", gr.Status)
	fmt.Printf("amount:             %s\n", gr.Amount)
	if gr.FailureCode != "" {
		fmt.Printf("failure_code:       %s\n", gr.FailureCode)
		fmt.Printf("failure_message:    %s\n", gr.FailureMessage)
	}
	fmt.Printf("raw (redacted, adapter-level): %s\n", string(gr.Raw))
}

// diagnose prints extra context for the well-known transport-level sentinels
// so a network blip is not confused with a rejected request.
func diagnose(err error) {
	switch {
	case errors.Is(err, payment.ErrProviderUnavailable):
		fmt.Fprintln(os.Stderr, "diagnosis: network/timeout/5xx — FreedomPay's sandbox did not answer within the retry budget. Check FREEDOMPAY_API_URL and connectivity; the payment status is UNKNOWN, not \"failed\".")
	case errors.Is(err, payment.ErrProviderRejected):
		fmt.Fprintln(os.Stderr, "diagnosis: the sandbox answered but rejected the request (HTTP 4xx or pg_status=error/pg_error_code). Check the request parameters printed above against the error text.")
	case errors.Is(err, payment.ErrProviderMalformed):
		fmt.Fprintln(os.Stderr, "diagnosis: the answer could not be parsed as signed XML, or its pg_sig did not verify against FREEDOMPAY_SECRET_KEY — double-check the secret key and the raw XML printed above.")
	default:
		fmt.Fprintln(os.Stderr, "diagnosis: unclassified error, see the raw request/response above.")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func probeAmountMinor() (int64, error) {
	return payment.ParseMinor(envOr("FPAY_PROBE_AMOUNT", "100.00"))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

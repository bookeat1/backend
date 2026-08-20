// Package tiptoppay adapts the TipTop Pay acquirer (https://tiptoppay.kz) to
// domain.PaymentGateway.
//
// Everything below is checked against https://developers.tiptoppay.kz (fetched
// 2026-07-21). Facts this adapter relies on:
//
//   - API root https://api.tiptoppay.kz, HTTP Basic Auth with Public ID as the
//     login and API Secret as the password;
//   - idempotency via the X-Request-ID header; a repeated id replays the stored
//     result for one hour;
//   - two-stage flow: POST /orders/create with RequireConfirmation=true creates
//     a hosted payment page, POST /payments/confirm captures, POST
//     /payments/void releases the hold, POST /payments/refund returns money,
//     POST /payments/get and POST /v2/payments/find read state;
//   - every answer is the envelope {"Success":bool,"Message":string|null,
//     "Model":{…}};
//   - transaction statuses are AwaitingAuthentication / Authorized / Completed /
//     Cancelled / Declined;
//   - notifications (check, pay, fail, confirm, refund, cancel) carry
//     X-Content-HMAC and Content-HMAC: base64(HMAC-SHA256(body, API Secret)),
//     UTF-8. The two differ only in URL encoding of the signed bytes.
//
// # Why the hosted order flow and not /payments/cards/auth
//
// /payments/cards/auth needs a CardCryptogramPacket, i.e. card data collected
// by a payment widget. domain.AuthorizeRequest carries none, deliberately — the
// domain must not learn what a cryptogram is, and this service must stay out of
// PCI DSS scope. /orders/create is the server-to-server equivalent: it returns
// a payment page URL, which is exactly what domain.GatewayPayment.PaymentURL
// is for.
//
// # The two identifiers
//
// A TipTopPay order and a TipTopPay transaction are different things.
// Authorize returns the ORDER id (a short opaque string). The numeric
// TransactionId only exists once the guest has actually paid, and it arrives in
// the pay/auth notification together with our own InvoiceId. So:
//
//   - Payment.ProviderPaymentID starts as the order id and is replaced by the
//     transaction id when the first webhook resolves the payment by
//     WebhookEvent.MerchantPaymentID;
//   - Capture / Void / Refund / Get require the numeric transaction id and
//     reject an order id with a clear error — there is nothing to capture
//     before the guest has paid;
//   - FindByMerchantPaymentID covers the gap for the reconciliation worker.
package tiptoppay

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultBaseURL is the TipTop Pay API root.
const DefaultBaseURL = "https://api.tiptoppay.kz"

// Config is the adapter's credentials and endpoints.
//
// PublicID and APISecret are secrets: they are read from the environment only
// (spec §8), are never persisted, and never appear in a log line or an error —
// see TestErrorsAndLogsCarryNoSecrets.
type Config struct {
	BaseURL   string // TIPTOPPAY_API_URL, defaults to DefaultBaseURL
	PublicID  string // TIPTOPPAY_PUBLIC_ID   (Basic auth login)
	APISecret string // TIPTOPPAY_API_SECRET  (Basic auth password + webhook HMAC key)
	// TestMode only affects the description we send; TipTopPay decides test vs
	// live by the terminal the Public ID belongs to.
	TestMode bool // TIPTOPPAY_TEST_MODE

	// SplitsEnabled says this terminal is set up for split payments
	// («Сплитование платежей» — it is switched on by TipTop Pay's manager on
	// request, it is not something an integration can turn on for itself).
	//
	// It is a hard switch, not a hint. With it OFF the adapter REFUSES an
	// AuthorizeRequest that carries splits instead of dropping them: a request
	// whose Splits are silently ignored still charges the guest the right
	// amount and pays it all to the marketplace terminal, i.e. the venue's
	// money lands on ours. It also decides whether Capture/Refund read the
	// original shares back before confirming or returning money — the one
	// extra request per money movement is only worth making on a terminal that
	// can have splits at all.
	SplitsEnabled bool // TIPTOPPAY_SPLITS_ENABLED

	// SplitViaOrders permits sending the Splits array on /orders/create, the
	// hosted-order method this adapter's Authorize uses.
	//
	// THIS IS AN UNVERIFIED GUESS AND IT DEFAULTS TO FALSE. TipTop Pay's
	// documentation lists Splits on /payments/cards/{charge,auth} and
	// /payments/tokens/{charge,auth} only; the /orders/create parameter table
	// does not mention it. An unknown field may be accepted, or ignored — and
	// "ignored" means every split payment quietly pays the platform the venue's
	// share, which is the single most expensive way this integration can fail.
	// So splits through the order flow stay off until somebody confirms them on
	// the sandbox.
	//
	// TODO(verify): проверить на песочнице — принимает ли /orders/create массив
	// Splits и действительно ли деньги расходятся по саб мерчам (а не молча
	// уходят на терминал маркетплейса). См. docs/payments/tiptoppay-splits.md.
	SplitViaOrders bool // TIPTOPPAY_SPLIT_VIA_ORDERS
}

// ConfigFromEnv reads the adapter's configuration from the environment. It is
// the only place credentials enter the process; bootstrap must not pass them
// through the database or a request (spec §8).
func ConfigFromEnv() Config {
	return Config{
		BaseURL:   envOr("TIPTOPPAY_API_URL", DefaultBaseURL),
		PublicID:  os.Getenv("TIPTOPPAY_PUBLIC_ID"),
		APISecret: os.Getenv("TIPTOPPAY_API_SECRET"),
		TestMode:  strings.EqualFold(os.Getenv("TIPTOPPAY_TEST_MODE"), "true"),

		SplitsEnabled:  strings.EqualFold(os.Getenv("TIPTOPPAY_SPLITS_ENABLED"), "true"),
		SplitViaOrders: strings.EqualFold(os.Getenv("TIPTOPPAY_SPLIT_VIA_ORDERS"), "true"),
	}
}

// Validate reports whether the adapter can be wired at all. bootstrap should
// skip the adapter (and leave the provider unconfigured in the registry) rather
// than start with half a credential.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.PublicID) == "" {
		missing = append(missing, "TIPTOPPAY_PUBLIC_ID")
	}
	if strings.TrimSpace(c.APISecret) == "" {
		missing = append(missing, "TIPTOPPAY_API_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("tiptoppay: missing %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("tiptoppay: empty base URL")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

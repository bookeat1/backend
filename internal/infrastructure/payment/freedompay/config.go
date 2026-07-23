// Package freedompay adapts the Freedom Pay acquirer (https://freedompay.kz,
// the former PayBox gateway) to domain.PaymentGateway.
//
// # What is confirmed by documentation
//
// Checked against https://docs.freedompay.kz on 2026-07-21 (Gateway API → Sync
// API → Purchase):
//
//   - API root https://api.freedompay.kz; requests are GET or POST with
//     form-data / x-www-form-urlencoded; responses are XML;
//   - signature: MD5 over "script;<fields sorted by name>;secret_key", hex
//     lowercase, in pg_sig, with a random pg_salt participating (see
//     signature.go);
//   - POST /init_payment creates a payment and answers pg_status, pg_payment_id,
//     pg_redirect_url; it accepts pg_idempotency_key, pg_order_id, pg_amount,
//     pg_currency, pg_description, pg_result_url, pg_success_url,
//     pg_failure_url, pg_request_method, pg_user_phone, pg_user_contact_email,
//     pg_testing_mode, pg_auto_clearing, pg_lifetime, and arbitrary merchant
//     parameters whose names do NOT start with "pg_";
//   - POST /g2g/clearing captures a two-stage hold (pg_payment_id, pg_amount →
//     pg_status_clearing, pg_clearing_amount);
//   - POST /g2g/cancel releases a two-stage hold before the funds are taken
//     (pg_payment_id → pg_revoke_status, pg_payment_revoke_id);
//   - POST /g2g/refund returns money from an already cleared payment
//     (pg_payment_id, pg_amount → pg_refund_status, pg_payment_refund_id); it
//     explicitly does NOT work on an uncleared two-stage payment;
//   - POST /g2g/status_v2 reads state and answers pg_payment_status,
//     pg_captured, pg_amount, pg_clearing_amount, pg_refund_amount,
//     pg_payment_date;
//   - a two-stage payment that receives neither clearing nor cancellation
//     within 5 DAYS is cleared automatically;
//   - the result callback (result_url) is a form POST carrying pg_order_id,
//     pg_payment_id, pg_result, pg_amount, pg_currency, pg_captured,
//     pg_can_reject, pg_payment_date, pg_salt, pg_sig; the merchant answers XML
//     <response><pg_status>ok|rejected|error</pg_status>…</response>, itself
//     signed;
//   - the callback is retried every 30 minutes for 2 hours until it gets a 200,
//     so the same event WILL arrive more than once.
//
// # What is NOT confirmed and must be checked on the sandbox
//
// docs.freedompay.kz renders its request-parameter tables client-side, so the
// per-field semantics below could not be read from the documentation. Every one
// of them is marked TODO(verify) at the place it is used:
//
//   - pg_auto_clearing: which value selects the two-stage flow (this adapter
//     sends 0, meaning "do not clear automatically");
//   - the exact value set of pg_payment_status and of pg_revoke_status /
//     pg_refund_status / pg_status_clearing;
//   - whether /g2g/refund takes pg_amount or a separate pg_refund_amount for a
//     partial refund;
//   - whether pg_currency is required on /g2g/refund;
//   - whether pg_lifetime bounds the payment link only or the hold as well.
//
// Nothing in this package pretends those are facts.
package freedompay

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultBaseURL is the Freedom Pay API root for Kazakhstan.
const DefaultBaseURL = "https://api.freedompay.kz"

// DefaultResultScriptName is the last path segment of our webhook route
// (POST /webhooks/payments/freedompay). It is part of the callback signature,
// so it MUST match the route the gateway actually calls.
const DefaultResultScriptName = "freedompay"

// Config is the adapter's credentials and endpoints. SecretKey is a secret: it
// comes from the environment only (spec §8) and never reaches a log line, an
// error or the database.
type Config struct {
	BaseURL    string // FREEDOMPAY_API_URL, defaults to DefaultBaseURL
	MerchantID string // FREEDOMPAY_MERCHANT_ID → pg_merchant_id
	SecretKey  string // FREEDOMPAY_SECRET_KEY  → signature key
	// ResultScriptName is the last segment of the result_url we register with
	// FreedomPay. The callback's signature is computed over OUR script name,
	// not over a FreedomPay endpoint, so getting this wrong makes every
	// callback fail verification.
	ResultScriptName string // FREEDOMPAY_RESULT_SCRIPT_NAME
	// TestingMode sends pg_testing_mode=1: the gateway simulates payments
	// without moving money.
	TestingMode bool // FREEDOMPAY_TESTING_MODE
}

// ConfigFromEnv reads the adapter's configuration from the environment.
func ConfigFromEnv() Config {
	return Config{
		BaseURL:          envOr("FREEDOMPAY_API_URL", DefaultBaseURL),
		MerchantID:       strings.TrimSpace(os.Getenv("FREEDOMPAY_MERCHANT_ID")),
		SecretKey:        os.Getenv("FREEDOMPAY_SECRET_KEY"),
		ResultScriptName: envOr("FREEDOMPAY_RESULT_SCRIPT_NAME", DefaultResultScriptName),
		TestingMode:      strings.EqualFold(os.Getenv("FREEDOMPAY_TESTING_MODE"), "true"),
	}
}

// Validate reports whether the adapter can be wired. bootstrap should leave the
// provider unconfigured rather than start with half a credential.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.MerchantID) == "" {
		missing = append(missing, "FREEDOMPAY_MERCHANT_ID")
	}
	if strings.TrimSpace(c.SecretKey) == "" {
		missing = append(missing, "FREEDOMPAY_SECRET_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("freedompay: missing %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("freedompay: empty base URL")
	}
	if strings.TrimSpace(c.ResultScriptName) == "" {
		return errors.New("freedompay: empty result script name")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

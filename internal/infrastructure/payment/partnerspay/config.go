// Package partnerspay is a TEMPLATE adapter for Partners Pay
// (https://partnerspay.co), a Kazakhstan fintech startup, behind
// domain.PaymentGateway.
//
// # What is confirmed by documentation
//
// Nothing. As of 2026-07-22 we have no API documentation, no sandbox access
// and no signed contract with Partners Pay — only the public marketing page
// (https://partnerspay.co/) and its Google Play listing. This package is
// deliberately honest about that: every method compiles, satisfies the
// interface, and returns ErrContractUnknown instead of guessing a protocol.
//
// What the marketing page DOES say (not a protocol, but worth recording so
// nobody re-discovers it from scratch):
//
//   - Partners Pay's core product is "мгновенные выплаты" (instant payouts) to
//     cards and business accounts for a contingent workforce (couriers,
//     drivers, freelancers, self-employed contractors), plus electronic
//     document workflow with them (contracts signed via eGov Mobile EDS) and
//     automated accounting;
//   - target customers explicitly include "HoReCa" alongside aggregators,
//     taxi/delivery services and marketplaces;
//   - an "Open API" is advertised for fintech startups / SaaS / ERP platforms
//     to embed the payment infrastructure, but no endpoint, auth scheme or
//     payload shape is published;
//   - licensed payment organisation, National Bank of Kazakhstan licence
//     No. 02-24-196 (since May 2024).
//
// This strongly suggests Partners Pay's product may be built around
// *payouts to contractors* (relevant to BookEat for restaurant settlement /
// mass payouts) rather than *consumer card acquiring* (a hosted checkout page
// for a guest's deposit or pre-order) — see
// docs/payments/partnerspay-integration-questions.md, question group 1, which
// asks this outright. Do not assume this adapter's Authorize/Capture/Void/
// Refund/Get shape (built for card acquiring, mirroring freedompay and
// tiptoppay) is even the right shape for what Partners Pay actually offers
// until that question is answered.
//
// # What is NOT known and must come from Partners Pay directly
//
// See docs/payments/partnerspay-integration-questions.md for the full,
// grouped list. In short: whether card acquiring / a hosted payment page
// exists at all; two-stage (hold/capture/void) vs one-stage only; partial
// refunds; idempotency of payment creation; webhook shape, signature scheme
// and retry behaviour; sandbox environment and test cards; the status
// vocabulary; currency/units (integer tiyn, assumed — see
// internal/domain/payment_money.go — or something else); timestamp timezone
// (FreedomPay's own sandbox run turned up a field in local Almaty time with
// no offset right next to one in UTC — see
// docs/payments/freedompay-sandbox-checklist.md — so this is not a
// theoretical trap); rate limits and pricing; payment splitting / mass
// payouts (their advertised strength, relevant to restaurant settlement); and
// fiscalisation (receipts).
package partnerspay

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultBaseURL is a PLACEHOLDER, not a confirmed API root.
//
// TODO(contract): replace with the real API host once Partners Pay provides
// it. "https://partnerspay.co" is the marketing site, not necessarily (and
// probably not) the API root — providers in this repo already show two
// different patterns (api.freedompay.kz vs api.tiptoppay.kz, both distinct
// from the marketing domain).
const DefaultBaseURL = "https://partnerspay.co"

// Config is the adapter's credentials and endpoints. Every secret field comes
// from the environment only (spec §8) and must never reach a log line, an
// error message or the database — same discipline as Config in
// freedompay/config.go and tiptoppay/config.go.
//
// The field names below are a reasonable starting guess (an API URL, some
// form of API key, a webhook secret, a testing-mode flag — every acquirer
// adapter in this repo needs at least that much) rather than confirmed
// Partners Pay field names. TODO(contract): confirm the actual auth scheme —
// it could be a bearer API key, HTTP Basic like TipTopPay, or a merchant
// id + secret pair signing each request like FreedomPay's pg_sig. Whichever
// it is, this Config and ConfigFromEnv must be reshaped to match; do not bend
// Partners Pay's scheme to fit these field names.
type Config struct {
	// BaseURL is PARTNERSPAY_API_URL, defaulting to DefaultBaseURL.
	BaseURL string
	// APIKey is PARTNERSPAY_API_KEY. TODO(contract): confirm whether
	// authentication is a single bearer key, a public/secret key pair, or a
	// merchant id + signing secret. Named generically until then.
	APIKey string
	// WebhookSecret is PARTNERSPAY_WEBHOOK_SECRET, the key used to verify an
	// incoming callback's signature. TODO(contract): confirm this is even a
	// shared secret (vs. e.g. an asymmetric signature or mTLS) and what
	// algorithm it feeds — see webhook.go.
	WebhookSecret string
	// TestingMode selects a sandbox/test flow. TODO(contract): confirm
	// whether Partners Pay distinguishes test vs live by a request flag (like
	// FreedomPay's pg_testing_mode) or purely by which credentials/terminal
	// were issued (like TipTopPay's TestMode, which only changes what we send
	// but not how the gateway decides).
	TestingMode bool // PARTNERSPAY_TESTING_MODE
}

// ConfigFromEnv reads the adapter's configuration from the environment. It is
// the only place credentials enter the process; bootstrap must not pass them
// through the database or a request (spec §8).
func ConfigFromEnv() Config {
	return Config{
		BaseURL:       envOr("PARTNERSPAY_API_URL", DefaultBaseURL),
		APIKey:        os.Getenv("PARTNERSPAY_API_KEY"),
		WebhookSecret: os.Getenv("PARTNERSPAY_WEBHOOK_SECRET"),
		TestingMode:   strings.EqualFold(os.Getenv("PARTNERSPAY_TESTING_MODE"), "true"),
	}
}

// Validate reports whether the adapter can be wired at all. bootstrap must
// skip the adapter (and leave the provider unconfigured in the registry)
// rather than start with half a credential — exactly like freedompay and
// tiptoppay. Until real credentials exist, PARTNERSPAY_API_KEY and
// PARTNERSPAY_WEBHOOK_SECRET are unset in every environment, so this adapter
// never rises in bootstrap today; that is the intended state, not a bug.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.APIKey) == "" {
		missing = append(missing, "PARTNERSPAY_API_KEY")
	}
	if strings.TrimSpace(c.WebhookSecret) == "" {
		missing = append(missing, "PARTNERSPAY_WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("partnerspay: missing %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("partnerspay: empty base URL")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

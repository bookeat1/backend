// Package kaspi is the adapter for Kaspi Pay behind domain.PaymentGateway.
//
// # What this adapter actually talks to
//
// NOT Kaspi directly. It talks to OUR OWN multi-tenant Kaspi service
// (kaspi-pos-automation, https://kaspi.book-eat.com), which holds the cashier
// sessions, drives the real Kaspi endpoints, polls every payment to a terminal
// status and fans the result out as a signed webhook. That service is the
// anti-corruption layer for Kaspi's own protocol; this package is the
// anti-corruption layer for that service.
//
// # The money flow, and why it is one-stage
//
// Kaspi Pay has no hold/capture. The guest opens a pay.kaspi.kz link and the
// money moves in one step, or it does not move at all. That does not fit the
// two-stage vocabulary of domain.PaymentGateway, so the mapping is explicit
// and is the single most important thing to understand here:
//
//	Authorize → POST /api/qr/create        — creates a payment LINK. No money
//	                                          moves. The payment is `created`.
//	(guest pays the link)
//	webhook payment.success → WebhookPaymentAuthorized — the money HAS moved.
//	Capture  → GET /api/qr/status          — READ-ONLY confirmation that the
//	                                          money really moved. It moves
//	                                          nothing itself; it exists so the
//	                                          usecase's authorized → captured
//	                                          step has something truthful to
//	                                          ask.
//	Void     → GET /api/qr/status          — a link that was never paid needs
//	                                          no release (it simply expires);
//	                                          a PAID one cannot be voided and
//	                                          this refuses rather than quietly
//	                                          refunding somebody's money.
//	Refund   → POST /api/refund/create     — the only call here that moves
//	                                          money backwards.
//
// Mapping success onto WebhookPaymentAuthorized (rather than …Captured) is
// deliberate: `created → captured` is not a legal transition in the payment
// state machine, while `created → authorized → captured` is, and for a
// pre-order the usecase layer already captures immediately on authorization
// (domain.PaymentPurpose.CapturesImmediately). Going through the front door
// keeps the ledger, the idempotency and the cancel-settlement logic identical
// to every other acquirer.
//
// # Multi-tenancy
//
// The Kaspi service is multi-company: every venue's money belongs to a company
// there, addressed by an integer company id and authorised by that company's
// own X-Api-Key. Which company a venue's money goes to is stored in
// restaurant_split_accounts (provider 'kaspi', account_ref = company id) and
// arrives here as domain.AuthorizeRequest.MerchantAccountRef; the KEY for that
// company comes from the environment only, never from the database.
//
// # No sandbox
//
// Kaspi has no test environment. Every successful /api/qr/create is a real
// payable link and every /api/refund/create moves real money. There is
// nothing in this package that can be exercised against a sandbox — the tests
// run against an httptest server that imitates the service's shapes.
package kaspi

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultBaseURL is the deployed multi-tenant Kaspi service.
const DefaultBaseURL = "https://kaspi.book-eat.com"

// Config is the adapter's endpoints and credentials. Every secret comes from
// the environment only (spec §8) and never reaches a log line, an error
// message or the database.
type Config struct {
	// BaseURL is KASPI_API_URL, defaulting to DefaultBaseURL.
	BaseURL string
	// BasicAuthUser / BasicAuthPassword are KASPI_BASIC_AUTH_USER and
	// KASPI_BASIC_AUTH_PASSWORD: the service sits behind Caddy basic auth on
	// everything except its own /webhooks* paths. Optional — a deployment
	// that reaches the service on a private network needs neither.
	BasicAuthUser     string
	BasicAuthPassword string
	// CompanyAPIKeys maps a Kaspi-service company id to that company's
	// X-Api-Key. Read from KASPI_COMPANY_API_KEYS as
	// "1=kpk_...,2=kpk_...". One key per company is the whole point: a key
	// authorises everything that company can do, so venues that settle to
	// different companies must never share one.
	CompanyAPIKeys map[string]string
	// WebhookSecrets are the HMAC secrets of the webhook subscriptions the
	// Kaspi service delivers to us, read from KASPI_WEBHOOK_SECRETS in the
	// same "1=secret,2=secret" shape.
	//
	// A callback does NOT say which company it came from, so verification
	// tries every configured secret (constant-time each) and accepts the
	// first that matches. That is the same shape as key rotation and is safe:
	// a body that matches ANY secret we configured was signed by a company we
	// configured. A bare value with no "=" is accepted as a single shared
	// secret, for a deployment with exactly one company.
	WebhookSecrets map[string]string
}

// ConfigFromEnv reads the adapter's configuration from the environment. It is
// the only place these credentials enter the process.
func ConfigFromEnv() Config {
	return Config{
		BaseURL:           envOr("KASPI_API_URL", DefaultBaseURL),
		BasicAuthUser:     strings.TrimSpace(os.Getenv("KASPI_BASIC_AUTH_USER")),
		BasicAuthPassword: os.Getenv("KASPI_BASIC_AUTH_PASSWORD"),
		CompanyAPIKeys:    parsePairs(os.Getenv("KASPI_COMPANY_API_KEYS")),
		WebhookSecrets:    parsePairs(os.Getenv("KASPI_WEBHOOK_SECRETS")),
	}
}

// Validate reports whether the adapter can be wired at all. bootstrap must
// skip the adapter (leaving the provider unconfigured in the registry) rather
// than start with half a credential, exactly like every other adapter here.
func (c Config) Validate() error {
	var missing []string
	if len(c.CompanyAPIKeys) == 0 {
		missing = append(missing, "KASPI_COMPANY_API_KEYS")
	}
	if len(c.WebhookSecrets) == 0 {
		missing = append(missing, "KASPI_WEBHOOK_SECRETS")
	}
	if len(missing) > 0 {
		return fmt.Errorf("kaspi: missing %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("kaspi: empty base URL")
	}
	return nil
}

// apiKeyFor resolves the X-Api-Key of one company. An unknown company is a
// configuration mistake an operator can fix (a venue was mapped to a company
// whose key was never put in env), so it is an ordinary error — and it names
// the company id, never the key.
func (c Config) apiKeyFor(companyID string) (string, error) {
	key, ok := c.CompanyAPIKeys[strings.TrimSpace(companyID)]
	if !ok || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("kaspi: no API key configured for company %q (KASPI_COMPANY_API_KEYS)", companyID)
	}
	return key, nil
}

// parsePairs reads "id=value,id=value" into a map. A bare value with no "="
// becomes the entry "" → value: a single shared secret for a single-company
// deployment. Whitespace around entries is ignored; the value is NOT trimmed
// of anything but surrounding whitespace, because a secret is opaque.
func parsePairs(raw string) map[string]string {
	out := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, value, ok := strings.Cut(entry, "=")
		if !ok {
			out[""] = entry
			continue
		}
		id, value = strings.TrimSpace(id), strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out[id] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

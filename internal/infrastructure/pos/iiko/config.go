// Package iiko adapts the iiko point-of-sale system (https://iiko.ru, the
// iikoCloud / iikoTransport API) to domain.POSConnector. It is the FIRST POS
// BookEat integrates.
//
// # What is confirmed
//
// iiko granted BookEat test access. That is the extent of what is settled:
// credentials for the test environment are PENDING and the concrete
// iikoTransport contract we will use has not been pinned down yet. This package
// is deliberately honest about that — every method compiles, satisfies
// domain.POSConnector, and returns ErrNotWired instead of guessing a protocol,
// the same discipline as infrastructure/payment/partnerspay.
//
// What is publicly known about iikoTransport (recorded so nobody re-discovers
// it from scratch, NOT relied on as a verified contract):
//
//   - auth is a two-step exchange: POST /api/1/access_token with an apiLogin
//     returns a bearer token, sent as Authorization: Bearer on every subsequent
//     call;
//   - an organisation is addressed by its organizationId (a UUID), obtained via
//     /api/1/organizations; a venue's tables/sections come from
//     /api/1/reserve/available_restaurant_sections and terminal groups from
//     /api/1/terminal_groups;
//   - reservations are created via the /api/1/reserve/* family and webhooks are
//     configured per organisation.
//
// # What is NOT known and must come before this adapter does anything real
//
//   - the exact test-environment apiLogin/token and organizationId (PENDING);
//   - the precise reserve-create request/response shape and its status
//     vocabulary, and how a party size / specific table is expressed;
//   - the occupancy read: whether table occupancy is polled
//     (/api/1/reserve/... or an order/table-status endpoint) or only pushed via
//     webhook, which decides how FetchOccupancy is implemented;
//   - the webhook transport, its event set, and the signature/verification
//     scheme — until that is known, VerifyWebhook must NOT interpret a payload;
//   - idempotency: how iiko de-duplicates a retried reserve-create, so
//     PushBooking's IdempotencyKey maps to the right field.
//
// Nothing in this package pretends any of the above is a fact.
package iiko

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultBaseURL is the iikoTransport API root. TODO(verify): confirm the test
// vs production host with iiko once credentials are issued — the production
// host below is the documented public one, but the test environment may differ.
const DefaultBaseURL = "https://api-ru.iiko.services"

// Config is the adapter's credentials and endpoints. APILogin is a secret: it
// comes from the environment only and never reaches a log line, an error or the
// database — same discipline as the acquirer adapters' Config
// (payment/freedompay, payment/tiptoppay).
//
// TODO(verify): the field set below mirrors the two-step iikoTransport auth
// (apiLogin → bearer token) plus a webhook secret, which is the reasonable
// starting shape. Confirm against the real test credentials once supplied and
// reshape if iiko's scheme differs; do not bend iiko's scheme to fit these
// names.
type Config struct {
	// BaseURL is IIKO_API_URL, defaulting to DefaultBaseURL.
	BaseURL string
	// APILogin is IIKO_API_LOGIN — the apiLogin exchanged at
	// /api/1/access_token for a bearer token. Secret.
	APILogin string
	// OrganizationID is IIKO_ORGANIZATION_ID — the iiko organisation this
	// integration acts on. Not secret, but environment-specific.
	OrganizationID string
	// WebhookSecret is IIKO_WEBHOOK_SECRET, the key used to verify an incoming
	// callback's signature. TODO(verify): confirm iiko signs webhooks with a
	// shared secret and what algorithm it feeds — see connector.go VerifyWebhook.
	WebhookSecret string
}

// ConfigFromEnv reads the adapter's configuration from the environment. It is
// the only place credentials enter the process; bootstrap must not pass them
// through the database or a request.
func ConfigFromEnv() Config {
	return Config{
		BaseURL:        envOr("IIKO_API_URL", DefaultBaseURL),
		APILogin:       os.Getenv("IIKO_API_LOGIN"),
		OrganizationID: strings.TrimSpace(os.Getenv("IIKO_ORGANIZATION_ID")),
		WebhookSecret:  os.Getenv("IIKO_WEBHOOK_SECRET"),
	}
}

// Validate reports whether the adapter can be wired at all. bootstrap should
// skip the adapter (leaving the POS unconfigured in the registry) rather than
// start with half a credential — exactly like the acquirer adapters. Until iiko
// supplies test credentials, IIKO_API_LOGIN and IIKO_ORGANIZATION_ID are unset
// in every environment, so this adapter never rises in bootstrap today; that is
// the intended state, not a bug.
func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.APILogin) == "" {
		missing = append(missing, "IIKO_API_LOGIN")
	}
	if strings.TrimSpace(c.OrganizationID) == "" {
		missing = append(missing, "IIKO_ORGANIZATION_ID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("iiko: missing %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("iiko: empty base URL")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

package domain

import "errors"

// Sentinel errors returned by usecases and repositories. The transport layer
// maps these to HTTP status codes in response.HandleError. Wrap with
// fmt.Errorf("...: %w", err) so callers can still match them via errors.Is.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrForbidden     = errors.New("forbidden")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrInvalidStatus = errors.New("invalid status transition")
	ErrValidation    = errors.New("validation failed")

	// ErrProviderOutcomeUnknown marks an acquirer call whose result could not
	// be observed: a timeout, a 5xx that survived every retry, or a response
	// that could not be parsed. The money-moving action (capture, void,
	// refund) may or may not have happened at the provider — callers MUST NOT
	// retry the same acquirer call blindly on this error; they may only read
	// the acquirer's own status (PaymentGateway.Get) or wait for a webhook.
	// See report item #1 / #4 (payments review, 2026-07-23).
	ErrProviderOutcomeUnknown = errors.New("acquirer outcome unknown, needs reconciliation")
	// ErrProviderDeclined marks an acquirer call that was answered with an
	// explicit, well-formed refusal (a 4xx, or a provider error envelope like
	// FreedomPay's pg_status=error / TipTopPay's Success=false). Unlike
	// ErrProviderOutcomeUnknown, this IS a definite "no": safe to record as a
	// terminal failure without further reconciliation.
	ErrProviderDeclined = errors.New("acquirer declined the request")
)

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

// ErrorCode is the stable, machine-readable name of a failure. It exists
// because the HTTP status alone is ambiguous: two entirely different outcomes
// can share one status (409 "the slot was taken by someone else" vs 409 "you
// replayed an Idempotency-Key with another body") and a client that has only
// the status cannot tell a guest whether a booking exists or not. The message
// text is NOT a contract — it is human-readable and may be reworded at any
// time; the code is the contract. The transport layer emits it as the "code"
// field of the response envelope.
type ErrorCode string

// Generic codes, one per sentinel above. They are the default when a caller
// did not attach a more specific code.
const (
	CodeNotFound      ErrorCode = "not_found"
	CodeAlreadyExists ErrorCode = "already_exists"
	CodeForbidden     ErrorCode = "forbidden"
	CodeUnauthorized  ErrorCode = "unauthorized"
	CodeValidation    ErrorCode = "validation_failed"
	CodeInvalidStatus ErrorCode = "invalid_status"
	CodeInternal      ErrorCode = "internal_error"
)

// Specific codes. Each one narrows a sentinel that would otherwise be
// indistinguishable on the wire.
const (
	// CodeSlotTaken — the requested table/slot was taken by somebody else
	// between the availability check and the write (the exclusion constraint
	// fired). NOTHING was booked for this caller: the client must NOT tell the
	// guest to look for a reservation, it must offer another time.
	CodeSlotTaken ErrorCode = "slot_taken"

	// CodeNoTableAvailable — no table fits the party at that time. Same family
	// as CodeSlotTaken (nothing was booked) but detected before the write, so
	// it is a plain "no availability", not a race.
	CodeNoTableAvailable ErrorCode = "no_table_available"

	// CodeIdempotencyKeyReused — the same Idempotency-Key was replayed with a
	// DIFFERENT request body. This one really does mean "your earlier submit
	// went through": the stored response belongs to the first body, and the
	// client must either reuse that response or retry with a fresh key.
	CodeIdempotencyKeyReused ErrorCode = "idempotency_key_reused"
)

// codedError attaches an ErrorCode to an error without hiding it: Unwrap keeps
// errors.Is(err, ErrAlreadyExists) working, so status mapping and every
// existing caller are unaffected.
type codedError struct {
	code ErrorCode
	err  error
}

func (e *codedError) Error() string        { return e.err.Error() }
func (e *codedError) Unwrap() error        { return e.err }
func (e *codedError) ErrorCode() ErrorCode { return e.code }

// WithCode tags err with a machine-readable code. Returns nil for a nil err so
// it can be used inline in a return.
func WithCode(code ErrorCode, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// CodeOf reports the code carried by err or anything it wraps. ok is false when
// no code was attached — the caller then falls back to the sentinel's generic
// code.
func CodeOf(err error) (code ErrorCode, ok bool) {
	var c interface{ ErrorCode() ErrorCode }
	if errors.As(err, &c) {
		return c.ErrorCode(), true
	}
	return "", false
}

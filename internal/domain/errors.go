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

	// ErrUnavailable marks a request the service understood and accepted but
	// cannot serve right now because an OPTIONAL dependency is missing or
	// unhealthy — a provider key that has not been provisioned yet, a provider
	// that timed out, a provider that rate-limited us. It is deliberately not
	// ErrInternal: nothing is broken in our code and the caller did nothing
	// wrong, so it maps to 503 (retry later) instead of a 500 with a stack.
	// Clients branch on the attached ErrorCode to tell the cases apart.
	ErrUnavailable = errors.New("temporarily unavailable")

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
	CodeUnavailable   ErrorCode = "unavailable"
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

	// Capacity-mode switch (PATCH the venue's booking policy). Both of these are
	// STAFF-facing, in the venue cabinet, and both mean "nothing was changed" —
	// they are separated from the generic codes for the same reason CodeSlotTaken
	// is: on the wire a 409 reads as "that already exists", which is not what
	// either of them means, and staff cannot act on it.

	// CodeCapacitySwitchConflict — the switch lost a race and was rolled back
	// whole: another transaction was changing the status of one of the venue's
	// bookings while the switch tried to commit, so the deferred trigger of
	// migration 0059 refused rather than commit occupancy for a booking whose
	// fate was undecided. NOTHING was changed and nothing is wrong with the
	// venue's data — the honest message is "try again", and a retry a moment
	// later almost always succeeds.
	CodeCapacitySwitchConflict ErrorCode = "capacity_switch_conflict"

	// CodeCapacitySwitchTooManyBookings — the venue has more live bookings in the
	// affected period than one switch may reconcile (MaxReconcileBookings), so
	// the whole set could not be read as a whole and the switch was refused
	// before touching anything. Unlike CodeCapacitySwitchConflict this does NOT
	// resolve by retrying immediately: it needs fewer live bookings in the
	// window, or support. The cabinet should say so instead of showing a
	// conflict.
	CodeCapacitySwitchTooManyBookings ErrorCode = "capacity_switch_too_many_bookings"

	// Static map proxy (GET /api/v1/restaurants/:id/map). All four mean "there
	// is no picture for you right now" and the app renders its existing no-map
	// placeholder for each; they differ in whose problem it is and whether a
	// retry can help, which the status alone cannot express.

	// CodeMapNoCoordinates — the restaurant exists but has no (or an
	// out-of-range) latitude/longitude on file. 404 and permanent until
	// somebody fills the venue's coordinates in: retrying will not help.
	CodeMapNoCoordinates ErrorCode = "map_no_coordinates"

	// CodeMapNotConfigured — no map provider key is provisioned on this
	// deployment. Our own missing configuration, not an outage: EVERY caller
	// gets it until the key is set, so a client should stop asking for the rest
	// of the session instead of retrying on every screen open.
	CodeMapNotConfigured ErrorCode = "map_not_configured"

	// CodeMapProviderRateLimited — the provider answered 429. Transient, and
	// specifically a signal for ops that the plan's quota is being hit.
	CodeMapProviderRateLimited ErrorCode = "map_provider_rate_limited"

	// CodeMapProviderUnavailable — the provider timed out, was unreachable,
	// refused our key, or answered with something that is not an image.
	// Transient: a later retry may succeed.
	CodeMapProviderUnavailable ErrorCode = "map_provider_unavailable"
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

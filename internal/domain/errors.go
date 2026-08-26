package domain

import (
	"errors"
	"time"
)

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

	// Phone-OTP login (POST /auth/otp/request, POST /auth/otp/verify). All of
	// these keep the status and the message they have always had; the code is
	// what lets the app say ONE true sentence instead of listing possibilities.
	//
	// # What an outsider may learn from them
	//
	// These endpoints are unauthenticated and a phone number is not a secret,
	// so every code below was chosen against one question: does it tell someone
	// who does not own the number something they could not already observe?
	//
	// Nothing here reveals whether a phone is REGISTERED — OTP login creates the
	// user on first success, so registered and unknown numbers behave
	// identically all the way through.

	// CodeOTPInvalidPhone — the phone could not be normalized into a number we
	// can send to. Pure input validation, no lookup happens, so it is the same
	// answer for every caller and leaks nothing. It is the one 422 the app can
	// point at the phone field instead of the timer.
	CodeOTPInvalidPhone ErrorCode = "otp_invalid_phone"

	// CodeOTPCodeRequired — the verify request carried no code at all.
	// Client-side bug or an empty submit; again no lookup, nothing leaked.
	CodeOTPCodeRequired ErrorCode = "otp_code_required"

	// CodeOTPRateLimitedMinute — this NUMBER already requested a code within
	// the last minute. Carries Retry-After.
	//
	// It does tell the caller that somebody asked for a code for this number
	// in the last minute — but that bit is already on the wire today and always
	// has been: a rate-limited request answers 422 while an accepted one
	// answers 200. Splitting the two windows apart adds no new observation, it
	// only stops the app from saying "подождите" when the truth is "номер
	// написан неправильно".
	CodeOTPRateLimitedMinute ErrorCode = "otp_rate_limited_minute"

	// CodeOTPRateLimitedHour — this NUMBER has spent its hourly budget of code
	// requests. Distinct from the per-minute one because the guest's next
	// action differs: waiting a minute helps, here it does not. Carries
	// Retry-After.
	CodeOTPRateLimitedHour ErrorCode = "otp_rate_limited_hour"

	// CodeOTPInvalid — the submitted code was not accepted. It DELIBERATELY
	// covers three situations at once: the code is wrong, the code expired, and
	// there is no active code for this number at all (never requested, already
	// used, or invalidated).
	//
	// They are merged, not merged by accident. Separating them turns
	// /auth/otp/verify into a presence oracle: one request with a random code
	// would answer "wrong code" exactly when a live code exists for that number
	// and "no active code" otherwise, letting anyone watch a number and learn
	// the moment its owner starts logging in — which is precisely the timing an
	// OTP phishing call needs. Nobody outside the login flow gets that bit here.
	//
	// The guest loses little: the only useful action is the same in all three
	// cases — check the digits, or request a new code — and the app knows from
	// its own state when it last requested one.
	CodeOTPInvalid ErrorCode = "otp_invalid"

	// CodeOTPTooManyAttempts — the code was guessed wrong too many times and is
	// now dead. Kept separate from CodeOTPInvalid because the action differs and
	// retyping cannot succeed any more: the guest must request a new code.
	//
	// Residual leak, accepted knowingly: reaching this state requires enough
	// wrong guesses to burn a LIVE code, so a prober learns "a code existed" —
	// at the price of destroying the very code they would want alive, and of
	// more requests than the strict per-IP rate limit passes in a minute.
	CodeOTPTooManyAttempts ErrorCode = "otp_too_many_attempts"

	// CodeCatalogVenueStateUnavailable — the catalog was asked to filter by
	// open_now / accepts_online_bookings, but the server-computed venue state
	// behind those fields could not be read for this request. 503, and
	// deliberately NOT an unfiltered 200: answering a filtered query with the
	// whole catalog is the silent lie the filter exists to remove. The client
	// should retry, or fall back to browsing without the filter — never
	// present the result as filtered.
	CodeCatalogVenueStateUnavailable ErrorCode = "catalog_venue_state_unavailable"
	// CodeCatalogAvailabilityUnavailable — the catalog was asked to filter by
	// free tables on a date and the booking engine could not answer. Same rule
	// as above and a separate code so the client can tell the guest WHICH
	// filter it has to drop: serving the unfiltered catalog under a "на двоих в
	// пятницу" query would send people to venues that have no table.
	CodeCatalogAvailabilityUnavailable ErrorCode = "catalog_availability_unavailable"

	// CodeCityRequired — a city-scoped guest listing (today: the main-screen
	// feed) was called without a city, or with one the platform does not serve.
	// The status and the message are exactly what they have always been; what is
	// new is that the app can now tell this 422 apart from every other 422 on
	// the same screen and react by asking the guest to pick a city, instead of
	// rendering "что-то пошло не так" on a first launch that simply has not
	// chosen one yet. The city itself stays required on the feed: a rail of
	// Astana offers shown to a guest in Almaty is worse than an empty rail.
	CodeCityRequired ErrorCode = "city_required"

	// CodeMenuTopPicksLimit — the venue tried to mark a dish as «Лучшие
	// позиции» while all MenuTopPickLimit slots of its storefront rail are
	// already taken. A separate 422 code because the panel must say "снимите
	// одно из отмеченных блюд" and not "validation failed": nothing about the
	// request is malformed, the shelf is simply full.
	CodeMenuTopPicksLimit ErrorCode = "menu_top_picks_limit"

	// CodeVenueTimezoneInvalid — the timezone offered for (or already stored
	// against) a venue is not a usable IANA zone name. Kept apart from the
	// generic validation code because it is the one venue field money decisions
	// are derived from: it names the venue's payout day, the local date of a
	// paid special day, and the wall clock its opening hours are read on. On a
	// write it means "nothing was saved, fix the name"; on a read it means "we
	// refuse to settle this venue on a guessed day until the stored value is
	// corrected" — never "we quietly used the platform default".
	CodeVenueTimezoneInvalid ErrorCode = "venue_timezone_invalid"

	// Payout destination ownership (money OUT — generating and dispatching a
	// venue payout). All three mean "no money was moved"; they are kept apart
	// because the operator's next action is completely different, and because a
	// mismatch is the one that must never be retried away.

	// CodePayoutDestinationMissing — the restaurant has no payout destination
	// on file at all (never configured, or the card was removed after the
	// payout had already been generated). Nothing to pay to: the money stays
	// owed and is settled once a card is registered.
	CodePayoutDestinationMissing ErrorCode = "payout_destination_missing"

	// CodePayoutDestinationMismatch — the card this payout would be paid to is
	// NOT the card currently registered to that restaurant: it belongs to a
	// different venue, or the venue's destination was changed after the payout
	// was generated and the frozen snapshot now addresses a card nobody at this
	// venue owns any more. This is the ownership failure: it is never retried
	// and never "fixed" by re-sending — the payout is failed, its ledger
	// entries are released, and the next generation builds a payout against the
	// card that is actually registered.
	CodePayoutDestinationMismatch ErrorCode = "payout_destination_mismatch"

	// CodePayoutDestinationIncomplete — the destination belongs to the right
	// restaurant but does not identify a card the provider can address (a
	// tokenized payout needs BOTH the card token and the provider user id it is
	// registered under). Fixed by completing the tokenization step for that
	// venue, not by retrying.
	CodePayoutDestinationIncomplete ErrorCode = "payout_destination_incomplete"

	// --- gastroguide editor (migration 0061) ---
	//
	// The editor cabinet is the only writer of the guide, and every refusal
	// below is something an editor can fix in the panel without asking an
	// engineer. A bare 409/422 here would leave them guessing which of three
	// different problems they hit.

	// CodeGuideSlugTaken — the slug of a collection or a rubric is already used
	// by another row. Slugs are the client-facing stable name (the app links to
	// a rubric by slug), so they are unique platform-wide and the editor must
	// pick a different one. Not retryable.
	CodeGuideSlugTaken ErrorCode = "guide_slug_taken"

	// CodeGuideOrderMismatch — the reorder request did not list exactly the
	// collection's current venues: it repeated one, skipped one, or named a
	// venue that is not in the collection. The order is sent as the intended
	// FINAL sequence, so a payload that disagrees with the membership means the
	// editor's screen is stale (somebody attached or detached a venue in
	// another tab). Nothing is written; the client reloads and retries.
	CodeGuideOrderMismatch ErrorCode = "guide_order_mismatch"

	// CodeGuideVenueAlreadyAttached — this venue is already in this collection.
	// A venue may sit in any number of DIFFERENT collections; what the primary
	// key forbids is the same venue twice in one.
	CodeGuideVenueAlreadyAttached ErrorCode = "guide_venue_already_attached"

	// CodeGuideRouteEmpty — publishing a route with no stops (migration 0078).
	// Deliberately NOT the mirror of a collection, where the same guard was
	// removed: a collection is an article whose venues are a bonus, so an empty
	// one still reads; a route IS its sequence of stops, and an empty itinerary
	// renders as a title, a cover and nothing to walk. Fixed by adding a stop.
	CodeGuideRouteEmpty ErrorCode = "guide_route_empty"

	// Phone change for a signed-in user (POST /users/me/phone/otp/request,
	// POST /users/me/phone/otp/verify). Both narrow a status that on its own
	// reads as something else on this authenticated, own-account flow.

	// CodePhoneInUse — the new number the caller asked to move to already
	// belongs to ANOTHER live account. Kept apart from the generic
	// already_exists (which reads as "you tried to create a duplicate") because
	// here the caller changed nothing of their own: the app must tell them the
	// NUMBER is taken, not that their account already exists. 409, not
	// retryable until the number is freed.
	CodePhoneInUse ErrorCode = "phone_in_use"

	// --- split payments (internal/domain/payment_split.go) ---
	//
	// One guest charge divided between the venue and the platform at the
	// acquirer. The first group is OUR pre-flight validation: nothing was sent
	// anywhere, nothing was charged, and the caller's request is wrong. The
	// second group is the acquirer's own refusal of a split, translated by the
	// adapter — those mean the request never became money either, but the fix
	// is configuration (usually a call to the acquirer's manager), not code.
	//
	// They are separate codes rather than one "split_failed" because the
	// operator's next action differs completely: fix an amount, fill in a
	// venue's sub-merchant id, or phone the acquirer.

	// CodeSplitSumMismatch — the shares do not add up to the amount charged.
	// The single most dangerous split mistake and the only one that a float
	// percentage produces on its own. Nothing was charged.
	CodeSplitSumMismatch ErrorCode = "split_sum_mismatch"

	// CodeSplitShareInvalid — a share is zero, negative, in the wrong
	// currency, or belongs to an unknown/repeated payee.
	CodeSplitShareInvalid ErrorCode = "split_share_invalid"

	// CodeSplitAccountMissing — a recipient has no acquirer sub-merchant
	// account on file. For a venue this means it was never onboarded as a
	// sub-merchant; for the platform it means this deployment has no
	// PAYMENTS_PLATFORM_SPLIT_ACCOUNT set. Not retryable.
	CodeSplitAccountMissing ErrorCode = "split_account_missing"

	// CodeSplitAccountDuplicate — two shares address the same acquirer
	// account. TipTop Pay answers "Duplicated PublicIds for splits".
	CodeSplitAccountDuplicate ErrorCode = "split_account_duplicate"

	// CodeSplitTooManyShares — more recipients than MaxPaymentSplits. Our own
	// sanity bound, see that constant.
	CodeSplitTooManyShares ErrorCode = "split_too_many_shares"

	// CodeSplitAmountTooBig — a confirm/refund asks for more than the split
	// itself is worth ("Amount is too big").
	CodeSplitAmountTooBig ErrorCode = "split_amount_too_big"

	// CodeSplitRequired — the acquirer refused an operation on a SPLIT payment
	// that carried no Splits array ("Field \"Splits\" is required"). It means
	// this build lost track of the original split; the payment is untouched.
	CodeSplitRequired ErrorCode = "split_required"

	// CodeSplitNotSupported — the acquirer terminal is not set up for split
	// payments at all, or its gateway is not ("Split transaction is not
	// supported", ErrorCode 6018). Configuration on the acquirer's side: no
	// retry helps, somebody has to file the request with the manager.
	CodeSplitNotSupported ErrorCode = "split_not_supported"

	// CodeSplitSubMerchantUnknown — the acquirer does not recognise a
	// sub-merchant id we sent ("SubMerchant not found"), or it is no longer
	// working with the marketplace ("SubMerchants is disabled: ...").
	CodeSplitSubMerchantUnknown ErrorCode = "split_submerchant_unknown"

	// CodeSplitTerminalMismatch — the marketplace terminal and the
	// sub-merchant terminal are in different modes ("Terminals modes not
	// equal"), or the request was authenticated with a sub-merchant's own
	// credentials instead of the marketplace's.
	CodeSplitTerminalMismatch ErrorCode = "split_terminal_mismatch"

	// CodeSplitFlowUnsupported — OUR configuration gap, not the acquirer's:
	// the payment flow this build uses is not one the acquirer documents split
	// support for. See docs/payments/tiptoppay-splits.md.
	CodeSplitFlowUnsupported ErrorCode = "split_flow_unsupported"

	// CodePhoneUnchanged — the new number normalizes to the caller's CURRENT
	// number. Nothing to verify and nothing to change; a plain validation_failed
	// would make the app show a field error with no actionable reason. 422.
	CodePhoneUnchanged ErrorCode = "phone_unchanged"
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

// retryAfterError attaches "do not retry before" to an error, the same way
// codedError attaches a code: additively, with Unwrap intact, so status mapping
// and errors.Is are untouched. The transport layer turns it into the standard
// Retry-After header — the repo already answers a 429 from the rate-limit
// middleware that way, and a client should not need a second convention to
// learn how long to wait.
type retryAfterError struct {
	after time.Duration
	err   error
}

func (e *retryAfterError) Error() string             { return e.err.Error() }
func (e *retryAfterError) Unwrap() error             { return e.err }
func (e *retryAfterError) RetryAfter() time.Duration { return e.after }

// WithRetryAfter tags err with how long the caller should wait. A non-positive
// duration or a nil err is left alone (a "retry after 0" header is noise).
func WithRetryAfter(after time.Duration, err error) error {
	if err == nil || after <= 0 {
		return err
	}
	return &retryAfterError{after: after, err: err}
}

// RetryAfterOf reports the wait carried by err or anything it wraps.
func RetryAfterOf(err error) (time.Duration, bool) {
	var r interface{ RetryAfter() time.Duration }
	if errors.As(err, &r) {
		return r.RetryAfter(), true
	}
	return 0, false
}

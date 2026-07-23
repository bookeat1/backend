package logging

// Business event names logged from the usecase layer, one per line in the log
// stream (log.Info(EventXxx, ...fields)). Keeping them as constants — rather
// than ad-hoc string literals scattered across usecases — is what makes it
// possible to grep the log stream or build a Loki/Grafana alert on an exact
// event name without also matching an unrelated log line that happens to
// share a word.
//
// Naming convention: "<domain>.<event>", lower snake_case, past tense for
// something that already happened.
const (
	// Booking lifecycle.
	EventBookingCreated       = "booking.created"
	EventBookingStatusChanged = "booking.status_changed"
	EventBookingCancelled     = "booking.cancelled"
	EventBookingNoShow        = "booking.no_show"

	// Anti-fraud, currently only the booking-creation rate limit (spec §4.4).
	EventAntifraudRejected = "antifraud.rejected"

	// Payments. Not wired to a usecase yet on this branch — see
	// conventions/bookeat-backend.md: only internal/domain and the acquirer
	// adapters (internal/infrastructure/payment/*) exist so far, there is no
	// internal/usecase/payments orchestration layer to call them from. Kept
	// here so that layer logs under these exact names from day one instead of
	// each author inventing their own string.
	EventPaymentCreated         = "payment.created"
	EventPaymentAuthorized      = "payment.authorized" // two-stage hold placed
	EventPaymentCaptured        = "payment.captured"   // hold converted to a charge
	EventPaymentRefunded        = "payment.refunded"
	EventPaymentWebhookReceived = "payment.webhook_received"
	EventPaymentAntifraudReject = "payment.antifraud_rejected"
)

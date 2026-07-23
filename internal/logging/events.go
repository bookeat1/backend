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

	// Payments, wired from internal/usecase/payments.
	EventPaymentCreated         = "payment.created"
	EventPaymentAuthorized      = "payment.authorized" // two-stage hold placed
	EventPaymentCaptured        = "payment.captured"   // hold converted to a charge
	EventPaymentVoided          = "payment.voided"     // hold released, guest never charged
	EventPaymentFailed          = "payment.failed"     // acquirer rejected, or lost a same-booking race
	EventPaymentRefunded        = "payment.refunded"
	EventPaymentSettled         = "payment.settled" // cancellation/no-show resolved with no money movement
	EventPaymentWebhookReceived = "payment.webhook_received"
	EventPaymentWebhookInvalid  = "payment.webhook_invalid" // signature verification failed
	EventPaymentAntifraudReject = "payment.antifraud_rejected"
	EventPaymentExpired         = "payment.expired" // hold TTL lapsed, no capture ever happened

	// Reconciliation worker (internal/usecase/payments.Reconciler). These are
	// what an alert is built on: EventPaymentReconcileTick's counts say
	// whether the worker is finding and clearing stuck payments/refunds at a
	// healthy rate, and EventPaymentReconcileManualReview is the one to page
	// on — it means N consecutive attempts could not tell what happened to
	// real money.
	EventPaymentReconcileTick         = "payment.reconcile_tick"          // one pass summary: found / resolved / still unknown
	EventPaymentReconcileResolved     = "payment.reconcile_resolved"      // one stuck payment/refund reached a terminal-for-now state
	EventPaymentReconcileUnknown      = "payment.reconcile_unknown"       // acquirer answer still does not let us decide
	EventPaymentReconcileManualReview = "payment.reconcile_manual_review" // attempts exhausted, needs a human
)

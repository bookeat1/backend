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
	// Staff deliberately seated a party beyond a table-less venue's declared
	// capacity (migration 0056). The durable record is the
	// booking_capacity_overrides row; this line exists so the event is alertable
	// without polling the table.
	EventBookingOverbooked = "booking.overbooked"

	// Venue capacity mode (spec §4.2 level 2, migration 0054). The switch
	// between booking-by-tables and booking-by-total-capacity is the one venue
	// setting that REWRITES existing reservations — it backfills capacity holds
	// or seats table-less parties at real tables inside one transaction — so both
	// outcomes are logged at Warn: an alert on the refusal means staff are stuck
	// on a toggle, and the "changed" line is the only record of how many
	// reservations were rewritten and how long it took under the venue lock.
	EventVenueCapacityModeChanged = "venue.capacity_mode_changed"
	EventVenueCapacityModeRefused = "venue.capacity_mode_refused"

	// Anti-fraud, currently only the booking-creation rate limit (spec §4.4).
	EventAntifraudRejected = "antifraud.rejected"

	// A guest proved ownership of their phone number (OTP) and the bookings
	// made for that number before they had an account became theirs. Logged on
	// every successful attach because the counts on migration day (~370
	// account-less bookings in production at the time this shipped) are the only
	// way to see the rule doing its work.
	EventGuestBookingsLinked = "guest.bookings_linked"

	// App Store review test account (AUTH_TEST_ACCOUNT_PHONE / _CODE, see
	// internal/usecase/auth/test_account.go). One number logs in with a fixed
	// code and no message is ever sent, so these lines are the ONLY trace such a
	// request leaves — nothing is written to otp_codes. All three are logged at
	// WARN, including the successful login: on a healthy contour this number
	// should see a handful of events per app submission and nothing in between,
	// so a stream of them (especially login_attempt with accepted=false) is
	// somebody probing and is worth an alert.
	EventTestAccountOTPRequested       = "auth.test_account_otp_requested"
	EventTestAccountLoginAttempt       = "auth.test_account_login_attempt" // field: accepted=true|false
	EventTestAccountPhoneChangeRefused = "auth.test_account_phone_change_refused"

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

	// Legacy one-way sync (cmd/worker's legacysync loop). LegacySyncTick is one
	// pass summary per entity: fetched / written / parked (parent not synced
	// yet, retried next tick) / skipped (a source row that can never land, e.g.
	// an overlapping table hold — logged and stepped over, never retried).
	EventLegacySyncTick = "legacy_sync.tick"
)

package domain

import "time"

// TicketRefundPolicy is a venue's own refund rules for ONE event (owner
// decision, 2026-07-25 — the platform no longer imposes a single policy):
// whether a ticket can be refunded at all, and how long before the event starts
// a refund is still allowed.
//
// It is a value object, not a row: the event carries the venue's CURRENT rules
// (Event.TicketRefundPolicy) and every sold ticket carries a frozen SNAPSHOT of
// the rules it was bought under (EventTicket.RefundPolicy). The eligibility
// decision below always runs against the snapshot for an existing ticket — the
// terms promise the guest that a later change by the venue does not apply to a
// ticket already paid for.
type TicketRefundPolicy struct {
	// Refundable is the venue's master switch. false = a sold ticket is final
	// (only an explicitly authorized staff override can still refund it).
	Refundable bool
	// CutoffMinutes is how many minutes before the event's start a guest may
	// still self-refund. 0 = right up to the start moment.
	CutoffMinutes int
}

// DefaultTicketRefundPolicy is the platform fallback for an event whose venue
// has not chosen anything yet: refundable up to 24 hours before the start
// (owner decision, 2026-07-25). A venue that wants a stricter rule sets it on
// its own event. This is the ONE place the platform default lives — the
// migration backfill/DEFAULT (0047/0048) and the API's create fallback must
// agree with it, or an event's rules would depend on which door it came through.
var DefaultTicketRefundPolicy = TicketRefundPolicy{Refundable: true, CutoffMinutes: 1440}

// TicketRefundDenyReason explains a refusal in a machine-readable way, so the
// app can show the right message ("this event's tickets are non-refundable" vs
// "the refund window closed on <date>") without parsing an error string.
type TicketRefundDenyReason string

const (
	// TicketRefundDenyNone is the zero value: nothing was denied.
	TicketRefundDenyNone TicketRefundDenyReason = ""
	// TicketRefundDenyNotRefundable = the venue sells this event's tickets as
	// final sale.
	TicketRefundDenyNotRefundable TicketRefundDenyReason = "not_refundable"
	// TicketRefundDenyWindowClosed = refundable in principle, but the cutoff
	// before the event's start has already passed.
	TicketRefundDenyWindowClosed TicketRefundDenyReason = "window_closed"
)

// TicketRefundDecision is the outcome of the eligibility check. Deadline is the
// last instant at which a self-refund is accepted; it is meaningful only when
// the policy is refundable at all (zero time otherwise) and is exposed to the
// app so the guest can see the rule BEFORE buying.
type TicketRefundDecision struct {
	Allowed  bool
	Reason   TicketRefundDenyReason
	Deadline time.Time
}

// TicketRefundAllowed decides whether a guest may refund a ticket sold under
// policy for an event starting at eventStartsAt, as of now. It is deliberately
// PURE — no repository, no clock, no actor: the money/eligibility rule lives
// here in one testable place, and the usecase only decides WHO is asking (a
// guest follows this decision; staff with PermPaymentRefund may override it
// explicitly).
//
// The deadline is exclusive: at exactly starts_at - cutoff the window is
// already closed. A guest standing on the boundary is a race we resolve in the
// venue's favour, because the venue has by then committed the seat.
func TicketRefundAllowed(policy TicketRefundPolicy, eventStartsAt, now time.Time) TicketRefundDecision {
	if !policy.Refundable {
		return TicketRefundDecision{Allowed: false, Reason: TicketRefundDenyNotRefundable}
	}
	// A negative window would read as "refundable until after the event began";
	// the DB CHECK refuses to store one, and we clamp here so a hand-edited row
	// can never widen the window instead of narrowing it.
	cutoff := policy.CutoffMinutes
	if cutoff < 0 {
		cutoff = 0
	}
	deadline := eventStartsAt.Add(-time.Duration(cutoff) * time.Minute)
	if !now.Before(deadline) {
		return TicketRefundDecision{Allowed: false, Reason: TicketRefundDenyWindowClosed, Deadline: deadline}
	}
	return TicketRefundDecision{Allowed: true, Deadline: deadline}
}

// TicketRefundPolicy returns the venue's CURRENT rules for this event — what a
// guest is shown before buying, and what gets frozen onto the next ticket sold.
func (e Event) TicketRefundPolicy() TicketRefundPolicy {
	return TicketRefundPolicy{Refundable: e.TicketsRefundable, CutoffMinutes: e.TicketRefundCutoffMinutes}
}

// RefundPolicy returns the rules this ticket was actually SOLD under (the
// purchase-time snapshot). This — never the event's current columns — is what
// the refund path must evaluate.
func (t EventTicket) RefundPolicy() TicketRefundPolicy {
	return TicketRefundPolicy{Refundable: t.RefundPolicyRefundable, CutoffMinutes: t.RefundPolicyCutoffMinutes}
}

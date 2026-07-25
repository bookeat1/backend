package domain

import (
	"testing"
	"time"
)

// TestTicketRefundAllowed is the table for the whole eligibility rule: the
// venue's master switch, the cutoff window and the exact boundary. Every money
// decision on the refund path comes from here, so it is tested here and nowhere
// else.
func TestTicketRefundAllowed(t *testing.T) {
	start := time.Date(2026, 8, 1, 19, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		policy       TicketRefundPolicy
		now          time.Time
		wantAllowed  bool
		wantReason   TicketRefundDenyReason
		wantDeadline time.Time
	}{
		{
			name:        "non-refundable event refuses even a week ahead",
			policy:      TicketRefundPolicy{Refundable: false, CutoffMinutes: 1440},
			now:         start.Add(-7 * 24 * time.Hour),
			wantAllowed: false,
			wantReason:  TicketRefundDenyNotRefundable,
			// No deadline is offered: there is no window to speak of.
			wantDeadline: time.Time{},
		},
		{
			name:         "refundable, comfortably before the cutoff",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 1440},
			now:          start.Add(-48 * time.Hour),
			wantAllowed:  true,
			wantReason:   TicketRefundDenyNone,
			wantDeadline: start.Add(-24 * time.Hour),
		},
		{
			name:         "refundable, one minute before the cutoff",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 120},
			now:          start.Add(-121 * time.Minute),
			wantAllowed:  true,
			wantReason:   TicketRefundDenyNone,
			wantDeadline: start.Add(-120 * time.Minute),
		},
		{
			name:         "exactly on the deadline is already closed",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 120},
			now:          start.Add(-120 * time.Minute),
			wantAllowed:  false,
			wantReason:   TicketRefundDenyWindowClosed,
			wantDeadline: start.Add(-120 * time.Minute),
		},
		{
			name:         "past the cutoff",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 120},
			now:          start.Add(-30 * time.Minute),
			wantAllowed:  false,
			wantReason:   TicketRefundDenyWindowClosed,
			wantDeadline: start.Add(-120 * time.Minute),
		},
		{
			name:         "zero cutoff allows a refund right up to the start",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 0},
			now:          start.Add(-time.Second),
			wantAllowed:  true,
			wantReason:   TicketRefundDenyNone,
			wantDeadline: start,
		},
		{
			name:         "zero cutoff still refuses after the start",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: 0},
			now:          start.Add(time.Second),
			wantAllowed:  false,
			wantReason:   TicketRefundDenyWindowClosed,
			wantDeadline: start,
		},
		{
			name:         "a negative cutoff is clamped to zero, never widened",
			policy:       TicketRefundPolicy{Refundable: true, CutoffMinutes: -600},
			now:          start.Add(time.Minute),
			wantAllowed:  false,
			wantReason:   TicketRefundDenyWindowClosed,
			wantDeadline: start,
		},
		{
			name:         "the platform default grants nobody a self-refund",
			policy:       DefaultTicketRefundPolicy,
			now:          start.Add(-30 * 24 * time.Hour),
			wantAllowed:  false,
			wantReason:   TicketRefundDenyNotRefundable,
			wantDeadline: time.Time{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TicketRefundAllowed(tc.policy, start, tc.now)
			if got.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v", got.Allowed, tc.wantAllowed)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if !got.Deadline.Equal(tc.wantDeadline) {
				t.Fatalf("Deadline = %s, want %s", got.Deadline, tc.wantDeadline)
			}
		})
	}
}

// TestRefundPolicyAccessors proves the two carriers of the policy stay
// distinct: the event holds the venue's CURRENT rules, the ticket holds the
// snapshot it was sold under. Mixing them up is exactly the bug the terms
// forbid, so it is pinned here.
func TestRefundPolicyAccessors(t *testing.T) {
	e := Event{TicketsRefundable: false, TicketRefundCutoffMinutes: 60}
	if got := e.TicketRefundPolicy(); got.Refundable || got.CutoffMinutes != 60 {
		t.Fatalf("event policy = %+v", got)
	}
	// The venue has since switched refunds off, but this ticket was sold when
	// they were on — the snapshot must still say so.
	tk := EventTicket{RefundPolicyRefundable: true, RefundPolicyCutoffMinutes: 120}
	if got := tk.RefundPolicy(); !got.Refundable || got.CutoffMinutes != 120 {
		t.Fatalf("ticket policy = %+v", got)
	}
}

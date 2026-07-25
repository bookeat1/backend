package events

import (
	"testing"

	"backend-core/internal/domain"
)

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// The refund rules are money rules, so how this payload reads them is pinned
// here rather than left to the handler's behaviour under a live request.
func TestEventRequestRefundPolicy(t *testing.T) {
	t.Run("create: absent fields take the ONE platform default", func(t *testing.T) {
		got := eventRequest{}.refundPolicy()
		if got != domain.DefaultTicketRefundPolicy {
			t.Fatalf("policy = %+v, want the platform default %+v", got, domain.DefaultTicketRefundPolicy)
		}
	})

	t.Run("create: a venue's own rules win", func(t *testing.T) {
		got := eventRequest{TicketsRefundable: boolPtr(false), TicketRefundCutoffMinutes: intPtr(0)}.refundPolicy()
		if got.Refundable || got.CutoffMinutes != 0 {
			t.Fatalf("policy = %+v, want non-refundable/0", got)
		}
	})

	// A full-replace update that says nothing about refunds must not touch them:
	// an older cabinet build editing a title cannot switch a venue's refunds off.
	t.Run("update: absent means keep", func(t *testing.T) {
		policy, ok := eventRequest{}.refundPolicyUpdate()
		if !ok {
			t.Fatal("a payload with no refund fields must be accepted")
		}
		if policy != nil {
			t.Fatalf("policy = %+v, want nil (leave the rules alone)", policy)
		}
	})

	t.Run("update: both fields replace the rules", func(t *testing.T) {
		policy, ok := eventRequest{TicketsRefundable: boolPtr(true), TicketRefundCutoffMinutes: intPtr(60)}.refundPolicyUpdate()
		if !ok || policy == nil {
			t.Fatalf("ok=%v policy=%v, want an accepted policy", ok, policy)
		}
		if !policy.Refundable || policy.CutoffMinutes != 60 {
			t.Fatalf("policy = %+v, want refundable/60", *policy)
		}
	})

	// Half a policy is refused rather than guessed: filling in the missing half
	// from a default would silently rewrite a rule the caller never mentioned.
	for name, req := range map[string]eventRequest{
		"only the flag":   {TicketsRefundable: boolPtr(true)},
		"only the window": {TicketRefundCutoffMinutes: intPtr(60)},
	} {
		t.Run("update: "+name+" is refused", func(t *testing.T) {
			if _, ok := req.refundPolicyUpdate(); ok {
				t.Fatal("half a policy must be refused (422), not completed by guesswork")
			}
		})
	}
}

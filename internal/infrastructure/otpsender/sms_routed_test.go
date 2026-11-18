package otpsender

import (
	"context"
	"errors"
	"testing"
)

type namedSMSProvider struct {
	name string
	sent *[]string
}

func (p namedSMSProvider) ProviderName() string { return p.name }

func (p namedSMSProvider) SendSMS(_ context.Context, _, _ string) (string, error) {
	*p.sent = append(*p.sent, p.name)
	return p.name + "-id", nil
}

// A provider must only ever see the numbers it can deliver to: Mobizon accepts
// (and bills) a US number and then drops it, so "who gets this number" is the
// whole point of the router.
func TestRoutedSMSPicksProviderByPrefix(t *testing.T) {
	var sent []string
	mobizon := namedSMSProvider{name: "mobizon", sent: &sent}
	twilio := namedSMSProvider{name: "twilio", sent: &sent}
	beeline := namedSMSProvider{name: "beeline", sent: &sent}

	tests := map[string]struct {
		fallback SMSProvider
		routes   []SMSRoute
		phone    string
		want     string // provider that must send; "" = ErrChannelUnavailable
	}{
		"matched prefix": {
			fallback: mobizon,
			routes:   []SMSRoute{{Prefix: "+1", Provider: twilio}},
			phone:    "+16504444022",
			want:     "twilio",
		},
		"unmatched number goes to the fallback": {
			fallback: mobizon,
			routes:   []SMSRoute{{Prefix: "+1", Provider: twilio}},
			phone:    "+77011234567",
			want:     "mobizon",
		},
		"longest prefix wins regardless of declaration order": {
			fallback: twilio,
			routes:   []SMSRoute{{Prefix: "+7", Provider: mobizon}, {Prefix: "+7705", Provider: beeline}},
			phone:    "+77051234567",
			want:     "beeline",
		},
		"no fallback and no match sends nothing": {
			routes: []SMSRoute{{Prefix: "+7", Provider: mobizon}},
			phone:  "+16504444022",
		},
		"a nil-provider route blocks the prefix instead of falling back": {
			fallback: mobizon,
			routes:   []SMSRoute{{Prefix: "+1", Provider: nil}},
			phone:    "+16504444022",
		},
		"first route wins on a duplicate prefix": {
			routes: []SMSRoute{{Prefix: "+1", Provider: twilio}, {Prefix: "+1", Provider: mobizon}},
			phone:  "+16504444022",
			want:   "twilio",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			sent = nil
			id, err := NewRoutedSMS(tc.fallback, tc.routes).SendSMS(context.Background(), tc.phone, "text")
			if tc.want == "" {
				if !errors.Is(err, ErrChannelUnavailable) {
					t.Fatalf("err = %v, want ErrChannelUnavailable", err)
				}
				if len(sent) != 0 {
					t.Fatalf("providers called: %v, want none — an unroutable number must cost nothing", sent)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendSMS: %v", err)
			}
			if len(sent) != 1 || sent[0] != tc.want {
				t.Fatalf("sent via %v, want exactly [%s]", sent, tc.want)
			}
			if id != tc.want+"-id" {
				t.Fatalf("message id = %q, want the chosen provider's id", id)
			}
		})
	}
}

func TestRoutedSMSProviderName(t *testing.T) {
	var sent []string
	r := NewRoutedSMS(namedSMSProvider{name: "mobizon", sent: &sent}, []SMSRoute{
		{Prefix: "+1", Provider: namedSMSProvider{name: "twilio", sent: &sent}},
		{Prefix: "+998", Provider: nil},
	})
	if got, want := r.ProviderName(), "routed(+998=none,+1=twilio,*=mobizon)"; got != want {
		t.Fatalf("ProviderName() = %q, want %q", got, want)
	}
}

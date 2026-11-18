package otpsender

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SMSRoute sends every number starting with Prefix through Provider.
//
// Prefix is an E.164 prefix with the leading "+" ("+7", "+1", "+7701"). A nil
// Provider is a deliberate dead end: numbers under that prefix get no SMS at
// all rather than falling through to the default provider.
type SMSRoute struct {
	Prefix   string
	Provider SMSProvider
}

// RoutedSMS picks an SMS provider by the number's country/operator prefix.
//
// # Why it exists
//
// No single SMS provider reaches every country. Mobizon delivers Kazakh numbers
// and ACCEPTS everything else — a "+1" number gets `code 0`, a message id and a
// charge, and then goes UNDELIV a few seconds later (US carriers do not take
// alphanumeric senders). From the waterfall's side that is a successful send,
// so the guest is told a code is on its way and it never arrives. Routing by
// prefix is what keeps a provider away from the numbers it cannot reach.
//
// # Shape
//
// It is an SMSProvider, not a Channel: the waterfall still sees one "sms"
// channel and the guest's remembered channel is still "sms", so adding or
// changing routes orphans no memory (same rule as swapping providers — see
// SMSProvider.ProviderName).
//
// The longest matching prefix wins, so "+7701" can be carved out of "+7". A
// number no route matches goes to the fallback; with no fallback, or on a route
// whose Provider is nil, the send fails with ErrChannelUnavailable — nothing is
// sent, nothing is billed, and the waterfall moves on.
type RoutedSMS struct {
	routes   []SMSRoute // longest prefix first
	fallback SMSProvider
}

// NewRoutedSMS builds the router. fallback may be nil (unmatched numbers then
// get no SMS). Routes with an empty prefix are dropped; on a duplicate prefix
// the first one wins.
func NewRoutedSMS(fallback SMSProvider, routes []SMSRoute) *RoutedSMS {
	kept := make([]SMSRoute, 0, len(routes))
	seen := make(map[string]bool, len(routes))
	for _, r := range routes {
		r.Prefix = strings.TrimSpace(r.Prefix)
		if r.Prefix == "" || seen[r.Prefix] {
			continue
		}
		seen[r.Prefix] = true
		kept = append(kept, r)
	}
	sort.SliceStable(kept, func(i, j int) bool { return len(kept[i].Prefix) > len(kept[j].Prefix) })
	return &RoutedSMS{routes: kept, fallback: fallback}
}

var _ SMSProvider = (*RoutedSMS)(nil)

// ProviderName renders the routing table for the startup log, e.g.
// "routed(+1=twilio,+7=mobizon,*=mobizon)".
func (r *RoutedSMS) ProviderName() string {
	parts := make([]string, 0, len(r.routes)+1)
	for _, route := range r.routes {
		parts = append(parts, route.Prefix+"="+smsProviderLabel(route.Provider))
	}
	parts = append(parts, "*="+smsProviderLabel(r.fallback))
	return "routed(" + strings.Join(parts, ",") + ")"
}

func smsProviderLabel(p SMSProvider) string {
	if p == nil {
		return "none"
	}
	return p.ProviderName()
}

// SendSMS hands the text to the provider that owns the number's prefix.
func (r *RoutedSMS) SendSMS(ctx context.Context, phone, text string) (string, error) {
	provider := r.pick(phone)
	if provider == nil {
		// No prefix in the error: the waterfall logs it next to a masked phone,
		// and the country code is already visible there.
		return "", fmt.Errorf("sms: no provider is routed for this number: %w", ErrChannelUnavailable)
	}
	return provider.SendSMS(ctx, phone, text)
}

func (r *RoutedSMS) pick(phone string) SMSProvider {
	for _, route := range r.routes {
		if strings.HasPrefix(phone, route.Prefix) {
			return route.Provider
		}
	}
	return r.fallback
}

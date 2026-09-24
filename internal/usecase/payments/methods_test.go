package payments

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// methodsHarness: a venue with Kaspi (account-bound acquirer) and a card
// acquirer both registered; each test flips the venue's switches and the
// Kaspi binding.
func methodsHarness(t *testing.T, kaspiOn, cardOn, kaspiBound, cardRegistered, kaspiRegistered bool) *capabilityHarness {
	t.Helper()
	h := newCapabilityHarness(t, capabilityOptions{
		paymentsEnabled: true, accountBound: true, wireSplitAccounts: true,
	})
	rid := h.booking.RestaurantID
	o := h.settings.byRestaurant[rid]
	o.KaspiEnabled, o.CardEnabled = boolPtr(kaspiOn), boolPtr(cardOn)
	h.settings.byRestaurant[rid] = o

	h.resolver.byProvider = map[domain.PaymentProvider]domain.PaymentGateway{}
	if kaspiRegistered {
		h.resolver.byProvider[domain.ProviderKaspi] = accountBoundGateway{fakeGateway: newFakeGateway(domain.ProviderKaspi)}
	}
	if cardRegistered {
		h.resolver.byProvider[domain.ProviderFreedomPay] = newFakeGateway(domain.ProviderFreedomPay)
	}
	if kaspiBound {
		h.accounts.byRestaurant[rid] = testVenueSplitAccount
	}
	return h
}

func TestAvailablePaymentMethods(t *testing.T) {
	kaspi, card := domain.MethodKaspi, domain.MethodCard
	cases := []struct {
		name                                                     string
		kaspiOn, cardOn, kaspiBound, cardRegistered, kaspiRegist bool
		want                                                     []domain.PaymentMethod
	}{
		{"kaspi only", true, false, true, true, true, []domain.PaymentMethod{kaspi}},
		{"card only", false, true, false, true, true, []domain.PaymentMethod{card}},
		{"both", true, true, true, true, true, []domain.PaymentMethod{kaspi, card}},
		{"none switched on", false, false, true, true, true, []domain.PaymentMethod{}},
		{"kaspi on but not bound", true, false, false, true, true, []domain.PaymentMethod{}},
		{"kaspi not bound, card still offered", true, true, false, true, true, []domain.PaymentMethod{card}},
		{"card on but no card provider enabled", true, true, true, false, true, []domain.PaymentMethod{kaspi}},
		{"kaspi on but kaspi provider not registered", true, true, true, true, false, []domain.PaymentMethod{card}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := methodsHarness(t, tc.kaspiOn, tc.cardOn, tc.kaspiBound, tc.cardRegistered, tc.kaspiRegist)
			got, err := h.uc.AvailablePaymentMethods(context.Background(), h.booking.RestaurantID)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("methods = %v, want %v", got, tc.want)
			}
			accepts, err := h.uc.AcceptsOnlinePayment(context.Background(), h.booking.RestaurantID)
			if err != nil || accepts != (len(tc.want) > 0) {
				t.Fatalf("accepts = %v (err %v), want %v", accepts, err, len(tc.want) > 0)
			}
		})
	}
}

func TestAvailablePaymentMethodsEmptyWhenMasterSwitchOff(t *testing.T) {
	h := methodsHarness(t, true, true, true, true, true)
	o := h.settings.byRestaurant[h.booking.RestaurantID]
	o.PaymentsEnabled = boolPtr(false)
	h.settings.byRestaurant[h.booking.RestaurantID] = o
	got, err := h.uc.AvailablePaymentMethods(context.Background(), h.booking.RestaurantID)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty non-nil, nil", got, err)
	}
}

func TestLegacyProviderKaspiMapsToKaspiMethodOnly(t *testing.T) {
	kaspiP, cardP := domain.ProviderKaspi, domain.ProviderTipTopPay
	s := resolveSettings(domain.PaymentSettingsOverride{Provider: &kaspiP}, Config{Enabled: true})
	if !s.KaspiEnabled || s.CardEnabled {
		t.Fatalf("legacy kaspi => kaspi=%v card=%v, want true/false", s.KaspiEnabled, s.CardEnabled)
	}
	s = resolveSettings(domain.PaymentSettingsOverride{Provider: &cardP}, Config{Enabled: true})
	if s.KaspiEnabled || !s.CardEnabled {
		t.Fatalf("legacy other => kaspi=%v card=%v, want false/true", s.KaspiEnabled, s.CardEnabled)
	}
}

func TestCreateRoutesByMethod(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		method domain.PaymentMethod
		want   domain.PaymentProvider
	}{{domain.MethodKaspi, domain.ProviderKaspi}, {domain.MethodCard, domain.ProviderFreedomPay}} {
		t.Run(string(tc.method), func(t *testing.T) {
			h := methodsHarness(t, true, true, true, true, true)
			p, err := h.uc.CreateForBooking(ctx, Actor{}, CreateInput{
				BookingID: h.booking.ID, IdempotencyKey: "k-" + string(tc.method), ReturnURL: "https://app/r", Method: tc.method,
			})
			if err != nil {
				t.Fatalf("CreateForBooking: %v", err)
			}
			if p.Provider != tc.want {
				t.Fatalf("provider = %s, want %s", p.Provider, tc.want)
			}
		})
	}
}

func TestCreateRefusesUnavailableOrUnknownMethod(t *testing.T) {
	ctx := context.Background()
	h := methodsHarness(t, false, true, false, true, true) // card only
	for _, m := range []domain.PaymentMethod{domain.MethodKaspi, "bitcoin"} {
		_, err := h.uc.CreateForBooking(ctx, Actor{}, CreateInput{
			BookingID: h.booking.ID, IdempotencyKey: "k-" + string(m), ReturnURL: "https://app/r", Method: m,
		})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("method %q: err = %v, want ErrValidation", m, err)
		}
	}
}

func TestCreateWithoutMethodKeepsWorking(t *testing.T) {
	ctx := context.Background()
	// card only
	h := methodsHarness(t, false, true, false, true, true)
	p, err := h.uc.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: h.booking.ID, IdempotencyKey: "a", ReturnURL: "https://app/r"})
	if err != nil || p.Provider != domain.ProviderFreedomPay {
		t.Fatalf("card only: %v, %+v", err, p)
	}
	// kaspi only, bound
	h = methodsHarness(t, true, false, true, true, true)
	p, err = h.uc.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: h.booking.ID, IdempotencyKey: "b", ReturnURL: "https://app/r"})
	if err != nil || p.Provider != domain.ProviderKaspi {
		t.Fatalf("kaspi only: %v, %+v", err, p)
	}
	// nothing switched on
	h = methodsHarness(t, false, false, true, true, true)
	if _, err = h.uc.CreateForBooking(ctx, Actor{}, CreateInput{BookingID: h.booking.ID, IdempotencyKey: "c", ReturnURL: "https://app/r"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("none: err = %v, want ErrValidation", err)
	}
	_ = uuid.Nil
}

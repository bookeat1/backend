package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestPayoutTransitions(t *testing.T) {
	ok := [][2]PayoutStatus{
		{PayoutPending, PayoutSent},
		{PayoutPending, PayoutFailed},
		{PayoutSent, PayoutPaid},
		{PayoutSent, PayoutFailed},
	}
	for _, c := range ok {
		if err := ValidatePayoutTransition(c[0], c[1]); err != nil {
			t.Errorf("expected %s->%s allowed, got %v", c[0], c[1], err)
		}
	}
	bad := [][2]PayoutStatus{
		{PayoutPaid, PayoutSent},    // paid is terminal
		{PayoutFailed, PayoutPaid},  // failed is terminal
		{PayoutPending, PayoutPaid}, // must go through sent
		{PayoutSent, PayoutPending}, // no going back
		{PayoutPaid, PayoutFailed},  // terminal
	}
	for _, c := range bad {
		if err := ValidatePayoutTransition(c[0], c[1]); err == nil {
			t.Errorf("expected %s->%s rejected", c[0], c[1])
		}
	}
	if !PayoutPaid.Terminal() || !PayoutFailed.Terminal() {
		t.Error("paid and failed must be terminal")
	}
	if PayoutPending.Terminal() || PayoutSent.Terminal() {
		t.Error("pending and sent must not be terminal")
	}
}

func TestPayoutMethodValid(t *testing.T) {
	if !PayoutMethodFreedomPayCardToken.Valid() {
		t.Error("card token method must be valid")
	}
	if PayoutMethod("freedompay_raw_pan").Valid() {
		t.Error("an unknown method must be invalid")
	}
}

func TestPayoutDestinationValidate_RejectsRawPAN(t *testing.T) {
	base := func() PayoutDestination {
		return PayoutDestination{
			RestaurantID:        uuid.New(),
			Provider:            ProviderFreedomPay,
			Method:              PayoutMethodFreedomPayCardToken,
			Token:               uuid.NewString(),
			ProviderCustomerRef: "fp-1",
			MaskedIdentifier:    "440043******1234",
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a well-formed tokenized destination must validate, got %v", err)
	}

	// Raw PAN as the token.
	d := base()
	d.Token = "4400430000001234"
	if err := d.Validate(); err == nil {
		t.Error("a raw PAN token must be rejected")
	}
	// Spaced/dashed PAN in the masked field.
	d = base()
	d.MaskedIdentifier = "4400-4300-0000-1234"
	if err := d.Validate(); err == nil {
		t.Error("a raw PAN in the masked field must be rejected")
	}
	// A non-UUID token (not an opaque provider handle).
	d = base()
	d.Token = "card-1234"
	if err := d.Validate(); err == nil {
		t.Error("a non-token handle must be rejected")
	}
	// Wrong provider.
	d = base()
	d.Provider = ProviderTipTopPay
	if err := d.Validate(); err == nil {
		t.Error("increment 1 supports only FreedomPay payouts")
	}
}

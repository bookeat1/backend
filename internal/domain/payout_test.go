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

// --- ownership of the card money is sent to -------------------------------

// TestPayoutDestinationVerifyOwnedBy covers the three ways a card can fail to
// be "the card this venue is entitled to be paid on". Each one carries its own
// code because the operator's next action differs, so the codes are asserted,
// not just the fact that an error came back.
func TestPayoutDestinationVerifyOwnedBy(t *testing.T) {
	rid := uuid.New()
	good := &PayoutDestination{
		RestaurantID:        rid,
		Provider:            ProviderFreedomPay,
		Method:              PayoutMethodFreedomPayCardToken,
		Token:               uuid.NewString(),
		ProviderCustomerRef: "fp-user-1",
	}
	if err := good.VerifyOwnedBy(rid); err != nil {
		t.Fatalf("the venue's own complete destination must pass, got %v", err)
	}

	foreign := *good
	foreign.RestaurantID = uuid.New()

	incomplete := *good
	incomplete.ProviderCustomerRef = "  "

	malformed := *good
	malformed.Token = "card-1234"

	cases := map[string]struct {
		dest *PayoutDestination
		want ErrorCode
	}{
		"no destination at all":                      {nil, CodePayoutDestinationMissing},
		"a card registered to a different venue":     {&foreign, CodePayoutDestinationMismatch},
		"a card with no provider user id":            {&incomplete, CodePayoutDestinationIncomplete},
		"a handle that is not a provider card token": {&malformed, CodePayoutDestinationIncomplete},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.dest.VerifyOwnedBy(rid)
			if err == nil {
				t.Fatal("expected the ownership check to refuse")
			}
			code, ok := CodeOf(err)
			if !ok || code != tc.want {
				t.Fatalf("expected code %q, got %q (ok=%v): %v", tc.want, code, ok, err)
			}
		})
	}

	// A nil restaurant id must never match a real destination: an unset id is
	// not proof of anything, so it is refused rather than treated as a wildcard.
	if err := good.VerifyOwnedBy(uuid.Nil); err == nil {
		t.Fatal("a nil restaurant id must not pass the ownership check")
	}
}

// TestVerifyPayoutDestinationSnapshot proves the frozen card on a payout is
// re-checked against the venue's LIVE destination — the window between
// generating a payout and dispatching it is where a card can change hands.
func TestVerifyPayoutDestinationSnapshot(t *testing.T) {
	rid := uuid.New()
	token, ref := uuid.NewString(), "fp-user-1"
	current := &PayoutDestination{
		RestaurantID:        rid,
		Provider:            ProviderFreedomPay,
		Method:              PayoutMethodFreedomPayCardToken,
		Token:               token,
		ProviderCustomerRef: ref,
	}
	p := Payout{
		ID:                     uuid.New(),
		RestaurantID:           rid,
		Method:                 PayoutMethodFreedomPayCardToken,
		DestinationToken:       token,
		DestinationCustomerRef: ref,
	}
	if err := VerifyPayoutDestination(p, current); err != nil {
		t.Fatalf("a payout addressing the venue's current card must pass, got %v", err)
	}

	// The venue replaced its card after the payout was generated: the old
	// snapshot must NOT be paid, and must not be silently re-pointed either.
	swapped := *current
	swapped.Token = uuid.NewString()
	err := VerifyPayoutDestination(p, &swapped)
	if code, _ := CodeOf(err); code != CodePayoutDestinationMismatch {
		t.Fatalf("a stale card snapshot must be refused as a mismatch, got %v", err)
	}

	// Same card token, different provider user id — the pair is the address,
	// so half a match is a mismatch.
	otherUser := *current
	otherUser.ProviderCustomerRef = "fp-user-2"
	err = VerifyPayoutDestination(p, &otherUser)
	if code, _ := CodeOf(err); code != CodePayoutDestinationMismatch {
		t.Fatalf("a different provider user id must be refused, got %v", err)
	}

	// The card is no longer attached to anybody at this venue.
	err = VerifyPayoutDestination(p, nil)
	if code, _ := CodeOf(err); code != CodePayoutDestinationMissing {
		t.Fatalf("a payout with no live destination must be refused as missing, got %v", err)
	}

	// The destination on file belongs to another restaurant entirely.
	foreign := *current
	foreign.RestaurantID = uuid.New()
	err = VerifyPayoutDestination(p, &foreign)
	if code, _ := CodeOf(err); code != CodePayoutDestinationMismatch {
		t.Fatalf("a foreign destination must be refused as a mismatch, got %v", err)
	}
}

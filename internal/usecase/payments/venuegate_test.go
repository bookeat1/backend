package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// "Can this venue take money online" — the flag the guest app reads
// (accepts_online_payment) must be the SAME answer the checkout gives.
// ---------------------------------------------------------------------------

// accountBoundGateway is an acquirer that cannot charge for a venue it has no
// account for — Kaspi Pay's shape (see kaspi.Gateway.RequiresMerchantAccount /
// validateAuthorize). Both halves are modelled: it DECLARES the requirement,
// and its Authorize REFUSES an empty reference, so a test can prove the flag
// and the charge agree instead of asserting the flag against itself.
type accountBoundGateway struct {
	*fakeGateway
}

func (g accountBoundGateway) RequiresMerchantAccount() bool { return true }

func (g accountBoundGateway) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	// Trimmed, exactly like kaspi.validateAuthorize: a reference of spaces is
	// an unfinished onboarding, not an address.
	if strings.TrimSpace(req.MerchantAccountRef) == "" {
		return nil, domain.WithCode(domain.CodeSplitAccountMissing, fmt.Errorf(
			"fake: this venue has no company configured: %w", domain.ErrUnavailable))
	}
	return g.fakeGateway.Authorize(ctx, req)
}

// capabilityHarness is the venue-level wiring the flag runs on: the three
// things that decide whether a venue can be charged, each independently
// controllable.
type capabilityHarness struct {
	uc       *createUseCase
	booking  *domain.Booking
	settings *fakeRestaurantSettings
	resolver *fakeGatewayResolver
	accounts *fakeSplitAccounts
}

type capabilityOptions struct {
	// paymentsEnabled is the venue's restaurants.payments_enabled override.
	paymentsEnabled bool
	// accountBound makes the resolved acquirer one that requires a per-venue
	// account (Kaspi's shape); otherwise it is an ordinary acquirer that
	// settles onto one platform account (FreedomPay's shape).
	accountBound bool
	// splitEnabled turns on PAYMENTS_SPLIT_ENABLED.
	splitEnabled bool
	// wireSplitAccounts controls whether the venue↔account mapping is wired at
	// all (WithSplitAccounts).
	wireSplitAccounts bool
}

func newCapabilityHarness(t *testing.T, opts capabilityOptions) *capabilityHarness {
	t.Helper()
	b := testBooking(uuid.New())
	repo := newFakePaymentRepo()
	outbox := newFakePaymentOutbox()
	settings := newFakeRestaurantSettings()
	settings.byRestaurant[b.RestaurantID] = domain.PaymentSettingsOverride{
		PaymentsEnabled:    boolPtr(opts.paymentsEnabled),
		DepositRequired:    boolPtr(true),
		DepositAmountMinor: int64Ptr(1_000_000),
		ServiceFeeBps:      intPtr(350),
	}
	base := newFakeGateway(domain.ProviderKaspi)
	var gw domain.PaymentGateway = base
	if opts.accountBound {
		gw = accountBoundGateway{fakeGateway: base}
	}
	resolver := &fakeGatewayResolver{byProvider: map[domain.PaymentProvider]domain.PaymentGateway{
		domain.ProviderKaspi: gw,
	}}
	accounts := newFakeSplitAccounts()
	ucOpts := []CreateOption{}
	if opts.wireSplitAccounts {
		ucOpts = append(ucOpts, WithSplitAccounts(accounts))
	}
	tx := &fakeTx{payments: repo, outbox: outbox}
	u := NewCreateUseCase(repo, outbox, newFakeBookingReader(b), newFakeItemReader(), settings,
		newFakeSpecialDays(), resolver, newFakeManagerChecker(), tx,
		Config{
			DefaultProvider:         domain.ProviderKaspi,
			SplitEnabled:            opts.splitEnabled,
			PlatformSplitAccountRef: testPlatformSplitAccount,
		},
		ucOpts...,
	).(*createUseCase)
	return &capabilityHarness{uc: u, booking: b, settings: settings, resolver: resolver, accounts: accounts}
}

// connectedHarness is a venue that satisfies all three conditions: payments on,
// an acquirer that needs a per-venue account, and that account present.
func connectedHarness(t *testing.T) *capabilityHarness {
	t.Helper()
	h := newCapabilityHarness(t, capabilityOptions{
		paymentsEnabled: true, accountBound: true, wireSplitAccounts: true,
	})
	h.accounts.byRestaurant[h.booking.RestaurantID] = testVenueSplitAccount
	return h
}

func (h *capabilityHarness) accepts(t *testing.T) (bool, error) {
	t.Helper()
	return h.uc.AcceptsOnlinePayment(context.Background(), h.booking.RestaurantID)
}

// TestAcceptsOnlinePaymentTrueWhenTheVenueIsFullyConnected is the only shape in
// which the guest may be shown a payment button.
func TestAcceptsOnlinePaymentTrueWhenTheVenueIsFullyConnected(t *testing.T) {
	h := connectedHarness(t)

	ok, err := h.accepts(t)
	if err != nil {
		t.Fatalf("AcceptsOnlinePayment() error = %v", err)
	}
	if !ok {
		t.Fatalf("accepts = false for a venue with payments on, an acquirer and an account — want true")
	}

	// And the same venue really can be charged: the flag is not a second,
	// looser opinion of what the checkout will do.
	if _, err := h.uc.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: h.booking.ID, IdempotencyKey: "cap-connected",
	}); err != nil {
		t.Fatalf("CreateForBooking() error = %v — the flag promised a payment the checkout refused", err)
	}
}

// TestAcceptsOnlinePaymentFalseOnEachMissingCondition breaks the conjunction
// one condition at a time: each of the three alone is enough to make the venue
// unpayable, and the flag must say so.
func TestAcceptsOnlinePaymentFalseOnEachMissingCondition(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*capabilityHarness)
	}{
		{
			// Condition 1: restaurants.payments_enabled / PAYMENTS_ENABLED.
			name: "payments switched off for the venue",
			break_: func(h *capabilityHarness) {
				h.settings.byRestaurant[h.booking.RestaurantID] = domain.PaymentSettingsOverride{
					PaymentsEnabled: boolPtr(false),
				}
			},
		},
		{
			// Condition 2: no adapter in the registry (production's state —
			// "kaspi adapter not configured, skipping") or every provider row
			// disabled. Both surface as an error wrapping ErrNotFound.
			name: "no acquirer resolves for a new payment",
			break_: func(h *capabilityHarness) {
				h.resolver.resolveErr = fmt.Errorf("no enabled payment provider: %w", domain.ErrNotFound)
			},
		},
		{
			// Condition 3: no restaurant_split_accounts row for this acquirer.
			name: "venue has no account at the acquirer",
			break_: func(h *capabilityHarness) {
				delete(h.accounts.byRestaurant, h.booking.RestaurantID)
			},
		},
		{
			// Condition 3, the sneaky half: a row exists but its account_ref is
			// blank. The adapter trims and refuses it, so the flag must too —
			// an unfinished onboarding is not an onboarding.
			name: "venue account reference is blank",
			break_: func(h *capabilityHarness) {
				h.accounts.byRestaurant[h.booking.RestaurantID] = "   "
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := connectedHarness(t)
			tc.break_(h)

			ok, err := h.accepts(t)
			if err != nil {
				t.Fatalf("AcceptsOnlinePayment() error = %v, want a definite false", err)
			}
			if ok {
				t.Fatalf("accepts = true, want false: %s", tc.name)
			}

			// The claim under test is not "the flag returns false" but "the
			// flag agrees with the checkout": whatever we hid the button for,
			// the payment itself must fail too.
			if _, err := h.uc.CreateForBooking(context.Background(), Actor{}, CreateInput{
				BookingID: h.booking.ID, IdempotencyKey: "cap-" + tc.name,
			}); err == nil {
				t.Fatalf("CreateForBooking() succeeded while the flag said the venue cannot be paid: %s", tc.name)
			}
		})
	}
}

// TestAcceptsOnlinePaymentIgnoresTheAccountForAnAcquirerThatNeedsNone guards the
// opposite mistake: requiring a per-venue account unconditionally would report
// "cannot pay" for every venue on FreedomPay/TipTopPay, which settle onto one
// platform account and are charged perfectly well without one. The flag must
// not be more conservative than the checkout either — a hidden button on a
// working venue is the same class of bug, only quieter.
func TestAcceptsOnlinePaymentIgnoresTheAccountForAnAcquirerThatNeedsNone(t *testing.T) {
	h := newCapabilityHarness(t, capabilityOptions{
		paymentsEnabled: true, accountBound: false, wireSplitAccounts: true,
	})
	// Deliberately no account row for this venue.

	ok, err := h.accepts(t)
	if err != nil {
		t.Fatalf("AcceptsOnlinePayment() error = %v", err)
	}
	if !ok {
		t.Fatalf("accepts = false for an acquirer that needs no per-venue account — want true")
	}
	if _, err := h.uc.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: h.booking.ID, IdempotencyKey: "cap-no-account-needed",
	}); err != nil {
		t.Fatalf("CreateForBooking() error = %v — the flag hid a payment that works", err)
	}
}

// TestAcceptsOnlinePaymentFalseWhenSplitPaymentsDemandAnAccount covers the
// second reason an account becomes mandatory: PAYMENTS_SPLIT_ENABLED, which
// makes resolveSplitPlan refuse a venue without one whatever the acquirer
// thinks (owner's rule, 2026-08-20).
func TestAcceptsOnlinePaymentFalseWhenSplitPaymentsDemandAnAccount(t *testing.T) {
	h := newCapabilityHarness(t, capabilityOptions{
		paymentsEnabled: true, accountBound: false, splitEnabled: true, wireSplitAccounts: true,
	})

	ok, err := h.accepts(t)
	if err != nil {
		t.Fatalf("AcceptsOnlinePayment() error = %v", err)
	}
	if ok {
		t.Fatalf("accepts = true with split payments on and no sub-merchant account — want false")
	}
	if _, err := h.uc.CreateForBooking(context.Background(), Actor{}, CreateInput{
		BookingID: h.booking.ID, IdempotencyKey: "cap-split-no-account",
	}); err == nil {
		t.Fatalf("CreateForBooking() succeeded while the flag said the venue cannot be paid")
	}
}

// TestAcceptsOnlinePaymentFalseWhenTheAcquirerNeedsAnAccountAndNoneCanBeRead
// is the deployment where the mapping is not wired at all: an acquirer that
// needs an address can never be given one, so no venue on it is payable.
func TestAcceptsOnlinePaymentFalseWhenTheAcquirerNeedsAnAccountAndNoneCanBeRead(t *testing.T) {
	h := newCapabilityHarness(t, capabilityOptions{
		paymentsEnabled: true, accountBound: true, wireSplitAccounts: false,
	})

	ok, err := h.accepts(t)
	if err != nil {
		t.Fatalf("AcceptsOnlinePayment() error = %v", err)
	}
	if ok {
		t.Fatalf("accepts = true with no venue↔account mapping wired — want false")
	}
}

// TestAcceptsOnlinePaymentReportsAnErrorRatherThanADefiniteNo separates the two
// answers the payload must never merge: "you cannot pay here" and "we could not
// find out". Only the first is a false; the second has to travel as an error so
// the transport omits the field entirely.
func TestAcceptsOnlinePaymentReportsAnErrorRatherThanADefiniteNo(t *testing.T) {
	readFailed := errors.New("connection refused")

	t.Run("venue settings unreadable", func(t *testing.T) {
		h := connectedHarness(t)
		h.settings.err = readFailed

		if _, err := h.accepts(t); !errors.Is(err, readFailed) {
			t.Fatalf("error = %v, want the underlying read failure", err)
		}
	})

	t.Run("provider registry unreadable", func(t *testing.T) {
		h := connectedHarness(t)
		// An infrastructure failure, NOT one of the registry's "this provider
		// is unusable" answers (which wrap ErrNotFound / ErrValidation).
		h.resolver.resolveErr = fmt.Errorf("read provider %q: %w", domain.ProviderKaspi, readFailed)

		if _, err := h.accepts(t); !errors.Is(err, readFailed) {
			t.Fatalf("error = %v, want the underlying read failure", err)
		}
	})
}

// TestAcceptsOnlinePaymentRejectsAnEmptyRestaurant — a caller with no venue id
// gets a validation error, never a cheerful false.
func TestAcceptsOnlinePaymentRejectsAnEmptyRestaurant(t *testing.T) {
	h := connectedHarness(t)

	if _, err := h.uc.AcceptsOnlinePayment(context.Background(), uuid.Nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want a validation error", err)
	}
}

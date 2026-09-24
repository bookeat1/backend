package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// Venue payment capability — "can this venue take money online at all"
//
// The answer is a CONJUNCTION of three facts, and this file is the only place
// that states it:
//
//  1. payments are enabled for the venue — restaurants.payments_enabled on top
//     of the global PAYMENTS_ENABLED (venueGate.settings);
//  2. an acquirer resolves for a NEW payment — the adapter was built from env
//     AND its payment_providers row is enabled (venueGate.gateway);
//  3. the venue has the acquirer-side account a charge is routed to, for every
//     acquirer that needs one (venueGate.accountUsable).
//
// CreateForBooking runs exactly these three, through these methods, before it
// calls an acquirer; the guest-facing flag (AcceptsOnlinePayment, served as
// accepts_online_payment on the venue detail) runs the same three. Keeping one
// implementation is the point: a second copy of the rule is how an app ends up
// showing a payment button for a venue whose payment the server then refuses —
// which is exactly the bug this flag exists to close.
// ---------------------------------------------------------------------------

// errVenuePaymentsDisabled is the refusal CreateForBooking has always returned
// for a venue that does not take payments — same wording, same domain
// sentinel, so transport keeps mapping it to 422 unchanged. It is a package
// VALUE rather than a fresh error per call so the capability check can
// recognise it with errors.Is and report a definite "no" instead of confusing
// it with a failed read.
var errVenuePaymentsDisabled = fmt.Errorf(
	"%w: payments are not enabled for this restaurant", domain.ErrValidation)

// merchantAccountRequirer is an OPTIONAL acquirer capability: "I refuse to
// charge for a venue that has no account of its own on my side".
//
// It is not part of domain.PaymentGateway because most acquirers settle onto
// one platform account and need no per-venue address at all; the same reasoning
// (and the same type-assertion pattern) as amountGranularity and
// payment.MerchantIDFinder. Kaspi Pay implements it: its money belongs to a
// COMPANY, and an unmapped venue is one nobody finished onboarding — charging
// its guests would credit somebody else's till (see kaspi.validateAuthorize,
// which is the enforcement; this is the same rule asked BEFORE the fact so the
// guest is not offered a button that cannot work).
type merchantAccountRequirer interface {
	RequiresMerchantAccount() bool
}

// venueGate holds the venue-level slice of the checkout's dependencies. It is
// built from the createUseCase's own fields (see createUseCase.gate), never
// wired separately — a separate wiring is how the flag and the checkout would
// come to run on two different configs.
type venueGate struct {
	restaurants restaurantPaymentSettings
	gateways    gatewayResolver
	splits      splitAccountReader
	cfg         Config
}

func (u *createUseCase) gate() venueGate {
	return venueGate{restaurants: u.restaurants, gateways: u.gateways, splits: u.splitAccounts, cfg: u.cfg}
}

// settings resolves the venue's payment settings over the global config and
// refuses a venue whose payments are switched off (check 1).
func (g venueGate) settings(ctx context.Context, restaurantID uuid.UUID) (domain.PaymentSettings, error) {
	override, err := g.restaurants.GetPaymentOverride(ctx, restaurantID)
	if err != nil {
		return domain.PaymentSettings{}, err
	}
	s := resolveSettings(override, g.cfg)
	if !s.Enabled {
		return domain.PaymentSettings{}, errVenuePaymentsDisabled
	}
	return s, nil
}

// account reads the venue's identity at this acquirer
// (restaurant_split_accounts). A venue with no row gets (nil, nil): most
// acquirers settle onto one platform account and need no per-venue address at
// all, and whether a missing mapping is fatal is decided by the callers —
// resolveSplitPlan refuses it when split payments are on, an adapter that needs
// an address refuses an empty ref itself (see kaspi.validateAuthorize), and
// accountUsable below asks both questions ahead of time.
func (g venueGate) account(ctx context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error) {
	if g.splits == nil {
		return nil, nil
	}
	account, err := g.splits.GetActive(ctx, provider, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return account, nil
}

// accountRequired reports whether a charge for this venue would be refused
// without an acquirer-side account. Two independent reasons, both already
// enforced elsewhere in the checkout:
//
//   - split payments are on and the mapping is wired: resolveSplitPlan hard-
//     stops a venue without an account (CodeSplitAccountMissing);
//   - the resolved adapter says it needs one (merchantAccountRequirer) —
//     Kaspi Pay does.
func (g venueGate) accountRequired(gw domain.PaymentGateway) bool {
	if g.cfg.SplitEnabled && g.splits != nil {
		return true
	}
	r, ok := gw.(merchantAccountRequirer)
	return ok && r.RequiresMerchantAccount()
}

// accountUsable answers check 3: does this venue have the account its acquirer
// needs, with a non-empty reference? A blank reference counts as MISSING, the
// same trimmed comparison the Kaspi adapter makes — an account row somebody
// created and never filled in must not read as "onboarded".
//
// (false, nil) is a definite "this venue cannot be charged"; a non-nil error is
// "we could not find out", which callers must not publish as either answer.
func (g venueGate) accountUsable(ctx context.Context, gw domain.PaymentGateway, restaurantID uuid.UUID) (bool, error) {
	if !g.accountRequired(gw) {
		return true, nil
	}
	if g.splits == nil {
		// The acquirer needs an address and this deployment has no mapping to
		// read it from: no venue on it can be charged through this acquirer.
		return false, nil
	}
	account, err := g.account(ctx, gw.Name(), restaurantID)
	if err != nil {
		return false, err
	}
	return account != nil && strings.TrimSpace(account.AccountRef) != "", nil
}

// AcceptsOnlinePayment reports whether this VENUE could take an online payment
// right now — the three checks above, in the order CreateForBooking makes them.
//
// It is a statement about the VENUE, not about any one booking: it says nothing
// about whether a particular booking owes anything, is still cancellable, or
// already has a live payment. The guest app uses it to decide whether to offer
// a payment button at all.
//
// The (bool, error) split is deliberate and mirrors the venue-state contract:
//   - (false, nil) — a definite no, and an ordinary one (payments off, no
//     acquirer configured, venue not onboarded at this acquirer);
//   - (_, err) — we could NOT find out (a failed read). Callers must leave the
//     field out of the payload entirely rather than publish a guess, because
//     "we did not compute it" and "you may not pay" are different statements.
func (u *createUseCase) AcceptsOnlinePayment(ctx context.Context, restaurantID uuid.UUID) (bool, error) {
	methods, err := u.AvailablePaymentMethods(ctx, restaurantID)
	if err != nil {
		return false, err
	}
	return len(methods) > 0, nil
}

// AvailablePaymentMethods lists the methods (kaspi, card — in that order) a
// guest could actually pay this venue with right now: the venue's master switch
// is on, the method is switched on for the venue, an acquirer resolves for it
// and, where that acquirer needs a per-venue account (Kaspi), the venue has one.
// A method that is switched on but not usable (Kaspi without a bound account, no
// enabled card provider) is NOT listed. Same (definite / error) contract as
// AcceptsOnlinePayment.
func (u *createUseCase) AvailablePaymentMethods(ctx context.Context, restaurantID uuid.UUID) ([]domain.PaymentMethod, error) {
	if restaurantID == uuid.Nil {
		return nil, fmt.Errorf("%w: restaurant required", domain.ErrValidation)
	}
	g := u.gate()
	settings, err := g.settings(ctx, restaurantID)
	if err != nil {
		if errors.Is(err, errVenuePaymentsDisabled) {
			return []domain.PaymentMethod{}, nil
		}
		return nil, err
	}
	out := []domain.PaymentMethod{}
	for _, m := range domain.PaymentMethods {
		_, ok, err := g.methodGateway(ctx, restaurantID, settings, m)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// methodEnabled reports whether the venue has switched the method on.
func methodEnabled(s domain.PaymentSettings, m domain.PaymentMethod) bool {
	switch m {
	case domain.MethodKaspi:
		return s.KaspiEnabled
	case domain.MethodCard:
		return s.CardEnabled
	}
	return false
}

// methodGateway is checks 2 and 3 for one method (check 1, the master switch,
// is g.settings). (nil, false, nil) is a definite "not available"; an error is
// "could not find out".
func (g venueGate) methodGateway(ctx context.Context, restaurantID uuid.UUID, s domain.PaymentSettings, m domain.PaymentMethod) (domain.PaymentGateway, bool, error) {
	if !methodEnabled(s, m) {
		return nil, false, nil
	}
	gw, err := g.gateways.ResolveMethod(ctx, m)
	if err != nil {
		if providerUnusable(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	ok, err := g.accountUsable(ctx, gw, restaurantID)
	if err != nil || !ok {
		return nil, false, err
	}
	return gw, true, nil
}

// resolveEnabledMethod is methodGateway WITHOUT the account check: the method is
// switched on and an acquirer resolves for it. (nil, false, nil) = definite no.
func (g venueGate) resolveEnabledMethod(ctx context.Context, s domain.PaymentSettings, m domain.PaymentMethod) (domain.PaymentGateway, bool, error) {
	if !methodEnabled(s, m) {
		return nil, false, nil
	}
	gw, err := g.gateways.ResolveMethod(ctx, m)
	if err != nil {
		if providerUnusable(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return gw, true, nil
}

// pickMethodGateway chooses the acquirer for a NEW payment.
//
// With an explicit method that method is used or the call is refused with a
// 422 — never a silent switch to the other one. Without one (older clients) the
// venue's legacy preferred provider orders the methods (kaspi first when it
// preferred kaspi, otherwise card first) and the first AVAILABLE one wins.
//
// A method that is enabled and has an acquirer but no usable venue account is
// deliberately still returned when nothing better exists: the checkout then
// fails further down with the specific, long-standing error (split_account_missing
// / the adapter's refusal) instead of a generic one.
func (g venueGate) pickMethodGateway(ctx context.Context, restaurantID uuid.UUID, s domain.PaymentSettings, requested domain.PaymentMethod) (domain.PaymentGateway, error) {
	notAvailable := func(m domain.PaymentMethod) error {
		return fmt.Errorf("%w: payment method %s is not available for this restaurant", domain.ErrValidation, m)
	}
	if requested != "" {
		if !requested.Valid() {
			return nil, fmt.Errorf("%w: unknown payment method %q", domain.ErrValidation, requested)
		}
		gw, ok, err := g.resolveEnabledMethod(ctx, s, requested)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, notAvailable(requested)
		}
		return gw, nil
	}
	order := []domain.PaymentMethod{domain.MethodCard, domain.MethodKaspi}
	if s.Provider == domain.ProviderKaspi {
		order = []domain.PaymentMethod{domain.MethodKaspi, domain.MethodCard}
	}
	for _, m := range order {
		gw, ok, err := g.resolveEnabledMethod(ctx, s, m)
		if err != nil {
			return nil, err
		}
		if ok {
			return gw, nil
		}
	}
	return nil, errVenuePaymentsDisabled
}

// providerUnusable separates "this deployment cannot take a new payment through
// any acquirer" from "the lookup itself failed".
//
// The registry reports the first case with errors wrapping domain.ErrNotFound
// (the adapter is not configured / nothing is enabled) or domain.ErrValidation
// (the provider code is unknown / its row is switched off) — all of them
// ordinary states of a platform that is simply not taking money, exactly the
// state production is in with an empty registry. Anything else (a failed read
// of payment_providers) wraps neither and must surface as an error, so the
// field is omitted instead of published as "нельзя платить".
//
// Matched on the domain sentinels rather than on infrastructure/payment's own
// error values on purpose: a usecase must not import an infrastructure package
// to ask a question about its own port.
func providerUnusable(err error) bool {
	return errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrValidation)
}

package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// Which of an acquirer's accounts a venue's money lands in
//
// For most acquirers this is nothing: the charge lands on the platform's one
// merchant account and is settled by payout afterwards. Kaspi Pay is the
// exception this exists for — it is reached through our own multi-tenant Kaspi
// service, where a venue's money belongs to a COMPANY, and which company is a
// per-venue fact nobody can derive. Without it the adapter refuses to create a
// payment link at all (kaspi.validateAuthorize), on purpose: "the default
// company" would mean crediting somebody else's till.
//
// The mapping lives in restaurant_split_accounts (migration 0077, opened up
// for Kaspi in 0091). Only the ADDRESS is stored here; the company's API key
// is env-only and never enters the database.
// ---------------------------------------------------------------------------

// acquirerAccountStore is the slice of the split-account repo this package
// needs. Implemented by *payment.SplitAccounts.
type acquirerAccountStore interface {
	GetActive(ctx context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error)
	Upsert(ctx context.Context, a *domain.RestaurantSplitAccount) error
}

// Option configures optional collaborators of the panel. It exists so a
// capability that only some deployments wire (acquirer accounts) does not
// force every call site of NewUseCase to pass a nil.
type Option func(*UseCase)

// WithAcquirerAccounts enables the acquirer-account endpoints. Without it they
// answer domain.ErrUnavailable rather than panicking on a nil store.
func WithAcquirerAccounts(store acquirerAccountStore) Option {
	return func(u *UseCase) { u.acquirerAccounts = store }
}

// AcquirerAccount is the panel's view of one venue↔acquirer mapping.
type AcquirerAccount struct {
	Provider domain.PaymentProvider
	// AccountRef is the venue's id AT THE ACQUIRER — for Kaspi, the company id
	// inside our Kaspi service. Empty means the venue has no mapping.
	AccountRef string
	IsActive   bool
}

// GetAcquirerAccount reports which of the acquirer's accounts this venue's
// money is routed to. Readable by owner/manager (PermRestaurantManage): a
// venue is entitled to see where its own money goes, and the value is an
// address, not a credential.
//
// A venue with no mapping is NOT an error — it is the ordinary state of every
// venue that has not been onboarded to this acquirer yet, and the panel needs
// to render exactly that.
func (u *UseCase) GetAcquirerAccount(ctx context.Context, actor Actor, restaurantID uuid.UUID, provider domain.PaymentProvider) (AcquirerAccount, error) {
	if err := u.authorize(ctx, actor, restaurantID, domain.PermRestaurantManage); err != nil {
		return AcquirerAccount{}, err
	}
	if err := validateAcquirerProvider(provider); err != nil {
		return AcquirerAccount{}, err
	}
	if u.acquirerAccounts == nil {
		return AcquirerAccount{}, fmt.Errorf("%w: acquirer accounts are not configured in this deployment", domain.ErrUnavailable)
	}
	account, err := u.acquirerAccounts.GetActive(ctx, provider, restaurantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return AcquirerAccount{Provider: provider}, nil
		}
		return AcquirerAccount{}, err
	}
	return AcquirerAccount{Provider: account.Provider, AccountRef: account.AccountRef, IsActive: account.IsActive}, nil
}

// SetAcquirerAccount points a venue's money at one of the acquirer's accounts.
//
// SUPERADMIN ONLY, and deliberately narrower than every other setting in this
// panel: this single string decides whose bank account a guest's money ends up
// in. A venue owner who could edit it could route another venue's takings —
// or their own — to a company they control. Onboarding is a platform act.
//
// Setting IsActive=false suspends the venue from this acquirer without losing
// the record of where historic payments went; the checkout treats an inactive
// mapping exactly like a missing one, so the next payment attempt is refused
// rather than misrouted.
func (u *UseCase) SetAcquirerAccount(ctx context.Context, actor Actor, restaurantID uuid.UUID, in AcquirerAccount) (AcquirerAccount, error) {
	if actor.UserID == uuid.Nil {
		return AcquirerAccount{}, fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role != domain.RoleAdmin {
		return AcquirerAccount{}, fmt.Errorf(
			"%w: routing a venue's money to an acquirer account is a platform action", domain.ErrForbidden)
	}
	if err := validateAcquirerProvider(in.Provider); err != nil {
		return AcquirerAccount{}, err
	}
	ref := strings.TrimSpace(in.AccountRef)
	if ref == "" {
		return AcquirerAccount{}, fmt.Errorf("%w: account_ref must not be empty", domain.ErrValidation)
	}
	if len(ref) > maxAcquirerAccountRefLen {
		return AcquirerAccount{}, fmt.Errorf("%w: account_ref is longer than %d characters",
			domain.ErrValidation, maxAcquirerAccountRefLen)
	}
	if u.acquirerAccounts == nil {
		return AcquirerAccount{}, fmt.Errorf("%w: acquirer accounts are not configured in this deployment", domain.ErrUnavailable)
	}

	account := &domain.RestaurantSplitAccount{
		RestaurantID: restaurantID, Provider: in.Provider, AccountRef: ref, IsActive: in.IsActive,
	}
	if err := u.acquirerAccounts.Upsert(ctx, account); err != nil {
		return AcquirerAccount{}, err
	}
	return AcquirerAccount{Provider: account.Provider, AccountRef: account.AccountRef, IsActive: account.IsActive}, nil
}

// maxAcquirerAccountRefLen is our own sanity bound — the column is plain
// `text` (0077) — so that a pasted page of HTML is refused with a 422 and a
// reason rather than being stored as somebody's "account".
const maxAcquirerAccountRefLen = 128

func validateAcquirerProvider(p domain.PaymentProvider) error {
	if !p.Valid() {
		return fmt.Errorf("%w: unknown payment provider %q", domain.ErrValidation, p)
	}
	return nil
}

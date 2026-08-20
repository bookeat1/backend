package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// SplitAccounts implements domain.RestaurantSplitAccountRepository — the
// venue↔sub-merchant mapping split payments are addressed by
// (restaurant_split_accounts, migration 0077).
type SplitAccounts struct{ pool sqltx.Querier }

// NewSplitAccounts builds the split-account repository.
func NewSplitAccounts(pool sqltx.Querier) *SplitAccounts { return &SplitAccounts{pool: pool} }

var _ domain.RestaurantSplitAccountRepository = (*SplitAccounts)(nil)

const splitAccountCols = `restaurant_id, provider, account_ref, is_active, created_at, updated_at`

// GetActive resolves the venue's account at one acquirer. A venue that was
// never onboarded and a venue whose account was deactivated are the SAME answer
// on purpose (domain.ErrNotFound): the checkout must refuse a split payment in
// both cases, and giving them separate outcomes only invites a caller to treat
// one of them as "then charge it all to the marketplace".
func (r *SplitAccounts) GetActive(ctx context.Context, provider domain.PaymentProvider, restaurantID uuid.UUID) (*domain.RestaurantSplitAccount, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+splitAccountCols+` FROM restaurant_split_accounts
		 WHERE provider = $1 AND restaurant_id = $2 AND is_active`,
		string(provider), restaurantID)

	a, err := scanSplitAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get restaurant split account: %w", err)
	}
	return a, nil
}

// Upsert writes the mapping for (restaurant, provider). The unique index on an
// ACTIVE (provider, account_ref) is the guard that one acquirer account cannot
// be claimed by two venues; a violation arrives here as
// domain.ErrAlreadyExists via mapWrite, never as a raw driver error.
func (r *SplitAccounts) Upsert(ctx context.Context, a *domain.RestaurantSplitAccount) error {
	if a == nil {
		return domain.ErrValidation
	}
	if err := a.Validate(); err != nil {
		return err
	}
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`INSERT INTO restaurant_split_accounts (restaurant_id, provider, account_ref, is_active)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (restaurant_id, provider)
		 DO UPDATE SET account_ref = EXCLUDED.account_ref,
		               is_active   = EXCLUDED.is_active,
		               updated_at  = now()
		 RETURNING `+splitAccountCols,
		a.RestaurantID, string(a.Provider), strings.TrimSpace(a.AccountRef), a.IsActive)

	stored, err := scanSplitAccount(row)
	if err != nil {
		return mapWrite(err, "upsert restaurant split account")
	}
	*a = *stored
	return nil
}

func scanSplitAccount(row scanner) (*domain.RestaurantSplitAccount, error) {
	var a domain.RestaurantSplitAccount
	var provider string
	if err := row.Scan(&a.RestaurantID, &provider, &a.AccountRef, &a.IsActive, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Provider = domain.PaymentProvider(provider)
	return &a, nil
}

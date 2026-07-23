package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Providers implements domain.PaymentProviderRepository — the admin-editable
// acquirer registry (payment_providers). It is what
// internal/infrastructure/payment.Registry needs to resolve a gateway for a
// NEW payment; ForRefund-style lookups never go through this at all (spec
// §9.1: a disabled acquirer must keep working for money it already touched).
type Providers struct{ pool sqltx.Querier }

// NewProviders builds the acquirer-registry repository.
func NewProviders(pool sqltx.Querier) *Providers { return &Providers{pool: pool} }

var _ domain.PaymentProviderRepository = (*Providers)(nil)

const providerCols = `provider, is_enabled, is_default, priority, created_at, updated_at`

func (r *Providers) List(ctx context.Context) ([]domain.PaymentProviderSetting, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+providerCols+` FROM payment_providers ORDER BY priority`)
	if err != nil {
		return nil, fmt.Errorf("list payment providers: %w", err)
	}
	defer rows.Close()
	return scanProviders(rows)
}

func (r *Providers) ListEnabled(ctx context.Context) ([]domain.PaymentProviderSetting, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+providerCols+` FROM payment_providers WHERE is_enabled ORDER BY priority`)
	if err != nil {
		return nil, fmt.Errorf("list enabled payment providers: %w", err)
	}
	defer rows.Close()
	return scanProviders(rows)
}

func (r *Providers) GetByCode(ctx context.Context, provider domain.PaymentProvider) (*domain.PaymentProviderSetting, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+providerCols+` FROM payment_providers WHERE provider=$1`, string(provider))
	s, err := scanProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get payment provider: %w", err)
	}
	return s, nil
}

// GetDefault returns the enabled default provider. "No enabled acquirer" is a
// legitimate state (the platform is not taking money right now) and is
// reported as domain.ErrNotFound, never guessed around.
func (r *Providers) GetDefault(ctx context.Context) (*domain.PaymentProviderSetting, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+providerCols+` FROM payment_providers WHERE is_default AND is_enabled`)
	s, err := scanProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get default payment provider: %w", err)
	}
	return s, nil
}

// Update writes the admin-editable flags. Only one row may carry is_default —
// idx_payment_providers_default enforces it, and a violation here (setting
// is_default=true on a second row) is mapped to domain.ErrAlreadyExists by
// mapWrite rather than surfacing as a raw driver error.
func (r *Providers) Update(ctx context.Context, s *domain.PaymentProviderSetting) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE payment_providers SET is_enabled=$2, is_default=$3, priority=$4, updated_at=now()
		 WHERE provider=$1`,
		string(s.Provider), s.IsEnabled, s.IsDefault, s.Priority)
	if err != nil {
		return mapWrite(err, "update payment provider")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func scanProviders(rows pgx.Rows) ([]domain.PaymentProviderSetting, error) {
	var out []domain.PaymentProviderSetting
	for rows.Next() {
		s, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("scan payment provider: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func scanProvider(row scanner) (*domain.PaymentProviderSetting, error) {
	var s domain.PaymentProviderSetting
	var provider string
	if err := row.Scan(&provider, &s.IsEnabled, &s.IsDefault, &s.Priority, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.Provider = domain.PaymentProvider(provider)
	return &s, nil
}

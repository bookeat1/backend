package payout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Destinations implements domain.PayoutDestinationRepository.
type Destinations struct{ pool sqltx.Querier }

// NewDestinations builds the destination repository.
func NewDestinations(pool sqltx.Querier) *Destinations { return &Destinations{pool: pool} }

var _ domain.PayoutDestinationRepository = (*Destinations)(nil)

const destinationCols = `id, restaurant_id, provider, method, token, provider_customer_ref, masked_identifier, created_at, updated_at`

// Upsert stores the restaurant's single destination, replacing an existing one
// in place (idempotent by uq_payout_destinations_restaurant). A raw PAN never
// reaches here — domain.PayoutDestination.Validate rejects it in the usecase
// before this is called, and there is no column that could hold one.
func (r *Destinations) Upsert(ctx context.Context, d *domain.PayoutDestination) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`INSERT INTO restaurant_payout_destinations (`+destinationCols+`)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (restaurant_id) DO UPDATE SET
			provider              = EXCLUDED.provider,
			method                = EXCLUDED.method,
			token                 = EXCLUDED.token,
			provider_customer_ref = EXCLUDED.provider_customer_ref,
			masked_identifier     = EXCLUDED.masked_identifier,
			updated_at            = EXCLUDED.updated_at`,
		d.ID, d.RestaurantID, string(d.Provider), string(d.Method), d.Token, d.ProviderCustomerRef,
		d.MaskedIdentifier, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return mapWrite(err, "upsert payout destination")
	}
	return nil
}

// Get returns the restaurant's destination or domain.ErrNotFound.
func (r *Destinations) Get(ctx context.Context, restaurantID uuid.UUID) (*domain.PayoutDestination, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+destinationCols+` FROM restaurant_payout_destinations WHERE restaurant_id=$1`,
		restaurantID)
	d, err := scanDestination(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get payout destination: %w", err)
	}
	return d, nil
}

func scanDestination(row scanner) (*domain.PayoutDestination, error) {
	var d domain.PayoutDestination
	var provider, method string
	if err := row.Scan(&d.ID, &d.RestaurantID, &provider, &method, &d.Token,
		&d.ProviderCustomerRef, &d.MaskedIdentifier, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.Provider = domain.PaymentProvider(provider)
	d.Method = domain.PayoutMethod(method)
	return &d, nil
}

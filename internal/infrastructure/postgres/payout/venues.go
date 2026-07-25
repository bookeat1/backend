package payout

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Venues implements domain.PayoutVenueReader: the one restaurant fact the
// payout module needs, read through its own narrow port rather than by
// depending on the restaurant repository.
type Venues struct{ pool sqltx.Querier }

// NewVenues builds the venue reader for the daily payout pass.
func NewVenues(pool sqltx.Querier) *Venues { return &Venues{pool: pool} }

var _ domain.PayoutVenueReader = (*Venues)(nil)

// TimezonesFor resolves every requested venue's IANA zone in ONE query. A venue
// whose timezone is NULL or empty is simply absent from the result — the caller
// then applies the platform fallback rather than this layer guessing one, so
// "no zone configured" stays a visible decision instead of being buried in SQL.
func (r *Venues) TimezonesFor(ctx context.Context, restaurantIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(restaurantIDs))
	if len(restaurantIDs) == 0 {
		return out, nil
	}
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT id, timezone FROM restaurants
		 WHERE id = ANY($1) AND timezone IS NOT NULL AND timezone <> ''`,
		restaurantIDs)
	if err != nil {
		return nil, fmt.Errorf("read venue timezones: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var tz string
		if err := rows.Scan(&id, &tz); err != nil {
			return nil, fmt.Errorf("scan venue timezone: %w", err)
		}
		out[id] = tz
	}
	return out, rows.Err()
}

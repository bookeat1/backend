// Package guest is a read-only Postgres read model for the restaurant admin
// panel's guest list, aggregated from the bookings table (there is no guests
// table — a guest exists only as the person on one or more bookings).
package guest

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository reads aggregated guest rows for a restaurant.
type Repository struct{ pool sqltx.Querier }

// New builds the guest read-model repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

// ListByRestaurant returns one row per distinct guest (grouped by the stable
// phone_normalized identity) who has ever booked restaurantID, most recent
// booking first. Scalar fields (name/phone/email/user_id) are taken from the
// guest's MOST RECENT booking via array_agg(... ORDER BY created_at DESC)[1];
// user_id additionally filters out NULLs so an account is surfaced if ANY of
// the guest's bookings carried one. Every field is derived from bookings —
// strictly read-only, no cross-tenant surface (the restaurant_id predicate is
// the tenant guard, backed by idx_bookings_restaurant_phone).
func (r *Repository) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantGuest, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT
			(array_agg(user_id ORDER BY created_at DESC) FILTER (WHERE user_id IS NOT NULL))[1] AS user_id,
			(array_agg(name    ORDER BY created_at DESC))[1] AS name,
			(array_agg(phone   ORDER BY created_at DESC))[1] AS phone,
			phone_normalized,
			(array_agg(email   ORDER BY created_at DESC))[1] AS email,
			count(*)                                                          AS bookings_count,
			count(*) FILTER (WHERE status IN ('arrived', 'completed'))        AS visits_count,
			min(created_at)                                                   AS first_booking_at,
			max(created_at)                                                   AS last_booking_at
		 FROM bookings
		 WHERE restaurant_id = $1
		 GROUP BY phone_normalized
		 ORDER BY max(created_at) DESC`, restaurantID)
	if err != nil {
		return nil, fmt.Errorf("list restaurant guests: %w", err)
	}
	defer rows.Close()

	var out []domain.RestaurantGuest
	for rows.Next() {
		var g domain.RestaurantGuest
		if err := rows.Scan(&g.UserID, &g.Name, &g.Phone, &g.PhoneNormalized, &g.Email,
			&g.BookingsCount, &g.VisitsCount, &g.FirstBookingAt, &g.LastBookingAt); err != nil {
			return nil, fmt.Errorf("scan restaurant guest: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list restaurant guests: %w", err)
	}
	return out, nil
}

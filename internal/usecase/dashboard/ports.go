package dashboard

import (
	"context"

	"backend-core/internal/domain"
)

// readRepo is the narrow read-model port the dashboard usecase depends on,
// satisfied by internal/infrastructure/postgres/dashboard.Repository. Kept
// local (not a domain interface) so it never forces churn on unrelated fakes —
// same convention as usecase/restaurants' my-restaurants ports.
type readRepo interface {
	Overview(ctx context.Context) (domain.PlatformOverview, error)
	BookingsByStatus(ctx context.Context, from, to any) ([]domain.BookingStatusCount, error)
	PaymentsGMV(ctx context.Context, from, to any, currency string) (domain.MoneyAggregate, domain.MoneyAggregate, error)
	TopRestaurantsByBookings(ctx context.Context, from, to any, limit int) ([]domain.TopRestaurant, error)
	TopRestaurantsByGMV(ctx context.Context, from, to any, currency string, limit int) ([]domain.TopRestaurant, error)
}

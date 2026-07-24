package payouts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// UseCase orchestrates payout destinations, generation, sending and listing.
type UseCase struct {
	perms        permissionChecker
	destinations domain.PayoutDestinationRepository
	payouts      domain.PayoutRepository
	items        domain.PayoutItemRepository
	owed         domain.OwedReader
	gateway      domain.PayoutGateway
	tx           domain.TxManager
	log          *slog.Logger
	now          func() time.Time
}

// NewUseCase builds the payout usecase.
func NewUseCase(p Ports, log *slog.Logger) *UseCase {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &UseCase{
		perms:        p.Perms,
		destinations: p.Destinations,
		payouts:      p.Payouts,
		items:        p.Items,
		owed:         p.Owed,
		gateway:      p.Gateway,
		tx:           p.Tx,
		log:          log,
		now:          time.Now,
	}
}

// authorizeRestaurant is the RBAC gate for a venue-scoped action (setting /
// reading a destination). A superadmin (global RoleAdmin) always passes; anyone
// else must hold perm AT restaurantID — which is what makes cross-tenant access
// impossible: a caller with no staff row at restaurantID holds no permission
// there, so the id from the request path is only ever the key the permission is
// checked against, never trusted on its own.
func (u *UseCase) authorizeRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, perm domain.Permission) error {
	if actor.UserID == uuid.Nil {
		return fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := u.perms.HasPermission(ctx, actor.UserID, restaurantID, perm)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: insufficient permission for this restaurant", domain.ErrForbidden)
	}
	return nil
}

// authorizeSuperadmin gates a money-OUT action (generate / send / reconcile a
// payout). Paying venues moves BookEat's own money, so it is a platform
// operation restricted to the global superadmin — a venue owner sets WHERE
// their money goes, but never triggers the disbursement itself.
func (u *UseCase) authorizeSuperadmin(actor Actor) error {
	if actor.UserID == uuid.Nil {
		return fmt.Errorf("%w: no authenticated actor", domain.ErrUnauthorized)
	}
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: payouts are a platform (superadmin) operation", domain.ErrForbidden)
	}
	return nil
}

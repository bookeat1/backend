package tickets

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// AdminUseCase is the restaurant-cabinet view of ticket sales: list the tickets
// sold for an event and the aggregate counts/revenue. Tenant-scoped: a caller
// must hold PermRestaurantManage at the EVENT's own restaurant (resolved from
// the event, never trusted from the request), a superadmin bypasses.
type AdminUseCase interface {
	// ListSold returns an event's tickets (optionally status-filtered), newest
	// first, paginated, plus the total. Requires PermRestaurantManage.
	ListSold(ctx context.Context, actor Actor, eventID uuid.UUID, statuses []domain.TicketStatus, page, perPage int) ([]domain.EventTicket, int, error)
	// Counts returns the tickets-sold aggregate (counts + paid revenue) for an
	// event. Requires PermRestaurantManage.
	Counts(ctx context.Context, actor Actor, eventID uuid.UUID) (domain.EventTicketCounts, error)
}

type adminUseCase struct {
	tickets domain.EventTicketRepository
	events  eventReader
	perms   permissionChecker
}

// NewAdminUseCase constructs the admin ticket-sales usecase.
func NewAdminUseCase(tickets domain.EventTicketRepository, events eventReader, perms permissionChecker) AdminUseCase {
	return &adminUseCase{tickets: tickets, events: events, perms: perms}
}

func (u *adminUseCase) ListSold(ctx context.Context, actor Actor, eventID uuid.UUID, statuses []domain.TicketStatus, page, perPage int) ([]domain.EventTicket, int, error) {
	if err := u.authorize(ctx, actor, eventID); err != nil {
		return nil, 0, err
	}
	return u.tickets.ListByEvent(ctx, eventID, statuses, page, perPage)
}

func (u *adminUseCase) Counts(ctx context.Context, actor Actor, eventID uuid.UUID) (domain.EventTicketCounts, error) {
	if err := u.authorize(ctx, actor, eventID); err != nil {
		return domain.EventTicketCounts{}, err
	}
	return u.tickets.Counts(ctx, eventID)
}

// authorize resolves the event first (cross-tenant IDOR guard) and checks
// PermRestaurantManage at ITS OWN restaurant — RoleAdmin bypasses.
func (u *adminUseCase) authorize(ctx context.Context, actor Actor, eventID uuid.UUID) error {
	event, err := u.events.GetByID(ctx, eventID)
	if err != nil {
		return err
	}
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	if actor.UserID == nil {
		return fmt.Errorf("%w: no authenticated staff actor", domain.ErrUnauthorized)
	}
	ok, err := u.perms.HasPermission(ctx, *actor.UserID, event.RestaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: cannot view ticket sales for this restaurant", domain.ErrForbidden)
	}
	return nil
}

// MyTicketsUseCase lists a guest's own tickets — own-user scoped, no
// cross-user surface at all.
type MyTicketsUseCase interface {
	ListMine(ctx context.Context, userID uuid.UUID, page, perPage int) ([]domain.EventTicket, int, error)
}

type myTicketsUseCase struct {
	tickets domain.EventTicketRepository
}

// NewMyTicketsUseCase constructs the guest own-tickets usecase.
func NewMyTicketsUseCase(tickets domain.EventTicketRepository) MyTicketsUseCase {
	return &myTicketsUseCase{tickets: tickets}
}

func (u *myTicketsUseCase) ListMine(ctx context.Context, userID uuid.UUID, page, perPage int) ([]domain.EventTicket, int, error) {
	return u.tickets.ListByUser(ctx, userID, page, perPage)
}

package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ManagerUseCase manages restaurant↔user manager assignments and answers the
// "does this user manage this restaurant?" question used to gate the back office.
type ManagerUseCase interface {
	List(ctx context.Context, restaurantID uuid.UUID) ([]domain.RestaurantManager, error)
	Assign(ctx context.Context, in AssignManagerInput) (*domain.RestaurantManager, error)
	Remove(ctx context.Context, id uuid.UUID) error
	Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error)
}

type managerUseCase struct {
	managers domain.RestaurantManagerRepository
	users    userReader
}

// NewManagerUseCase constructs the ManagerUseCase.
func NewManagerUseCase(managers domain.RestaurantManagerRepository, users userReader) ManagerUseCase {
	return &managerUseCase{managers: managers, users: users}
}

// AssignManagerInput assigns a user as a manager of a restaurant.
type AssignManagerInput struct {
	RestaurantID  uuid.UUID
	UserID        uuid.UUID
	CreatedBy     *uuid.UUID
	WhatsappOptIn bool
	WhatsappPhone *string
}

func (u *managerUseCase) List(ctx context.Context, rid uuid.UUID) ([]domain.RestaurantManager, error) {
	return u.managers.ListByRestaurant(ctx, rid)
}

func (u *managerUseCase) Assign(ctx context.Context, in AssignManagerInput) (*domain.RestaurantManager, error) {
	if _, err := u.users.GetByID(ctx, in.UserID); err != nil {
		return nil, err // ErrNotFound when the assignee doesn't exist
	}
	m := &domain.RestaurantManager{
		RestaurantID: in.RestaurantID, UserID: in.UserID, CreatedBy: in.CreatedBy,
		WhatsappOptIn: in.WhatsappOptIn, WhatsappPhone: in.WhatsappPhone,
	}
	if err := u.managers.Create(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (u *managerUseCase) Remove(ctx context.Context, id uuid.UUID) error {
	return u.managers.Delete(ctx, id)
}

func (u *managerUseCase) Manages(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	ms, err := u.managers.ListByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	for _, m := range ms {
		if m.RestaurantID == restaurantID {
			return true, nil
		}
	}
	return false, nil
}

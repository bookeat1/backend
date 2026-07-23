package bookings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ExternalHoldInput is a request to record occupancy that originated outside
// BookEat (a phone booking, a walk-in, a POS push). TableID nil means a
// whole-venue block for the window.
type ExternalHoldInput struct {
	TableID     *uuid.UUID
	StartsAt    time.Time
	EndsAt      time.Time
	Source      domain.ExternalSource
	ExternalRef *string
	Note        *string
}

// ExternalReservationUseCase records and removes external occupancy holds. It is
// the staff-facing seam today and the write target a future POS/Kwaaka webhook
// will call: both land in the same availability engine, because every hold is
// enforced by the GiST exclusion constraint on booking_tables — the same
// constraint that guards native bookings.
//
// Every mutation requires domain.PermBookingManage at the target restaurant
// (RBAC matrix in internal/domain/rbac.go); an admin bypasses the venue scope,
// same as everywhere else in this package.
type ExternalReservationUseCase interface {
	Create(ctx context.Context, actor Actor, restaurantID uuid.UUID, in ExternalHoldInput) (*domain.ExternalReservation, error)
	Delete(ctx context.Context, actor Actor, restaurantID, id uuid.UUID) error
	List(ctx context.Context, actor Actor, restaurantID uuid.UUID, from, to time.Time) ([]domain.ExternalReservation, error)
}

// staffPermissionChecker is the slice of restaurants.ManagerUseCase this usecase
// needs: whether a user holds a specific domain.Permission at a restaurant. Kept
// separate from the package's role-agnostic managerChecker so the existing
// availability/create fakes do not have to grow a method they never use — same
// split as usecase/payments.managerChecker.
type staffPermissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

type externalReservationUseCase struct {
	holds       domain.ExternalReservationRepository
	restaurants restaurantReader
	schedule    scheduleReader
	perms       staffPermissionChecker
	tx          domain.TxManager
}

// NewExternalReservationUseCase constructs the external-hold usecase.
func NewExternalReservationUseCase(
	holds domain.ExternalReservationRepository,
	restaurants restaurantReader,
	schedule scheduleReader,
	perms staffPermissionChecker,
	tx domain.TxManager,
) ExternalReservationUseCase {
	return &externalReservationUseCase{
		holds: holds, restaurants: restaurants, schedule: schedule, perms: perms, tx: tx,
	}
}

func (u *externalReservationUseCase) Create(ctx context.Context, actor Actor, restaurantID uuid.UUID, in ExternalHoldInput) (*domain.ExternalReservation, error) {
	if err := u.authorize(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	source := in.Source
	if source == "" {
		source = domain.ExtSourceManual
	}
	if !source.Valid() {
		return nil, fmt.Errorf("%w: unknown external source", domain.ErrValidation)
	}
	starts, ends := in.StartsAt.UTC(), in.EndsAt.UTC()
	if starts.IsZero() || ends.IsZero() {
		return nil, fmt.Errorf("%w: starts_at and ends_at required", domain.ErrValidation)
	}
	if !ends.After(starts) {
		return nil, fmt.Errorf("%w: ends_at must be after starts_at", domain.ErrValidation)
	}

	// The venue must exist. It need NOT be is_active: a place paused on BookEat
	// still takes phone bookings, and blocking those slots is the whole point.
	if _, err := u.restaurants.GetByID(ctx, restaurantID); err != nil {
		return nil, err
	}

	enforce, err := u.enforceTables(ctx, restaurantID, in.TableID)
	if err != nil {
		return nil, err
	}

	res := &domain.ExternalReservation{
		ID:           uuid.New(),
		RestaurantID: restaurantID,
		TableID:      in.TableID,
		StartsAt:     starts,
		EndsAt:       ends,
		Source:       source,
		ExternalRef:  trimPtr(in.ExternalRef),
		Note:         trimPtr(in.Note),
		CreatedBy:    actorID(actor),
		Active:       true,
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		return u.holds.Create(ctx, res, enforce)
	})
	if err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: the slot is already occupied", domain.ErrAlreadyExists)
		}
		return nil, err
	}
	return res, nil
}

func (u *externalReservationUseCase) Delete(ctx context.Context, actor Actor, restaurantID, id uuid.UUID) error {
	res, err := u.holds.GetByID(ctx, id)
	if err != nil {
		return err
	}
	// The path carries the restaurant; a hold that belongs to another venue is
	// reported as absent, not forbidden — no cross-tenant existence oracle
	// (same reasoning as bookings.authorize).
	if res.RestaurantID != restaurantID {
		return fmt.Errorf("%w: external reservation", domain.ErrNotFound)
	}
	if err := u.authorize(ctx, actor, restaurantID); err != nil {
		return err
	}
	return u.tx.WithinTx(ctx, func(ctx context.Context) error {
		return u.holds.Delete(ctx, id)
	})
}

func (u *externalReservationUseCase) List(ctx context.Context, actor Actor, restaurantID uuid.UUID, from, to time.Time) ([]domain.ExternalReservation, error) {
	if err := u.authorize(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	from, to = from.UTC(), to.UTC()
	if from.IsZero() || to.IsZero() {
		return nil, fmt.Errorf("%w: from and to required", domain.ErrValidation)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%w: to must be after from", domain.ErrValidation)
	}
	return u.holds.List(ctx, restaurantID, from, to)
}

// enforceTables resolves the tables whose slot the hold occupies: the one named
// table (validated to belong to the venue and be active), or — for a whole-venue
// block — every currently active table. A block created when a table does not
// yet exist will not cover that table; this is disclosed in the domain doc.
func (u *externalReservationUseCase) enforceTables(ctx context.Context, restaurantID uuid.UUID, tableID *uuid.UUID) ([]uuid.UUID, error) {
	tables, err := u.schedule.ListTables(ctx, restaurantID)
	if err != nil {
		return nil, err
	}
	if tableID != nil {
		for _, t := range tables {
			if t.ID == *tableID {
				if !t.IsActive {
					return nil, fmt.Errorf("%w: table is not active", domain.ErrValidation)
				}
				return []uuid.UUID{t.ID}, nil
			}
		}
		return nil, fmt.Errorf("%w: table does not belong to this restaurant", domain.ErrValidation)
	}
	ids := make([]uuid.UUID, 0, len(tables))
	for _, t := range tables {
		if t.IsActive {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// authorize enforces domain.PermBookingManage at restaurantID. RoleAdmin acts on
// any venue; a RoleRestaurant actor must hold the permission there per their
// StaffRole; everyone else is forbidden. Mirrors
// usecase/payments.authorizeStaffPermission.
func (u *externalReservationUseCase) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	switch actor.Role {
	case domain.RoleAdmin:
		return nil
	case domain.RoleRestaurant:
		if actor.UserID == uuid.Nil {
			return fmt.Errorf("%w: no authenticated staff actor", domain.ErrUnauthorized)
		}
		ok, err := u.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermBookingManage)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: this staff role cannot manage bookings at this restaurant", domain.ErrForbidden)
		}
		return nil
	default:
		return fmt.Errorf("%w: only venue staff can manage occupancy holds", domain.ErrForbidden)
	}
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

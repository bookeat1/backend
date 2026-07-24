// Package events is the application logic for restaurant events (Ф2). It owns
// the admin CRUD authorization (every mutation requires PermRestaurantManage at
// the event's OWN restaurant, or a superadmin) and the public read rules (only
// published, not-yet-ended events are listed). It reuses the shared domain RBAC
// matrix — it invents no permission — resolved per (actor, restaurant) exactly
// like usecase/admin and usecase/reviews do, so an owner of venue A can never
// touch venue B.
package events

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the staff-side (admin CRUD) actions. A
// global superadmin (Role == domain.RoleAdmin) bypasses restaurant scoping;
// anyone else is authorized by PermRestaurantManage against the event's own
// restaurant.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant",
// per the domain RBAC matrix. Bound to restaurants.ManagerUseCase in bootstrap.
// It is unaware of the global superadmin — this package checks RoleAdmin FIRST
// and bypasses it, the same contract every other HasPermission call site keeps.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Facade exposes admin CRUD and public read operations for events.
type Facade interface {
	Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Event, error)
	Update(ctx context.Context, actor Actor, eventID uuid.UUID, in UpdateInput) (*domain.Event, error)
	Delete(ctx context.Context, actor Actor, eventID uuid.UUID) error
	// GetAdmin returns any of a restaurant's events (any status) for the
	// cabinet. Requires PermRestaurantManage at the event's own restaurant.
	GetAdmin(ctx context.Context, actor Actor, eventID uuid.UUID) (*domain.Event, error)
	// ListAdmin returns a restaurant's events (optionally status-filtered) for
	// the cabinet, paginated. Requires PermRestaurantManage at restaurantID.
	ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error)

	// ListPublic returns a restaurant's published, not-yet-ended events,
	// paginated. No authorization.
	ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Event, int, error)
	// GetPublic returns one published event that belongs to restaurantID.
	// A draft/hidden event, or one of another restaurant, is ErrNotFound.
	GetPublic(ctx context.Context, restaurantID, eventID uuid.UUID) (*domain.Event, error)
}

// CreateInput carries a new event's fields. Status defaults to draft when empty.
type CreateInput struct {
	RestaurantID     uuid.UUID
	Title            string
	TitleI18n        domain.I18n
	Description      string
	DescriptionI18n  domain.I18n
	StartsAt         time.Time
	EndsAt           time.Time
	Venue            string
	CoverImageURL    *string
	Status           domain.EventStatus
	Ticketed         bool
	TicketPriceMinor *int64
	Capacity         *int
}

// UpdateInput carries an event's mutable fields (full replace). Status must be
// a valid EventStatus.
type UpdateInput struct {
	Title            string
	TitleI18n        domain.I18n
	Description      string
	DescriptionI18n  domain.I18n
	StartsAt         time.Time
	EndsAt           time.Time
	Venue            string
	CoverImageURL    *string
	Status           domain.EventStatus
	Ticketed         bool
	TicketPriceMinor *int64
	Capacity         *int
}

type facade struct {
	repo  domain.EventRepository
	perms permissionChecker
	clock func() time.Time
}

// NewFacade constructs the events Facade.
func NewFacade(repo domain.EventRepository, perms permissionChecker) Facade {
	return &facade{repo: repo, perms: perms, clock: time.Now}
}

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Event, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = domain.EventDraft
	}
	e := &domain.Event{
		RestaurantID:     in.RestaurantID,
		Title:            strings.TrimSpace(in.Title),
		TitleI18n:        in.TitleI18n,
		Description:      in.Description,
		DescriptionI18n:  in.DescriptionI18n,
		StartsAt:         in.StartsAt,
		EndsAt:           in.EndsAt,
		Venue:            in.Venue,
		CoverImageURL:    in.CoverImageURL,
		Status:           status,
		Ticketed:         in.Ticketed,
		TicketPriceMinor: in.TicketPriceMinor,
		Capacity:         in.Capacity,
	}
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) Update(ctx context.Context, actor Actor, eventID uuid.UUID, in UpdateInput) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	e.Title = strings.TrimSpace(in.Title)
	e.TitleI18n = in.TitleI18n
	e.Description = in.Description
	e.DescriptionI18n = in.DescriptionI18n
	e.StartsAt = in.StartsAt
	e.EndsAt = in.EndsAt
	e.Venue = in.Venue
	e.CoverImageURL = in.CoverImageURL
	e.Status = in.Status
	e.Ticketed = in.Ticketed
	e.TicketPriceMinor = in.TicketPriceMinor
	e.Capacity = in.Capacity
	if err := validateEvent(e); err != nil {
		return nil, err
	}
	if err := f.repo.Update(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) Delete(ctx context.Context, actor Actor, eventID uuid.UUID) error {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return err
	}
	return f.repo.Delete(ctx, eventID)
}

func (f *facade) GetAdmin(ctx context.Context, actor Actor, eventID uuid.UUID) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, e.RestaurantID); err != nil {
		return nil, err
	}
	return e, nil
}

func (f *facade) ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.EventStatus, page, perPage int) ([]domain.Event, int, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, statuses, page, perPage)
}

func (f *facade) ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Event, int, error) {
	return f.repo.ListPublishedUpcoming(ctx, restaurantID, f.clock(), page, perPage)
}

func (f *facade) GetPublic(ctx context.Context, restaurantID, eventID uuid.UUID) (*domain.Event, error) {
	e, err := f.repo.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// Never leak a draft/hidden event or one belonging to another restaurant
	// through the public endpoint: it simply does not exist to a guest.
	if e.RestaurantID != restaurantID || e.Status != domain.EventPublished {
		return nil, fmt.Errorf("get public event: %w", domain.ErrNotFound)
	}
	return e, nil
}

// authorize enforces PermRestaurantManage at restaurantID; a superadmin
// bypasses the check entirely (same contract as usecase/admin).
func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's events", domain.ErrForbidden)
	}
	return nil
}

// validateEvent enforces the invariants the DB CHECKs also guard, but with a
// domain.ErrValidation (422) instead of a 500 from a constraint violation.
func validateEvent(e *domain.Event) error {
	if e.Title == "" {
		return fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if !e.Status.Valid() {
		return fmt.Errorf("%w: unknown event status %q", domain.ErrValidation, e.Status)
	}
	if e.StartsAt.IsZero() || e.EndsAt.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", domain.ErrValidation)
	}
	if !e.EndsAt.After(e.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", domain.ErrValidation)
	}
	if e.TicketPriceMinor != nil && *e.TicketPriceMinor < 0 {
		return fmt.Errorf("%w: ticket_price_minor must be >= 0", domain.ErrValidation)
	}
	if e.Capacity != nil && *e.Capacity < 0 {
		return fmt.Errorf("%w: capacity must be >= 0", domain.ErrValidation)
	}
	return nil
}

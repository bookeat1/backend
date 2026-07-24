// Package promos is the application logic for restaurant promos (Ф2). Same
// shape as usecase/events: admin CRUD gated by PermRestaurantManage at the
// promo's own restaurant (superadmin bypasses), and a public listing that
// shows only published promos whose validity window contains now.
package promos

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the admin CRUD actions.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant".
// Bound to restaurants.ManagerUseCase in bootstrap.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Facade exposes admin CRUD and public read operations for promos.
type Facade interface {
	Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Promo, error)
	Update(ctx context.Context, actor Actor, promoID uuid.UUID, in UpdateInput) (*domain.Promo, error)
	Delete(ctx context.Context, actor Actor, promoID uuid.UUID) error
	GetAdmin(ctx context.Context, actor Actor, promoID uuid.UUID) (*domain.Promo, error)
	ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error)

	// ListPublic returns a restaurant's published promos whose validity window
	// contains now, paginated. No authorization.
	ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Promo, int, error)
}

// CreateInput carries a new promo's fields. Status defaults to draft when empty.
type CreateInput struct {
	RestaurantID    uuid.UUID
	Title           string
	TitleI18n       domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	StartsAt        time.Time
	EndsAt          time.Time
	Terms           string
	Status          domain.PromoStatus
}

// UpdateInput carries a promo's mutable fields (full replace).
type UpdateInput struct {
	Title           string
	TitleI18n       domain.I18n
	Description     string
	DescriptionI18n domain.I18n
	StartsAt        time.Time
	EndsAt          time.Time
	Terms           string
	Status          domain.PromoStatus
}

type facade struct {
	repo  domain.PromoRepository
	perms permissionChecker
	clock func() time.Time
}

// NewFacade constructs the promos Facade.
func NewFacade(repo domain.PromoRepository, perms permissionChecker) Facade {
	return &facade{repo: repo, perms: perms, clock: time.Now}
}

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Promo, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	status := in.Status
	if status == "" {
		status = domain.PromoDraft
	}
	p := &domain.Promo{
		RestaurantID:    in.RestaurantID,
		Title:           strings.TrimSpace(in.Title),
		TitleI18n:       in.TitleI18n,
		Description:     in.Description,
		DescriptionI18n: in.DescriptionI18n,
		StartsAt:        in.StartsAt,
		EndsAt:          in.EndsAt,
		Terms:           in.Terms,
		Status:          status,
	}
	if err := validatePromo(p); err != nil {
		return nil, err
	}
	if err := f.repo.Create(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (f *facade) Update(ctx context.Context, actor Actor, promoID uuid.UUID, in UpdateInput) (*domain.Promo, error) {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	p.Title = strings.TrimSpace(in.Title)
	p.TitleI18n = in.TitleI18n
	p.Description = in.Description
	p.DescriptionI18n = in.DescriptionI18n
	p.StartsAt = in.StartsAt
	p.EndsAt = in.EndsAt
	p.Terms = in.Terms
	p.Status = in.Status
	if err := validatePromo(p); err != nil {
		return nil, err
	}
	if err := f.repo.Update(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (f *facade) Delete(ctx context.Context, actor Actor, promoID uuid.UUID) error {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return err
	}
	return f.repo.Delete(ctx, promoID)
}

func (f *facade) GetAdmin(ctx context.Context, actor Actor, promoID uuid.UUID) (*domain.Promo, error) {
	p, err := f.repo.GetByID(ctx, promoID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, p.RestaurantID); err != nil {
		return nil, err
	}
	return p, nil
}

func (f *facade) ListAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID, statuses []domain.PromoStatus, page, perPage int) ([]domain.Promo, int, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, 0, err
	}
	return f.repo.ListByRestaurant(ctx, restaurantID, statuses, page, perPage)
}

func (f *facade) ListPublic(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.Promo, int, error) {
	return f.repo.ListActive(ctx, restaurantID, f.clock(), page, perPage)
}

func (f *facade) authorize(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's promos", domain.ErrForbidden)
	}
	return nil
}

func validatePromo(p *domain.Promo) error {
	if p.Title == "" {
		return fmt.Errorf("%w: title is required", domain.ErrValidation)
	}
	if !p.Status.Valid() {
		return fmt.Errorf("%w: unknown promo status %q", domain.ErrValidation, p.Status)
	}
	if p.StartsAt.IsZero() || p.EndsAt.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", domain.ErrValidation)
	}
	if !p.EndsAt.After(p.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", domain.ErrValidation)
	}
	return nil
}

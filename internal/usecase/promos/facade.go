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

// feedModerator pulls an item off the main-screen feed when its content
// changes. Minimal local port (bound to the feed repository in bootstrap): the
// promos usecase must not know the whole FeedRepository, only this one effect.
type feedModerator interface {
	DemoteAfterContentEdit(ctx context.Context, kind domain.FeedItemKind, itemID uuid.UUID) error
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
	CoverImageURL   *string
	DiscountPercent *int
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
	CoverImageURL   *string
	DiscountPercent *int
	Status          domain.PromoStatus
}

type facade struct {
	repo  domain.PromoRepository
	perms permissionChecker
	feed  feedModerator
	clock func() time.Time
}

// NewFacade constructs the promos Facade.
func NewFacade(repo domain.PromoRepository, perms permissionChecker, feed feedModerator) Facade {
	return &facade{repo: repo, perms: perms, feed: feed, clock: time.Now}
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
		CoverImageURL:   in.CoverImageURL,
		DiscountPercent: in.DiscountPercent,
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
	// Whether the CARD's content actually changed is decided before anything is
	// overwritten. Update carries Status too, and hiding then re-publishing an
	// approved promo goes through this same method — demoting for that would
	// send a venue back to the moderation queue for touching nothing a
	// moderator ever read.
	contentChanged := promoContentChanged(*p, in)

	p.Title = strings.TrimSpace(in.Title)
	p.TitleI18n = in.TitleI18n
	p.Description = in.Description
	p.DescriptionI18n = in.DescriptionI18n
	p.StartsAt = in.StartsAt
	p.EndsAt = in.EndsAt
	p.Terms = in.Terms
	p.CoverImageURL = in.CoverImageURL
	p.DiscountPercent = in.DiscountPercent
	p.Status = in.Status
	if err := validatePromo(p); err != nil {
		return nil, err
	}
	// Demote BEFORE writing the new content, not after: the platform approved
	// specific words, so changing them invalidates the decision. Ordered this
	// way the failure modes are both safe — a failed edit after a successful
	// demotion only costs the venue a re-review, whereas a failed demotion
	// after a successful edit would leave unreviewed text live on the main
	// screen. A transaction is deliberately not used: the safe ordering already
	// gives the guarantee that matters, without dragging a tx manager into a
	// simple CRUD facade. The residual window (a moderator approving in the
	// milliseconds between the demotion and the write) is known, self-healing on
	// the next edit, and judged not worth a tx here.
	if contentChanged {
		if err := f.feed.DemoteAfterContentEdit(ctx, domain.FeedItemPromo, promoID); err != nil {
			return nil, err
		}
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
	// The DB CHECK is the last line of defense; validating here turns a raw
	// constraint violation into a clean 422 with a readable message. Nil is
	// valid (no discount badge); a set value must be a real percentage.
	if p.DiscountPercent != nil && (*p.DiscountPercent < 0 || *p.DiscountPercent > 100) {
		return fmt.Errorf("%w: discount_percent must be between 0 and 100", domain.ErrValidation)
	}
	return nil
}

// promoContentChanged reports whether this update touches anything a moderator
// actually reviewed: the words shown on the card and the window it runs in.
// Status is excluded on purpose — publishing or hiding is the venue's own
// lever over its card and changes nothing a moderator read.
func promoContentChanged(cur domain.Promo, in UpdateInput) bool {
	return strings.TrimSpace(in.Title) != cur.Title ||
		in.Description != cur.Description ||
		in.Terms != cur.Terms ||
		!strPtrEqual(in.CoverImageURL, cur.CoverImageURL) ||
		!intPtrEqual(in.DiscountPercent, cur.DiscountPercent) ||
		!in.StartsAt.Equal(cur.StartsAt) ||
		!in.EndsAt.Equal(cur.EndsAt) ||
		!i18nEqual(in.TitleI18n, cur.TitleI18n) ||
		!i18nEqual(in.DescriptionI18n, cur.DescriptionI18n)
}

// strPtrEqual compares two optional strings by value: two nils are equal, and a
// nil never equals a set value ("the picture was removed" IS an edit).
func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// intPtrEqual compares two optional ints by value: two nils are equal, and a
// nil never equals a set value ("the discount was removed" IS an edit).
func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// i18nEqual compares two localized maps by content: a nil map and an empty one
// mean the same thing to a reader, so they must not count as an edit.
func i18nEqual(a, b domain.I18n) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

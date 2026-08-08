package stories

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the admin CRUD actions.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// permissionChecker answers "may this user perform perm at this restaurant".
// Bound to restaurants.ManagerUseCase in bootstrap — the same port promos and
// events use for their PermRestaurantManage gate.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// Facade exposes the public story read plus the admin CRUD/reorder surface.
// Every admin action is gated by PermRestaurantManage at the story's own
// restaurant (superadmin bypasses). Unlike promos/events, stories carry no feed
// gating: a story is not a moderated main-screen card, so content edits do not
// pull anything back into a review queue.
type Facade interface {
	// List returns the active stories of restaurantID in display order (public,
	// no authorization).
	List(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error)

	// ListForAdmin returns ALL of a restaurant's stories (active AND inactive) in
	// display order, gated by PermRestaurantManage at restaurantID.
	ListForAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.Story, error)

	// CreateStory adds a story to restaurantID.
	CreateStory(ctx context.Context, actor Actor, in CreateInput) (*domain.Story, error)

	// UpdateStory patches the given story (only the fields set in the input
	// change), gated at the story's own restaurant.
	UpdateStory(ctx context.Context, actor Actor, storyID uuid.UUID, in UpdateInput) (*domain.Story, error)

	// DeleteStory removes the given story, gated at its own restaurant.
	DeleteStory(ctx context.Context, actor Actor, storyID uuid.UUID) error

	// ReorderStories rewrites the sort order of a restaurant's stories to match
	// orderedIDs, gated at restaurantID. Foreign ids are ignored by the repo.
	ReorderStories(ctx context.Context, actor Actor, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error
}

// CreateInput carries a new story's fields.
//
// ImageURL is the full public URL, required. Caption is optional (nil means no
// caption). SortOrder, when nil, defaults to the end of the restaurant's current
// list. IsActive, when nil, defaults to true (a freshly added card is live).
type CreateInput struct {
	RestaurantID uuid.UUID
	ImageURL     string
	Caption      *string
	SortOrder    *int
	IsActive     *bool
}

// UpdateInput carries a story's mutable fields for a PARTIAL update: a nil field
// leaves the stored value untouched. Caption is a special case — nil leaves it
// unchanged, and an explicit empty/whitespace string clears it (empty→nil), so
// the "no caption" state is reachable through the edit form.
type UpdateInput struct {
	ImageURL  *string
	Caption   *string
	SortOrder *int
	IsActive  *bool
}

type facade struct {
	stories domain.StoryRepository
	perms   permissionChecker
}

// NewFacade constructs the stories Facade. perms is the same RBAC checker
// promos/events use; the admin methods require it.
func NewFacade(stories domain.StoryRepository, perms permissionChecker) Facade {
	return &facade{stories: stories, perms: perms}
}

func (f *facade) List(ctx context.Context, restaurantID uuid.UUID) ([]domain.Story, error) {
	return f.stories.ListActiveByRestaurant(ctx, restaurantID)
}

func (f *facade) ListForAdmin(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.Story, error) {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	return f.stories.ListByRestaurant(ctx, restaurantID)
}

func (f *facade) CreateStory(ctx context.Context, actor Actor, in CreateInput) (*domain.Story, error) {
	if err := f.authorize(ctx, actor, in.RestaurantID); err != nil {
		return nil, err
	}
	imageURL := strings.TrimSpace(in.ImageURL)
	if err := validateImageURL(imageURL); err != nil {
		return nil, err
	}
	sortOrder, err := f.resolveSortOrder(ctx, in.RestaurantID, in.SortOrder)
	if err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	s := &domain.Story{
		RestaurantID: in.RestaurantID,
		ImageURL:     imageURL,
		Caption:      normalizeCaption(in.Caption),
		SortOrder:    sortOrder,
		IsActive:     isActive,
	}
	if err := f.stories.Create(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (f *facade) UpdateStory(ctx context.Context, actor Actor, storyID uuid.UUID, in UpdateInput) (*domain.Story, error) {
	s, err := f.stories.GetByID(ctx, storyID)
	if err != nil {
		return nil, err
	}
	if err := f.authorize(ctx, actor, s.RestaurantID); err != nil {
		return nil, err
	}
	if in.ImageURL != nil {
		imageURL := strings.TrimSpace(*in.ImageURL)
		if err := validateImageURL(imageURL); err != nil {
			return nil, err
		}
		s.ImageURL = imageURL
	}
	if in.Caption != nil {
		s.Caption = normalizeCaption(in.Caption)
	}
	if in.SortOrder != nil {
		s.SortOrder = *in.SortOrder
	}
	if in.IsActive != nil {
		s.IsActive = *in.IsActive
	}
	if err := f.stories.Update(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (f *facade) DeleteStory(ctx context.Context, actor Actor, storyID uuid.UUID) error {
	s, err := f.stories.GetByID(ctx, storyID)
	if err != nil {
		return err
	}
	if err := f.authorize(ctx, actor, s.RestaurantID); err != nil {
		return err
	}
	return f.stories.Delete(ctx, storyID, s.RestaurantID)
}

func (f *facade) ReorderStories(ctx context.Context, actor Actor, restaurantID uuid.UUID, orderedIDs []uuid.UUID) error {
	if err := f.authorize(ctx, actor, restaurantID); err != nil {
		return err
	}
	return f.stories.Reorder(ctx, restaurantID, orderedIDs)
}

// resolveSortOrder returns the requested sort_order, or, when none was given,
// the end of the restaurant's current list (max existing sort_order + 1, or 0
// when the venue has no stories yet).
func (f *facade) resolveSortOrder(ctx context.Context, restaurantID uuid.UUID, requested *int) (int, error) {
	if requested != nil {
		return *requested, nil
	}
	existing, err := f.stories.ListByRestaurant(ctx, restaurantID)
	if err != nil {
		return 0, err
	}
	next := 0
	for i := range existing {
		if existing[i].SortOrder >= next {
			next = existing[i].SortOrder + 1
		}
	}
	return next, nil
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
		return fmt.Errorf("%w: restaurant.manage required to manage this restaurant's stories", domain.ErrForbidden)
	}
	return nil
}

// normalizeCaption trims the caption and collapses an empty result to nil, so
// "no caption" is stored as NULL rather than an empty string.
func normalizeCaption(c *string) *string {
	if c == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*c)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// validateImageURL turns what would otherwise be a NOT NULL / bad-data write
// into a clean 422: the image URL must be present and an absolute http(s) URL
// with a host, matching the "ImageURL is the full public URL" contract.
func validateImageURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: image_url is required", domain.ErrValidation)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: image_url must be an absolute http(s) URL", domain.ErrValidation)
	}
	return nil
}

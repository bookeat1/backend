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
	// CaptionI18n carries the caption's translations as a PARTIAL update
	// (domain.I18nPatch): a named language is written, a null one removed, an
	// unmentioned one kept. A `ru` key writes the caption itself — that column
	// IS the Russian text. Translations of a card with NO caption are dropped:
	// there is nothing to translate, and the DB CHECK from migration 0101 says
	// the same.
	CaptionI18n domain.I18nPatch
	// ActionURL is the EXTERNAL link a tap on the card follows — NOT the
	// picture's address, which is ImageURL. nil or an empty string means "the
	// card leads nowhere"; anything else must pass
	// domain.ValidateExternalActionURL.
	ActionURL *string
	SortOrder *int
	IsActive  *bool
}

// UpdateInput carries a story's mutable fields for a PARTIAL update: a nil field
// leaves the stored value untouched. Caption is a special case — nil leaves it
// unchanged, and an explicit empty/whitespace string clears it (empty→nil), so
// the "no caption" state is reachable through the edit form.
type UpdateInput struct {
	ImageURL *string
	Caption  *string
	// CaptionI18n is a PARTIAL translation update — see CreateInput. Sending it
	// alone (without caption) edits only the translations; sending a `ru` key
	// edits the caption column itself.
	CaptionI18n domain.I18nPatch
	// ActionURL follows the same three-state rule as Caption: nil leaves the
	// stored link untouched, an empty/whitespace string clears it (so the
	// operator can un-link a story through the edit form), and anything else is
	// validated and stored.
	ActionURL *string
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
	if err := in.CaptionI18n.Validate("caption_i18n"); err != nil {
		return nil, err
	}
	caption := normalizeCaption(promoteRussianCaption(in.Caption, in.CaptionI18n))
	actionURL, err := normalizeActionURL(in.ActionURL)
	if err != nil {
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
		Caption:      caption,
		CaptionI18n:  captionTranslations(nil, in.CaptionI18n, caption),
		ActionURL:    actionURL,
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
	if err := in.CaptionI18n.Validate("caption_i18n"); err != nil {
		return nil, err
	}
	// The caption and its translations move together, whichever of the two the
	// request actually carried: editing only the map still has to keep
	// i18n["ru"] equal to the column, and clearing the caption has to take the
	// translations with it (a card with no Russian text has nothing the other
	// languages could be a translation OF — and the DB CHECK refuses the pair).
	if caption := promoteRussianCaption(in.Caption, in.CaptionI18n); caption != nil {
		s.Caption = normalizeCaption(caption)
	}
	if in.Caption != nil || in.CaptionI18n != nil {
		s.CaptionI18n = captionTranslations(s.CaptionI18n, in.CaptionI18n, s.Caption)
	}
	if in.ActionURL != nil {
		actionURL, err := normalizeActionURL(in.ActionURL)
		if err != nil {
			return nil, err
		}
		s.ActionURL = actionURL
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

// promoteRussianCaption routes a `ru` entry inside the translation patch to the
// caption column it belongs to, unless the request already wrote the column
// itself (in which case the column wins — see domain.ApplyTranslations).
func promoteRussianCaption(caption *string, patch domain.I18nPatch) *string {
	if caption != nil {
		return caption
	}
	if v, ok := patch.Russian(); ok {
		return &v
	}
	return nil
}

// captionTranslations merges the patch onto the stored map and re-establishes
// the ru invariant — except for a card with NO caption, which can have no
// translations at all.
func captionTranslations(stored domain.I18n, patch domain.I18nPatch, caption *string) domain.I18n {
	if caption == nil {
		return nil
	}
	return domain.ApplyTranslations(stored, patch, *caption)
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

// normalizeActionURL turns the operator's input into what gets stored: nil (or
// a blank string) means "no link", and anything else must survive
// domain.ValidateExternalActionURL — the SAME validator the event action button
// uses, deliberately not a second, softer copy of it. It refuses javascript:/
// data: and friends, credentials in the URL, control characters smuggled into
// the scheme ("java\nscript:") and anything over 2048 characters, all as
// ErrValidation → 422.
//
// Note this is stricter than validateImageURL below, and that is on purpose:
// the image URL is fetched by an <img>, while this one is OPENED in the guest's
// browser on tap, which is what makes a hostile scheme dangerous.
func normalizeActionURL(raw *string) (*string, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	v, err := domain.ValidateExternalActionURL(*raw)
	if err != nil {
		return nil, err
	}
	return &v, nil
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

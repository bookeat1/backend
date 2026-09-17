// Package foodieoptions is the "Фуди-профиль" option dictionary usecase: the
// platform-owned reference list (create / edit / hide — superadmin only,
// spec foodie-profile-admin-dictionaries-20260916.md) behind both
// GET /foodie-profile/options (public) and the four
// /admin/foodie-profile/options routes.
//
// Modeled directly on usecase/cuisines (same package shape, same
// requirePlatform gate, same "hide never deletes" rule) — the two dictionaries
// share an owner and a UI pattern, so keeping the same shape means one mental
// model for both.
package foodieoptions

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. Only domain.RoleAdmin (the platform's
// superadmin) may touch the dictionary — same gate as usecase/cuisines.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// cuisineResolver is the minimal slice of domain.CuisineRepository this
// package needs: turning a cuisine-tile's chosen cuisine ids into real,
// ACTIVE dictionary entries (🔴1 = A). A local port, not the concrete
// repository, per CLAUDE.md's usecase-layering rule.
type cuisineResolver interface {
	ResolveIDs(ctx context.Context, ids []uuid.UUID) ([]domain.Cuisine, error)
}

// maxNameRunes is the 🟡6.8 assumption from the spec: a tile renders its
// name in two lines (numberOfLines={2}); anything longer is refused server
// side rather than silently clipped.
const maxNameRunes = 40

// maxCodeLen/maxBudgetCodeLen bound Code (criterion 6): the general cap
// matches cuisine's own code length ceiling; budget is narrower because its
// code is also written into users.foodie_budget_tier varchar(16) (migration
// 0109) — a longer code there would simply fail to insert, so the API
// refuses it up front instead of failing on save.
const (
	maxCodeLen       = 32
	maxBudgetCodeLen = 16
)

// UseCase is the dictionary API surface.
type UseCase interface {
	// List returns the dictionary across all four kinds. includeInactive is
	// honoured ONLY for a superadmin — same "guest asking for hidden gets the
	// active list, not a 403" rule as usecase/cuisines.List, because the
	// public route is anonymous.
	List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.FoodieOption, error)
	Create(ctx context.Context, actor Actor, in SaveInput) (*domain.FoodieOption, error)
	Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.FoodieOption, error)
	// SetActive hides (false) or restores (true) an option. There is no hard
	// delete: user_foodie_* rows can still reference a hidden code (spec
	// 3.5/3.9). Hiding is refused for domain.FoodieDietExclusiveID
	// ("no_diet") and for the last active option of its kind (criterion 8).
	SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.FoodieOption, error)
}

type useCase struct {
	repo     domain.FoodieOptionRepository
	cuisines cuisineResolver
	tx       domain.TxManager
}

// NewUseCase constructs the foodie-option dictionary usecase.
func NewUseCase(repo domain.FoodieOptionRepository, cuisines cuisineResolver, tx domain.TxManager) UseCase {
	return &useCase{repo: repo, cuisines: cuisines, tx: tx}
}

// SaveInput carries the mutable dictionary fields. Every field is a pointer
// so Update can distinguish "absent from the request" (preserve) from
// "explicitly provided" — same convention as usecase/cuisines.SaveInput.
//
// Kind and Code are accepted on Update too (not omitted from the type) so the
// transport layer can detect "sent, but different from what is stored" and
// answer criterion 7's 422 "код неизменяем" — applyInput never changes
// Kind/Code, Update itself compares them against the existing row first.
type SaveInput struct {
	Kind            *domain.FoodieOptionKind
	Code            *string
	Name            *string
	NameI18n        domain.I18n
	ImageURL        *string
	Description     *string
	DescriptionI18n domain.I18n
	PriceLabel      *string
	PriceLabelI18n  domain.I18n
	// PriceCategory follows the same "nil = not sent, empty = explicitly
	// cleared, non-empty = explicitly set" convention as ImageURL/Description/
	// PriceLabel above (not domain.PriceCategory directly, so "" is
	// representable) — a budget tier can go from having a ₸-equivalent to not
	// having one (spec 3.10's "ярус можно оставить пустым").
	PriceCategory *string
	// CuisineIDs is *[]uuid.UUID (not a plain slice) for the same reason:
	// nil = "field omitted, keep the existing links"; a non-nil pointer to an
	// empty slice = "clear every link".
	CuisineIDs   *[]uuid.UUID
	DisplayOrder *int
	IsActive     *bool
}

func (u *useCase) List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.FoodieOption, error) {
	f := domain.FoodieOptionFilter{IncludeInactive: includeInactive && actor.Role == domain.RoleAdmin}
	return u.repo.List(ctx, f)
}

// requirePlatform is the single gate on every dictionary mutation — the
// transport layer already mounts these routes behind RequireRole(RoleAdmin);
// this is the defense-in-depth re-check (same as usecase/cuisines).
func requirePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform may manage the foodie-profile option dictionary", domain.ErrForbidden)
	}
	return nil
}

func (u *useCase) Create(ctx context.Context, actor Actor, in SaveInput) (*domain.FoodieOption, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	if in.Kind == nil || !in.Kind.Valid() {
		return nil, fmt.Errorf("%w: kind must be one of cuisine/diet/allergy/budget", domain.ErrValidation)
	}
	o := &domain.FoodieOption{ID: uuid.New(), Kind: *in.Kind, IsActive: true}
	if err := applyInput(o, in); err != nil {
		return nil, err
	}
	if o.Code == "" {
		return nil, fmt.Errorf("%w: code is required", domain.ErrValidation)
	}
	if o.Name == "" {
		return nil, fmt.Errorf("%w: name is required", domain.ErrValidation)
	}

	cuisineIDs, err := u.resolveCuisineIDs(ctx, o.Kind, in.CuisineIDs)
	if err != nil {
		return nil, err
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.repo.Create(ctx, o); err != nil {
			return err
		}
		if o.Kind == domain.FoodieOptionKindCuisine && in.CuisineIDs != nil {
			if err := u.repo.SetCuisineLinks(ctx, o.ID, idsOf(cuisineIDs)); err != nil {
				return err
			}
			o.Cuisines = cuisineIDs
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (u *useCase) Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.FoodieOption, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	existing, err := u.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Kind != nil && *in.Kind != existing.Kind {
		return nil, fmt.Errorf("%w: kind is immutable", domain.ErrValidation)
	}
	if in.Code != nil && normalizeCode(*in.Code) != existing.Code {
		return nil, fmt.Errorf("%w: code is immutable", domain.ErrValidation)
	}

	updated := *existing
	if err := applyInput(&updated, in); err != nil {
		return nil, err
	}
	if updated.Name == "" {
		return nil, fmt.Errorf("%w: name must not be empty", domain.ErrValidation)
	}

	// Hiding (active -> hidden) is the ONLY transition the two spec guards
	// apply to (criterion 8): restoring, or any edit that leaves is_active
	// alone, never touches them.
	if existing.IsActive && !updated.IsActive {
		if err := u.guardHide(ctx, *existing); err != nil {
			return nil, err
		}
	}

	cuisineIDs, err := u.resolveCuisineIDs(ctx, updated.Kind, in.CuisineIDs)
	if err != nil {
		return nil, err
	}

	err = u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.repo.Update(ctx, &updated); err != nil {
			return err
		}
		if updated.Kind == domain.FoodieOptionKindCuisine && in.CuisineIDs != nil {
			if err := u.repo.SetCuisineLinks(ctx, updated.ID, idsOf(cuisineIDs)); err != nil {
				return err
			}
			updated.Cuisines = cuisineIDs
		} else {
			updated.Cuisines = existing.Cuisines
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// guardHide enforces criterion 8's two refusals: domain.FoodieDietExclusiveID
// ("no_diet") can never be hidden — it is the only way a guest says "I have
// no diet", and it carries the exclusivity rule — and the LAST active option
// of a kind can never be hidden, because every wizard step but budget
// requires at least one pick to continue (spec 3.6).
//
// KNOWN, ACCEPTED RACE: CountActive reads outside a row lock, so two admins
// hiding the last two active options of the same kind in the same instant can
// both pass this check and leave the kind with zero active options. This is
// an admin-configuration safeguard, not a money-moving invariant — the same
// class of accepted race usecase/cuisines documents for its alias insert, and
// tightening it would cost a SELECT ... FOR UPDATE on every edit of a table
// with 36 rows and a handful of editors.
func (u *useCase) guardHide(ctx context.Context, existing domain.FoodieOption) error {
	if existing.Kind == domain.FoodieOptionKindDiet && existing.Code == domain.FoodieDietExclusiveID {
		return fmt.Errorf("%w: %q cannot be hidden", domain.ErrValidation, domain.FoodieDietExclusiveID)
	}
	n, err := u.repo.CountActive(ctx, existing.Kind)
	if err != nil {
		return err
	}
	if n <= 1 {
		return fmt.Errorf("%w: cannot hide the last active option of kind %q", domain.ErrValidation, existing.Kind)
	}
	return nil
}

func (u *useCase) SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.FoodieOption, error) {
	return u.Update(ctx, actor, id, SaveInput{IsActive: &active})
}

// resolveCuisineIDs validates and resolves a cuisine-kind SaveInput.CuisineIDs
// against the cuisine dictionary (criterion 6: every id must exist AND be
// active). Returns nil, nil when ids is nil (field omitted) or kind is not
// cuisine and ids is nil; a non-cuisine kind with a non-nil ids is a 422.
func (u *useCase) resolveCuisineIDs(ctx context.Context, kind domain.FoodieOptionKind, ids *[]uuid.UUID) ([]domain.Cuisine, error) {
	if ids == nil {
		return nil, nil
	}
	if kind != domain.FoodieOptionKindCuisine {
		return nil, fmt.Errorf("%w: cuisine_ids is only allowed for cuisine options", domain.ErrValidation)
	}
	if len(*ids) == 0 {
		return []domain.Cuisine{}, nil
	}
	resolved, err := u.cuisines.ResolveIDs(ctx, *ids)
	if err != nil {
		return nil, err
	}
	for _, c := range resolved {
		if !c.IsActive {
			return nil, fmt.Errorf("%w: cuisine %q is hidden", domain.ErrValidation, c.Code)
		}
	}
	return resolved, nil
}

func idsOf(cs []domain.Cuisine) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// applyInput writes in's present fields onto o, validating each as it goes.
// Code/Kind are written ONLY on Create (o.Code == "" / freshly-assigned Kind)
// — Update rejects a changed Code/Kind before ever calling this, but
// applyInput still needs to accept an UNCHANGED Code/Kind sent alongside
// other fields (criterion 7 allows resending the same value).
func applyInput(o *domain.FoodieOption, in SaveInput) error {
	if in.Code != nil {
		maxLen := maxCodeLen
		if o.Kind == domain.FoodieOptionKindBudget {
			maxLen = maxBudgetCodeLen
		}
		code := normalizeCode(*in.Code)
		if err := validateCode(code, maxLen); err != nil {
			return err
		}
		o.Code = code
	}
	if in.Name != nil {
		name := strings.Join(strings.Fields(*in.Name), " ")
		if utf8.RuneCountInString(name) > maxNameRunes {
			return fmt.Errorf("%w: name must be at most %d characters", domain.ErrValidation, maxNameRunes)
		}
		o.Name = name
	}
	if in.NameI18n != nil {
		o.NameI18n = in.NameI18n
	}
	if in.ImageURL != nil {
		o.ImageURL = trimmedOrNil(*in.ImageURL)
	}
	if in.Description != nil {
		o.Description = trimmedOrNil(*in.Description)
	}
	if in.DescriptionI18n != nil {
		o.DescriptionI18n = in.DescriptionI18n
	}
	if in.PriceLabel != nil {
		o.PriceLabel = trimmedOrNil(*in.PriceLabel)
	}
	if in.PriceLabelI18n != nil {
		o.PriceLabelI18n = in.PriceLabelI18n
	}
	if in.PriceCategory != nil {
		v := strings.TrimSpace(*in.PriceCategory)
		if v == "" {
			if o.Kind == domain.FoodieOptionKindBudget {
				o.PriceCategory = nil
			}
			// A non-budget kind ignoring an explicit-clear of a field it
			// never had is a no-op, not an error.
		} else {
			if o.Kind != domain.FoodieOptionKindBudget {
				return fmt.Errorf("%w: price_category is only allowed for budget options", domain.ErrValidation)
			}
			pc := domain.PriceCategory(v)
			if !pc.Valid() {
				return fmt.Errorf("%w: price_category must be a valid price tier", domain.ErrValidation)
			}
			o.PriceCategory = &pc
		}
	}
	if in.DisplayOrder != nil {
		o.DisplayOrder = *in.DisplayOrder
	}
	if in.IsActive != nil {
		o.IsActive = *in.IsActive
	}
	return nil
}

func trimmedOrNil(s string) *string {
	v := strings.TrimSpace(s)
	if v == "" {
		return nil
	}
	return &v
}

func normalizeCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// validateCode keeps Code a stable machine key: lowercase latin, digits and
// underscores only — same alphabet as cuisine's own validateCode, plus a
// caller-chosen max length (budget is narrower, see maxBudgetCodeLen).
func validateCode(code string, maxLen int) error {
	if code == "" || len(code) > maxLen {
		return fmt.Errorf("%w: code must be 1..%d characters", domain.ErrValidation, maxLen)
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return fmt.Errorf("%w: code may contain only a-z, 0-9 and _", domain.ErrValidation)
		}
	}
	return nil
}

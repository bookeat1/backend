// Package cuisines is the cuisine dictionary usecase: the platform-owned
// reference list (create / edit / hide — superadmin only) and the venue's own
// cuisine set (chosen from that list by the venue's staff).
//
// The rule the whole package exists to enforce (ADR-022): a venue PICKS from
// the dictionary, it never invents an entry. Letting venues type their own is
// exactly how «Кафе, европейская» ended up in the catalog as a single value
// that matched no filter.
package cuisines

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller. Only domain.RoleAdmin (the platform's
// superadmin) may touch the dictionary itself; venue-scoped calls are
// authorized by the transport's RequireRestaurantManager gate plus the
// permission re-check below.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// restaurantCuisineTypeWriter rewrites the DERIVED backward-compatibility
// columns on `restaurants` (cuisine_type / cuisine_type_i18n) after the venue's
// cuisine set changes.
//
// It is a narrow port rather than a full RestaurantRepository dependency on
// purpose: this package must be able to write those two columns and nothing
// else, so it can never accidentally overwrite a venue's other fields with a
// stale read-modify-write.
type restaurantCuisineTypeWriter interface {
	UpdateCuisineTypeString(ctx context.Context, restaurantID uuid.UUID, cuisineType string, i18n domain.I18n) error
}

// permissionChecker is the minimal slice of usecase/restaurants.ManagerUseCase
// needed to answer "may this user edit THIS venue". Bound in bootstrap/deps.go.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// UseCase is the cuisine dictionary API surface.
type UseCase interface {
	// List returns the dictionary. includeInactive is honoured ONLY for a
	// superadmin — a guest or a venue asking for hidden entries gets the
	// active list, not a 403, because the hidden ones are simply not their
	// business and a guest calls this route anonymously.
	List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.Cuisine, error)
	Create(ctx context.Context, actor Actor, in SaveInput) (*domain.Cuisine, error)
	Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.Cuisine, error)
	// SetActive hides (false) or restores (true) a dictionary entry. There is
	// no hard delete: venues and guest preferences reference cuisines.
	SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.Cuisine, error)

	// ForRestaurant returns a venue's cuisines in the venue's own order.
	ForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.Cuisine, error)
	// SetForRestaurant replaces the venue's cuisine set (order matters: the
	// first entry is the venue's main cuisine) and rewrites the derived
	// cuisine_type string in the SAME transaction.
	SetForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, ids []uuid.UUID) ([]domain.Cuisine, error)
}

type useCase struct {
	repo    domain.CuisineRepository
	venues  restaurantCuisineTypeWriter
	perms   permissionChecker
	tx      domain.TxManager
	maxPerV int
}

// MaxCuisinesPerVenue caps a venue's cuisine set. A venue that claims eight
// cuisines is telling the guest nothing; the cap is a product decision, not a
// technical one, and it lives here rather than in a DB CHECK so it can be
// changed without a migration.
const MaxCuisinesPerVenue = 5

// NewUseCase constructs the cuisine usecase.
func NewUseCase(
	repo domain.CuisineRepository,
	venues restaurantCuisineTypeWriter,
	perms permissionChecker,
	tx domain.TxManager,
) UseCase {
	return &useCase{repo: repo, venues: venues, perms: perms, tx: tx, maxPerV: MaxCuisinesPerVenue}
}

// SaveInput carries the mutable dictionary fields. Every field is a pointer so
// Update can distinguish "absent from the request" (preserve) from "explicitly
// provided" — the same read-modify-write convention the restaurants facade uses.
type SaveInput struct {
	Code         *string
	Name         *string
	NameI18n     domain.I18n
	ImageURL     *string
	DisplayOrder *int
	IsActive     *bool
}

func (u *useCase) List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.Cuisine, error) {
	f := domain.CuisineFilter{IncludeInactive: includeInactive && actor.Role == domain.RoleAdmin}
	return u.repo.List(ctx, f)
}

// requirePlatform is the single gate on every dictionary mutation. The
// transport layer already mounts these routes behind RequireRole(RoleAdmin);
// this is the defense-in-depth re-check, so a future re-mount on a wider group
// cannot silently hand the dictionary to venue staff.
func requirePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform may manage the cuisine dictionary", domain.ErrForbidden)
	}
	return nil
}

func (u *useCase) Create(ctx context.Context, actor Actor, in SaveInput) (*domain.Cuisine, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	c := &domain.Cuisine{ID: uuid.New(), IsActive: true}
	if err := applyInput(c, in); err != nil {
		return nil, err
	}
	if c.Code == "" || c.Name == "" {
		return nil, fmt.Errorf("%w: code and name are required", domain.ErrValidation)
	}
	if err := u.repo.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (u *useCase) Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.Cuisine, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	c, err := u.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := applyInput(c, in); err != nil {
		return nil, err
	}
	if c.Code == "" || c.Name == "" {
		return nil, fmt.Errorf("%w: code and name must not be empty", domain.ErrValidation)
	}
	if err := u.repo.Update(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (u *useCase) SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.Cuisine, error) {
	return u.Update(ctx, actor, id, SaveInput{IsActive: &active})
}

func (u *useCase) ForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.Cuisine, error) {
	byVenue, err := u.repo.ListByRestaurants(ctx, []uuid.UUID{restaurantID})
	if err != nil {
		return nil, err
	}
	out := byVenue[restaurantID]
	if out == nil {
		out = []domain.Cuisine{}
	}
	return out, nil
}

// SetForRestaurant replaces the venue's cuisine set and rewrites the derived
// cuisine_type string in ONE transaction.
//
// Both halves must land together: the set is what the new clients read and the
// string is what the store builds read, and a venue whose two answers disagree
// is worse than either being stale. An empty set is allowed and clears the
// string — the venue explicitly said it has no cuisine listed.
func (u *useCase) SetForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, ids []uuid.UUID) ([]domain.Cuisine, error) {
	if err := u.authorizeVenue(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	ids = dedupe(ids)
	if len(ids) > u.maxPerV {
		return nil, fmt.Errorf("%w: at most %d cuisines per venue", domain.ErrValidation, u.maxPerV)
	}

	var set []domain.Cuisine
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		resolved, err := u.repo.ResolveIDs(ctx, ids)
		if err != nil {
			return err
		}
		// A hidden dictionary entry must not be newly assigned: hiding a
		// cuisine has to actually stop it spreading, or "скрыть" means nothing.
		for _, c := range resolved {
			if !c.IsActive {
				return fmt.Errorf("%w: cuisine %q is hidden", domain.ErrValidation, c.Code)
			}
		}
		if err := u.repo.SetForRestaurant(ctx, restaurantID, ids); err != nil {
			return err
		}
		if err := u.venues.UpdateCuisineTypeString(ctx, restaurantID,
			domain.JoinCuisineNames(resolved, ""), domain.CuisineI18nFromSet(resolved)); err != nil {
			return err
		}
		set = resolved
		return nil
	})
	if err != nil {
		return nil, err
	}
	if set == nil {
		set = []domain.Cuisine{}
	}
	return set, nil
}

// authorizeVenue: a superadmin passes; anyone else needs restaurant.manage at
// THIS venue. The transport gate is not trusted alone — same reason
// usecase/restaurants.ManagerUseCase re-resolves its target.
func (u *useCase) authorizeVenue(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	if u.perms == nil {
		return fmt.Errorf("%w: venue permissions unavailable", domain.ErrForbidden)
	}
	ok, err := u.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: not a manager of this restaurant", domain.ErrForbidden)
	}
	return nil
}

func applyInput(c *domain.Cuisine, in SaveInput) error {
	if in.Code != nil {
		code := strings.ToLower(strings.TrimSpace(*in.Code))
		if err := validateCode(code); err != nil {
			return err
		}
		c.Code = code
	}
	if in.Name != nil {
		c.Name = strings.Join(strings.Fields(*in.Name), " ")
	}
	if in.NameI18n != nil {
		c.NameI18n = in.NameI18n
	}
	if in.ImageURL != nil {
		v := strings.TrimSpace(*in.ImageURL)
		if v == "" {
			c.ImageURL = nil
		} else {
			c.ImageURL = &v
		}
	}
	if in.DisplayOrder != nil {
		c.DisplayOrder = *in.DisplayOrder
	}
	if in.IsActive != nil {
		c.IsActive = *in.IsActive
	}
	return nil
}

// validateCode keeps Code a stable machine key: lowercase latin, digits and
// underscores only. Clients key their bundled fallback image off it and it
// travels in query strings, so anything else (spaces, Cyrillic, punctuation)
// would break either the asset lookup or the URL.
func validateCode(code string) error {
	if code == "" || len(code) > 64 {
		return fmt.Errorf("%w: code must be 1..64 characters", domain.ErrValidation)
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

// dedupe keeps first-seen order: the venue's order is meaningful (position 0 is
// the main cuisine), and a repeated id would otherwise become a duplicate row.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

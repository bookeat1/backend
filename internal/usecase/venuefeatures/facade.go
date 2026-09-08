// Package venuefeatures is the venue-feature («удобства») dictionary usecase:
// the platform-owned reference list (create / edit / hide — superadmin only)
// and the venue's own feature set, chosen from that list by the venue's staff.
//
// The rule the whole package exists to enforce (ADR-022, extended to features
// on 2026-08-25): a venue PICKS from the dictionary, it never invents an entry.
// The free-text table this replaces is exactly what happens otherwise — it
// accumulated a cuisine («Восточная кухня»), a district («Коктобе») and a
// sound-engineering spec («Профессиональный звук») under the same column as
// «Wi-Fi», and none of it could ever become a filter.
package venuefeatures

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

// permissionChecker is the minimal slice of usecase/restaurants.ManagerUseCase
// needed to answer "may this user edit THIS venue". Bound in bootstrap/deps.go.
type permissionChecker interface {
	HasPermission(ctx context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error)
}

// UseCase is the venue-feature dictionary API surface.
type UseCase interface {
	// List returns the dictionary. includeInactive is honoured ONLY for a
	// superadmin — a guest or a venue asking for hidden entries gets the
	// active list, not a 403, because the hidden ones are simply not their
	// business and a guest calls this route anonymously.
	//
	// Features carried by ZERO venues are still returned. That is a product
	// decision (2026-08-25), not an oversight: the owner fills the data by
	// hand and there are only eleven active venues, so hiding empty features
	// would cost more than it saves. `venue_count` travels in the payload so
	// a client that later wants to grey them out can, without a new endpoint.
	List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.VenueFeature, error)
	Create(ctx context.Context, actor Actor, in SaveInput) (*domain.VenueFeature, error)
	Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.VenueFeature, error)
	// SetActive hides (false) or restores (true) a dictionary entry. There is
	// no hard delete: venues reference features.
	SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.VenueFeature, error)

	// ForRestaurant returns a venue's features in the venue's own order.
	ForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.VenueFeature, error)
	// SetForRestaurant replaces the venue's feature set (order is the venue's
	// own display order).
	SetForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, ids []uuid.UUID) ([]domain.VenueFeature, error)
}

type useCase struct {
	repo    domain.VenueFeatureRepository
	perms   permissionChecker
	tx      domain.TxManager
	maxPerV int
}

// MaxFeaturesPerVenue caps a venue's feature set. The cap is generous compared
// with cuisines (a venue really can have Wi-Fi, a terrace, parking, a prayer
// room and high chairs at once) but it is not unlimited: a venue that ticks
// every box is telling the guest nothing, and an unbounded set turns the
// AND-filter into an arbitrarily long chain of EXISTS.
const MaxFeaturesPerVenue = 15

// NewUseCase constructs the venue-feature usecase.
func NewUseCase(
	repo domain.VenueFeatureRepository,
	perms permissionChecker,
	tx domain.TxManager,
) UseCase {
	return &useCase{repo: repo, perms: perms, tx: tx, maxPerV: MaxFeaturesPerVenue}
}

// SaveInput carries the mutable dictionary fields. Every field is a pointer so
// Update can distinguish "absent from the request" (preserve) from "explicitly
// provided" — the same read-modify-write convention the restaurants facade uses.
type SaveInput struct {
	Code         *string
	Name         *string
	NameI18n     domain.I18n
	DisplayOrder *int
	IsActive     *bool
}

func (u *useCase) List(ctx context.Context, actor Actor, includeInactive bool) ([]domain.VenueFeature, error) {
	f := domain.VenueFeatureFilter{IncludeInactive: includeInactive && actor.Role == domain.RoleAdmin}
	return u.repo.List(ctx, f)
}

// requirePlatform is the single gate on every dictionary mutation. The
// transport layer already mounts these routes behind RequireRole(RoleAdmin);
// this is the defense-in-depth re-check, so a future re-mount on a wider group
// cannot silently hand the dictionary to venue staff.
func requirePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform may manage the venue feature dictionary", domain.ErrForbidden)
	}
	return nil
}

func (u *useCase) Create(ctx context.Context, actor Actor, in SaveInput) (*domain.VenueFeature, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	vf := &domain.VenueFeature{ID: uuid.New(), IsActive: true}
	if err := applyInput(vf, in); err != nil {
		return nil, err
	}
	if vf.Code == "" || vf.Name == "" {
		return nil, fmt.Errorf("%w: code and name are required", domain.ErrValidation)
	}
	if err := u.repo.Create(ctx, vf); err != nil {
		return nil, err
	}
	return vf, nil
}

func (u *useCase) Update(ctx context.Context, actor Actor, id uuid.UUID, in SaveInput) (*domain.VenueFeature, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	vf, err := u.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := applyInput(vf, in); err != nil {
		return nil, err
	}
	if vf.Code == "" || vf.Name == "" {
		return nil, fmt.Errorf("%w: code and name must not be empty", domain.ErrValidation)
	}
	if err := u.repo.Update(ctx, vf); err != nil {
		return nil, err
	}
	return vf, nil
}

func (u *useCase) SetActive(ctx context.Context, actor Actor, id uuid.UUID, active bool) (*domain.VenueFeature, error) {
	return u.Update(ctx, actor, id, SaveInput{IsActive: &active})
}

func (u *useCase) ForRestaurant(ctx context.Context, restaurantID uuid.UUID) ([]domain.VenueFeature, error) {
	byVenue, err := u.repo.ListByRestaurants(ctx, []uuid.UUID{restaurantID})
	if err != nil {
		return nil, err
	}
	out := byVenue[restaurantID]
	if out == nil {
		out = []domain.VenueFeature{}
	}
	return out, nil
}

// SetForRestaurant replaces the venue's whole feature set.
//
// Unlike its cuisine twin there is no derived scalar column to keep in step,
// so the transaction wraps a single logical write — it is still a transaction
// because SetForRestaurant is a DELETE followed by N INSERTs, and a venue that
// crashed halfway would be left with fewer features than it had before and no
// way to know it.
//
// No consistency rules between features are enforced. «Детские стульчики» and
// «Без детей» are opposites in meaning but NOT mutually exclusive technically,
// and the owner asked explicitly not to invent such checks: a venue may tick
// whatever it likes, and an absurd combination is something a human notices
// while filling the data, not something a 422 should teach them.
func (u *useCase) SetForRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID, ids []uuid.UUID) ([]domain.VenueFeature, error) {
	if err := u.authorizeVenue(ctx, actor, restaurantID); err != nil {
		return nil, err
	}
	ids = dedupe(ids)
	if len(ids) > u.maxPerV {
		return nil, fmt.Errorf("%w: at most %d features per venue", domain.ErrValidation, u.maxPerV)
	}

	var set []domain.VenueFeature
	err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		resolved, err := u.repo.ResolveIDs(ctx, ids)
		if err != nil {
			return err
		}
		// A hidden dictionary entry must not be newly assigned: hiding a
		// feature has to actually stop it spreading, or "скрыть" means nothing.
		for _, vf := range resolved {
			if !vf.IsActive {
				return fmt.Errorf("%w: venue feature %q is hidden", domain.ErrValidation, vf.Code)
			}
		}
		if err := u.repo.SetForRestaurant(ctx, restaurantID, ids); err != nil {
			return err
		}
		set = resolved
		return nil
	})
	if err != nil {
		return nil, err
	}
	if set == nil {
		set = []domain.VenueFeature{}
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

func applyInput(vf *domain.VenueFeature, in SaveInput) error {
	if in.Code != nil {
		code := strings.ToLower(strings.TrimSpace(*in.Code))
		if err := validateCode(code); err != nil {
			return err
		}
		vf.Code = code
	}
	if in.Name != nil {
		vf.Name = strings.Join(strings.Fields(*in.Name), " ")
	}
	if in.NameI18n != nil {
		vf.NameI18n = in.NameI18n
	}
	if in.DisplayOrder != nil {
		vf.DisplayOrder = *in.DisplayOrder
	}
	if in.IsActive != nil {
		vf.IsActive = *in.IsActive
	}
	return nil
}

// validateCode keeps Code a stable machine key: lowercase latin, digits and
// underscores only. The filter travels by code in a query string and clients
// key their bundled icon off it, so anything else (spaces, Cyrillic,
// punctuation) would break either the icon lookup or the URL.
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

// dedupe keeps first-seen order: the venue's order is its display order, and a
// repeated id would otherwise become a duplicate row.
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

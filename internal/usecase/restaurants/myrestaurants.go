package restaurants

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// membershipReader resolves the caller's own staff memberships (the restaurants
// they are staff of, with the venue name + their role there). Implemented by
// the concrete restaurant.Managers repository via a single join scoped to the
// user id — a caller can never see a restaurant they have no membership in.
type membershipReader interface {
	ListMembershipsByUser(ctx context.Context, userID uuid.UUID) ([]domain.StaffMembership, error)
}

// restaurantBriefReader lists every restaurant on the platform (lightweight
// rows). Used ONLY on the superadmin branch of MyRestaurantsUseCase — a
// superadmin manages the whole platform, so their picker spans all venues.
// Implemented by the concrete restaurant.Repository.
type restaurantBriefReader interface {
	ListManageableBrief(ctx context.Context) ([]domain.RestaurantBrief, error)
}

// MyRestaurant is one entry of the my-restaurants picker: the restaurant id,
// its display name (still carrying the raw i18n map so the transport layer can
// localize per the request locale) and the caller's role there.
//
// Role is a plain string, not a domain.StaffRole, precisely because the
// superadmin branch has no restaurant-scoped StaffRole: those entries carry the
// global role "admin" instead (see List). For an ordinary staff caller Role is
// one of "owner"/"manager"/"hostess".
type MyRestaurant struct {
	RestaurantID uuid.UUID
	Name         string
	NameI18n     domain.I18n
	Role         string
}

// MyRestaurantsUseCase answers "which restaurants am I staff of" for the
// authenticated caller, so the admin panel can offer a post-login picker
// instead of asking staff to type a restaurant UUID.
//
// Superadmin behavior (chosen as the least-surprising, documented in the PR):
// a global superadmin (domain.RoleAdmin) is NOT a staff member of any single
// venue, so a literal membership read would return an empty list and leave them
// facing the very "type a UUID" problem this endpoint removes. Instead a
// superadmin receives EVERY restaurant on the platform (including inactive /
// hidden venues — they manage them all), each tagged with the role "admin".
type MyRestaurantsUseCase struct {
	memberships membershipReader
	restaurants restaurantBriefReader
}

// NewMyRestaurantsUseCase wires the membership reader (ordinary staff) and the
// platform-wide brief reader (superadmin). Both are the concrete postgres
// repositories in bootstrap; the narrow ports keep this package free of any
// infrastructure import, same as the rest of usecase/restaurants.
func NewMyRestaurantsUseCase(memberships membershipReader, restaurants restaurantBriefReader) *MyRestaurantsUseCase {
	return &MyRestaurantsUseCase{memberships: memberships, restaurants: restaurants}
}

// List returns the restaurants the actor may manage. For a superadmin: every
// venue, role "admin". For any other caller: exactly their own staff
// memberships, with their StaffRole at each. The result is always a non-nil
// slice so it serializes as [] (never null) for a caller with no memberships.
func (u *MyRestaurantsUseCase) List(ctx context.Context, actor Actor) ([]MyRestaurant, error) {
	if actor.Role == domain.RoleAdmin {
		briefs, err := u.restaurants.ListManageableBrief(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]MyRestaurant, 0, len(briefs))
		for _, b := range briefs {
			out = append(out, MyRestaurant{
				RestaurantID: b.ID,
				Name:         b.Name,
				NameI18n:     b.NameI18n,
				Role:         string(domain.RoleAdmin),
			})
		}
		return out, nil
	}

	memberships, err := u.memberships.ListMembershipsByUser(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]MyRestaurant, 0, len(memberships))
	for _, m := range memberships {
		out = append(out, MyRestaurant{
			RestaurantID: m.RestaurantID,
			Name:         m.Name,
			NameI18n:     m.NameI18n,
			Role:         string(m.Role),
		})
	}
	return out, nil
}

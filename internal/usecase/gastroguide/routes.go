package gastroguide

import (
	"context"
	"fmt"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// The guest side of «Гастропрогулки». It is a separate interface from Facade
// and not three more methods on it: a route and a collection share a
// publication rule, not a read model, and a client asking for routes should not
// have to know what a rubric is.
//
// Like Facade, it holds no visibility logic of its own — which route is live
// and which stop resolves to a venue card lives in SQL, so no filter passed by
// a caller can widen it. The facade validates the filters and supplies the
// clock.

// RouteFacade exposes the guest-facing reads of the routes.
type RouteFacade interface {
	// ListRoutes returns live routes, paginated, plus the total.
	ListRoutes(ctx context.Context, in RouteListInput) ([]domain.GastroRoute, int, error)
	// GetRoute returns one live route with its ordered stops. ErrNotFound
	// covers both "no such slug" and "not published".
	GetRoute(ctx context.Context, slug string) (*domain.GastroRouteDetail, error)
}

// RouteListInput carries the optional filters of the route listing.
type RouteListInput struct {
	City    *domain.City
	Page    int
	PerPage int
}

type routeFacade struct {
	repo  domain.GastroRouteRepository
	clock func() time.Time
}

// NewRouteFacade constructs the guest route facade.
func NewRouteFacade(repo domain.GastroRouteRepository) RouteFacade {
	return &routeFacade{repo: repo, clock: time.Now}
}

func (f *routeFacade) ListRoutes(ctx context.Context, in RouteListInput) ([]domain.GastroRoute, int, error) {
	if in.City != nil && !in.City.Valid() {
		return nil, 0, domain.WithCode(domain.CodeCityRequired,
			fmt.Errorf("%w: unknown city", domain.ErrValidation))
	}
	return f.repo.ListPublishedRoutes(ctx, domain.GastroRouteFilter{
		City: in.City, Page: in.Page, PerPage: in.PerPage,
	}, f.clock())
}

func (f *routeFacade) GetRoute(ctx context.Context, slug string) (*domain.GastroRouteDetail, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("%w: slug is required", domain.ErrValidation)
	}
	return f.repo.GetPublishedRouteBySlug(ctx, slug, f.clock())
}

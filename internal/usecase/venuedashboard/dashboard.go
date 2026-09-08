// Package venuedashboard is the application logic behind one restaurant's own
// dashboard: the numbers the people who run a venue look at, as opposed to the
// platform-wide dashboard the owner reads.
package venuedashboard

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// defaultLookback is the window applied when the caller asks for no period. A
// month is long enough that a quiet venue still sees a shape, and short enough
// that "cancellations" means the current state of things rather than history.
const (
	defaultLookback = 30 * 24 * time.Hour
	maxLookback     = 365 * 24 * time.Hour
)

// Repo is the narrow read port, satisfied by
// internal/infrastructure/postgres/venuedashboard.Repository.
type Repo interface {
	Summary(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) (domain.VenueDashboard, error)
	Load(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.VenueLoadSlot, error)
}

// UseCase serves a venue its own numbers.
type UseCase struct {
	repo Repo
	now  func() time.Time
}

// NewUseCase wires the read model in.
func NewUseCase(repo Repo) *UseCase { return &UseCase{repo: repo, now: time.Now} }

// Summary returns the venue's counters for the period.
//
// Authorisation is NOT here: the route is mounted behind the same
// RequireRestaurantManager gate as every other venue-scoped screen, so by the
// time a call reaches this method the caller has been proven to manage
// restaurantID. What this layer owns is the period — validating it, and
// refusing a window so wide it would turn a dashboard into a table scan.
func (u *UseCase) Summary(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) (domain.VenueDashboard, error) {
	p, err := u.period(from, to)
	if err != nil {
		return domain.VenueDashboard{}, err
	}
	return u.repo.Summary(ctx, restaurantID, p.from, p.to)
}

// Load returns occupancy by weekday and hour for the period.
func (u *UseCase) Load(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.VenueLoadSlot, error) {
	p, err := u.period(from, to)
	if err != nil {
		return nil, err
	}
	return u.repo.Load(ctx, restaurantID, p.from, p.to)
}

type window struct{ from, to time.Time }

// period fills in the defaults and validates the window. A zero `to` means now;
// a zero `from` means the default look-back before `to`.
func (u *UseCase) period(from, to time.Time) (window, error) {
	if to.IsZero() {
		to = u.now()
	}
	if from.IsZero() {
		from = to.Add(-defaultLookback)
	}
	if !from.Before(to) {
		return window{}, fmt.Errorf("%w: period start must be before its end", domain.ErrValidation)
	}
	if to.Sub(from) > maxLookback {
		return window{}, fmt.Errorf("%w: period must not exceed one year", domain.ErrValidation)
	}
	return window{from: from, to: to}, nil
}

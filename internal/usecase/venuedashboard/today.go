package venuedashboard

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Limits for the two lists on the panel's home screen.
//
// The defaults are what fits on a screen a hostess scans, not what the database
// can return: twenty unanswered requests is already a bad day, and fifty covers
// a full service in one read. The ceilings exist so a caller cannot turn a
// dashboard tile into an export of the whole venue.
const (
	defaultAwaitingLimit = 20
	maxAwaitingLimit     = 100
	defaultTodayLimit    = 50
	maxTodayLimit        = 200
)

// TodayRepo is the narrow read port, satisfied by
// internal/infrastructure/postgres/venuedashboard.TodayRepository.
type TodayRepo interface {
	Today(ctx context.Context, restaurantID uuid.UUID, now time.Time, awaitingLimit, todayLimit int) (domain.VenueToday, error)
}

// TodayUseCase serves the operational top of a venue's panel: what still needs
// an answer, and what is happening today.
type TodayUseCase struct {
	repo TodayRepo
	now  func() time.Time
}

// NewTodayUseCase wires the read model in.
func NewTodayUseCase(repo TodayRepo) *TodayUseCase {
	return &TodayUseCase{repo: repo, now: time.Now}
}

// Today returns the panel's operational view.
//
// Authorisation is NOT here, for the same reason as in Summary: the route is
// mounted behind RequireRestaurantManager(..., "id"), so by the time a call
// lands the caller has been proven to manage restaurantID. What this layer owns
// is the limits and the clock — one instant for both lists, so "today" and
// every waiting time are measured against the same moment.
//
// A non-positive limit means "the caller did not choose" and becomes the
// default; an oversized one is clamped rather than refused, matching how
// BookingFilter.PerPage treats a too-large page.
func (u *TodayUseCase) Today(ctx context.Context, restaurantID uuid.UUID, awaitingLimit, todayLimit int) (domain.VenueToday, error) {
	return u.repo.Today(ctx, restaurantID, u.now(),
		clampLimit(awaitingLimit, defaultAwaitingLimit, maxAwaitingLimit),
		clampLimit(todayLimit, defaultTodayLimit, maxTodayLimit),
	)
}

func clampLimit(v, def, max int) int {
	switch {
	case v <= 0:
		return def
	case v > max:
		return max
	default:
		return v
	}
}

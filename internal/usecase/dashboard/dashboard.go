// Package dashboard is the superadmin platform-dashboard usecase (Ф1): it
// gates every read on the global superadmin role and returns platform-wide
// aggregate statistics. It holds no domain logic of its own — it authorizes,
// normalizes the requested period, and delegates the aggregation to the
// read-model repository (all SQL, no Go-side looping).
package dashboard

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// defaultPeriod is the look-back window used when a caller supplies no from/to.
const defaultPeriod = 30 * 24 * time.Hour

// Ranking dimensions for the top-restaurants endpoint.
const (
	ByBookings = "bookings"
	ByGMV      = "gmv"
)

const (
	defaultCurrency = "KZT"
	defaultTopLimit = 10
	maxTopLimit     = 50
)

// Actor is the authenticated principal asking for dashboard data. Only a global
// superadmin (domain.RoleAdmin) is ever authorized — restaurant owners,
// managers, hostesses and guests are all rejected with ErrForbidden.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// UseCase serves the superadmin platform dashboard.
type UseCase struct {
	repo readRepo
	now  func() time.Time
}

// NewUseCase wires the read repository into the dashboard usecase.
func NewUseCase(repo readRepo) *UseCase {
	return &UseCase{repo: repo, now: time.Now}
}

// authorize is the single superadmin gate. It is defense-in-depth behind the
// transport-level RequireRole(RoleAdmin): every method calls it FIRST so the
// authorization is enforced in the usecase too, not only by the router — a
// non-superadmin (restaurant owner/manager/hostess/guest) is ErrForbidden.
func (u *UseCase) authorize(a Actor) error {
	if a.Role != domain.RoleAdmin {
		return domain.ErrForbidden
	}
	return nil
}

// Overview returns the platform top-line counters.
func (u *UseCase) Overview(ctx context.Context, actor Actor) (domain.PlatformOverview, error) {
	if err := u.authorize(actor); err != nil {
		return domain.PlatformOverview{}, err
	}
	return u.repo.Overview(ctx)
}

// BookingsBreakdown returns booking counts by status over the period, with EVERY
// known status present (zero-filled) so a client renders a stable set of buckets
// even on an empty platform. from/to may be zero → a sane default window.
func (u *UseCase) BookingsBreakdown(ctx context.Context, actor Actor, from, to time.Time) (domain.BookingsBreakdown, error) {
	if err := u.authorize(actor); err != nil {
		return domain.BookingsBreakdown{}, err
	}
	p, err := u.normalizePeriod(from, to)
	if err != nil {
		return domain.BookingsBreakdown{}, err
	}
	counts, err := u.repo.BookingsByStatus(ctx, p.from, p.to)
	if err != nil {
		return domain.BookingsBreakdown{}, err
	}
	return buildBreakdown(p, counts), nil
}

// PaymentsGMV returns captured (GMV) and refunded money over the period for one
// currency (defaults to KZT). Money is summed from stored amounts, not
// recomputed. Mixed currencies are never summed together — the currency is an
// explicit filter.
func (u *UseCase) PaymentsGMV(ctx context.Context, actor Actor, from, to time.Time, currency string) (domain.PaymentsGMV, error) {
	if err := u.authorize(actor); err != nil {
		return domain.PaymentsGMV{}, err
	}
	p, err := u.normalizePeriod(from, to)
	if err != nil {
		return domain.PaymentsGMV{}, err
	}
	cur := normalizeCurrency(currency)
	captured, refunded, err := u.repo.PaymentsGMV(ctx, p.from, p.to, cur)
	if err != nil {
		return domain.PaymentsGMV{}, err
	}
	return domain.PaymentsGMV{
		From:     p.from,
		To:       p.to,
		Currency: cur,
		Captured: captured,
		Refunded: refunded,
	}, nil
}

// TopRestaurants returns up to `limit` restaurants ranked over the period, by
// booking count (by=="bookings", default) or by captured GMV in a single
// currency (by=="gmv"). An unknown `by` is a validation error.
func (u *UseCase) TopRestaurants(ctx context.Context, actor Actor, from, to time.Time, by, currency string, limit int) ([]domain.TopRestaurant, error) {
	if err := u.authorize(actor); err != nil {
		return nil, err
	}
	p, err := u.normalizePeriod(from, to)
	if err != nil {
		return nil, err
	}
	limit = normalizeLimit(limit)
	switch normalizeBy(by) {
	case ByBookings:
		return u.repo.TopRestaurantsByBookings(ctx, p.from, p.to, limit)
	case ByGMV:
		return u.repo.TopRestaurantsByGMV(ctx, p.from, p.to, normalizeCurrency(currency), limit)
	default:
		return nil, domain.ErrValidation
	}
}

// period is a validated half-open window [from, to).
type period struct {
	from time.Time
	to   time.Time
}

// normalizePeriod applies defaults for a zero from/to and validates the window.
// A zero `to` defaults to now; a zero `from` defaults to `to` minus the default
// look-back. from must be strictly before to.
func (u *UseCase) normalizePeriod(from, to time.Time) (period, error) {
	if to.IsZero() {
		to = u.now()
	}
	if from.IsZero() {
		from = to.Add(-defaultPeriod)
	}
	if !from.Before(to) {
		return period{}, domain.ErrValidation
	}
	return period{from: from, to: to}, nil
}

// buildBreakdown zero-fills every known booking status in a stable order so the
// response shape is identical whether or not a status had bookings.
func buildBreakdown(p period, counts []domain.BookingStatusCount) domain.BookingsBreakdown {
	got := make(map[domain.BookingStatus]int64, len(counts))
	var total int64
	for _, c := range counts {
		got[c.Status] = c.Count
		total += c.Count
	}
	statuses := []domain.BookingStatus{
		domain.BookingPending,
		domain.BookingConfirmed,
		domain.BookingWaitlist,
		domain.BookingArrived,
		domain.BookingCompleted,
		domain.BookingCancelled,
		domain.BookingNoShow,
	}
	byStatus := make([]domain.BookingStatusCount, 0, len(statuses))
	for _, s := range statuses {
		byStatus = append(byStatus, domain.BookingStatusCount{Status: s, Count: got[s]})
	}
	return domain.BookingsBreakdown{From: p.from, To: p.to, Total: total, ByStatus: byStatus}
}

func normalizeCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return defaultCurrency
	}
	return c
}

func normalizeBy(by string) string {
	by = strings.ToLower(strings.TrimSpace(by))
	if by == "" {
		return ByBookings
	}
	return by
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultTopLimit
	}
	if limit > maxTopLimit {
		return maxTopLimit
	}
	return limit
}

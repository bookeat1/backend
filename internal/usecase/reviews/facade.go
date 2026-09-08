// Package reviews is the application logic for guest reviews & ratings. It
// enforces the two rules the transport layer must not be trusted with: a guest
// may only review a restaurant they have a COMPLETED booking at (a verified
// review), and only a venue's staff holding PermStaffManage (or a superadmin)
// may reply to or moderate a review.
package reviews

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller for the staff-side actions (reply/hide).
// A global superadmin (Role == domain.RoleAdmin) bypasses restaurant scoping;
// anyone else is authorized entirely by staff permission against the review's
// OWN restaurant — mirrors restaurants.Actor / usecase/payments authorization.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// bookingLookup is the minimal slice of the bookings repository this package
// needs: proving the guest has a completed visit at the restaurant. Bound to
// domain.BookingRepository in bootstrap.
type bookingLookup interface {
	List(ctx context.Context, f domain.BookingFilter) ([]domain.Booking, int, error)
}

// staffRoles resolves a user's staff rows so this package can derive their
// StaffRole at a given restaurant. Bound directly to
// domain.RestaurantManagerRepository in bootstrap (the favorites-style "depend
// on the domain repo, not another usecase" convention) so the reviews package
// stays decoupled from usecase/restaurants.
type staffRoles interface {
	ListByUser(ctx context.Context, userID uuid.UUID) ([]domain.RestaurantManager, error)
}

// Facade exposes guest, public and staff review operations.
type Facade interface {
	// Submit creates or edits the caller's own review for a restaurant. Fails
	// with ErrValidation for a rating outside 1..5, and ErrForbidden if the
	// caller has no completed booking at the restaurant (verified-review rule).
	Submit(ctx context.Context, userID uuid.UUID, in SubmitInput) (*domain.Review, error)
	// DeleteOwn removes the caller's own review (idempotent).
	DeleteOwn(ctx context.Context, userID, restaurantID uuid.UUID) error
	// GetOwn returns the caller's own review, ErrNotFound if none.
	GetOwn(ctx context.Context, userID, restaurantID uuid.UUID) (*domain.Review, error)

	// ListPublished returns a restaurant's published reviews, paginated.
	ListPublished(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.ReviewListItem, int, error)
	// Rating returns a restaurant's aggregate (avg + count) over published reviews.
	Rating(ctx context.Context, restaurantID uuid.UUID) (domain.RatingAggregate, error)

	// Reply posts the venue's reply to a review. Requires PermStaffManage at
	// the review's own restaurant (or superadmin).
	Reply(ctx context.Context, actor Actor, reviewID uuid.UUID, reply string) (*domain.Review, error)
	// Moderate hides or unhides a review. Same authorization as Reply.
	Moderate(ctx context.Context, actor Actor, reviewID uuid.UUID, status domain.ReviewStatus) (*domain.Review, error)
}

// SubmitInput carries a guest's review content.
type SubmitInput struct {
	RestaurantID uuid.UUID
	Rating       int
	Body         string
}

type facade struct {
	repo     domain.ReviewRepository
	bookings bookingLookup
	staff    staffRoles
	clock    func() time.Time
}

// NewFacade constructs the reviews Facade.
func NewFacade(repo domain.ReviewRepository, bookings bookingLookup, staff staffRoles) Facade {
	return &facade{repo: repo, bookings: bookings, staff: staff, clock: time.Now}
}

// Submit enforces the rating range and the verified-review rule, then upserts.
func (f *facade) Submit(ctx context.Context, userID uuid.UUID, in SubmitInput) (*domain.Review, error) {
	if !domain.ValidRating(in.Rating) {
		return nil, fmt.Errorf("%w: rating must be between %d and %d", domain.ErrValidation, domain.ReviewRatingMin, domain.ReviewRatingMax)
	}
	ok, err := f.hasCompletedBooking(ctx, userID, in.RestaurantID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: only guests with a completed booking at this restaurant may review it", domain.ErrForbidden)
	}
	rv := &domain.Review{RestaurantID: in.RestaurantID, UserID: userID, Rating: in.Rating, Body: in.Body}
	if err := f.repo.Upsert(ctx, rv); err != nil {
		return nil, err
	}
	return rv, nil
}

// hasCompletedBooking reports whether userID has at least one completed
// booking at restaurantID. Uses a PerPage:1 lookup — only the total count
// matters, not the rows.
func (f *facade) hasCompletedBooking(ctx context.Context, userID, restaurantID uuid.UUID) (bool, error) {
	uid := userID
	rid := restaurantID
	_, total, err := f.bookings.List(ctx, domain.BookingFilter{
		RestaurantID: &rid,
		UserID:       &uid,
		Statuses:     []domain.BookingStatus{domain.BookingCompleted},
		PerPage:      1,
	})
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

func (f *facade) DeleteOwn(ctx context.Context, userID, restaurantID uuid.UUID) error {
	return f.repo.DeleteOwn(ctx, restaurantID, userID)
}

func (f *facade) GetOwn(ctx context.Context, userID, restaurantID uuid.UUID) (*domain.Review, error) {
	return f.repo.GetOwn(ctx, restaurantID, userID)
}

func (f *facade) ListPublished(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]domain.ReviewListItem, int, error) {
	return f.repo.ListPublished(ctx, restaurantID, page, perPage)
}

func (f *facade) Rating(ctx context.Context, restaurantID uuid.UUID) (domain.RatingAggregate, error) {
	return f.repo.Aggregate(ctx, restaurantID)
}

func (f *facade) Reply(ctx context.Context, actor Actor, reviewID uuid.UUID, reply string) (*domain.Review, error) {
	rv, err := f.authorizeStaffOnReview(ctx, actor, reviewID)
	if err != nil {
		return nil, err
	}
	now := f.clock()
	if err := f.repo.SetReply(ctx, reviewID, reply, now); err != nil {
		return nil, err
	}
	rv.OwnerReply = &reply
	rv.RepliedAt = &now
	return rv, nil
}

func (f *facade) Moderate(ctx context.Context, actor Actor, reviewID uuid.UUID, status domain.ReviewStatus) (*domain.Review, error) {
	if !status.Valid() {
		return nil, fmt.Errorf("%w: unknown review status %q", domain.ErrValidation, status)
	}
	rv, err := f.authorizeStaffOnReview(ctx, actor, reviewID)
	if err != nil {
		return nil, err
	}
	if err := f.repo.SetStatus(ctx, reviewID, status); err != nil {
		return nil, err
	}
	rv.Status = status
	return rv, nil
}

// authorizeStaffOnReview loads the review, then checks the actor is a
// manager-or-owner of its OWN restaurant (or a superadmin). Resolving the
// review first means authorization is always against the row's real
// restaurant, never a caller-supplied id — the same cross-tenant IDOR guard
// the staff-roster usecase uses. Returns the loaded review so callers avoid a
// second read.
//
// Why "manager-or-owner" and not PermStaffManage: reply/moderation is an
// everyday cabinet action a venue's manager must be able to do, but never a
// hostess. The RBAC matrix has no single permission that means exactly
// {owner, manager} without also meaning something semantically unrelated
// (payment.refund), and inventing a new permission is out of scope, so this
// reuses the existing StaffRole hierarchy: a manager or owner STRICTLY
// outranks a hostess, which is precisely the {owner, manager} set.
func (f *facade) authorizeStaffOnReview(ctx context.Context, actor Actor, reviewID uuid.UUID) (*domain.Review, error) {
	rv, err := f.repo.GetByID(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	if actor.Role == domain.RoleAdmin {
		return rv, nil
	}
	role, ok, err := f.staffRoleAt(ctx, actor.UserID, rv.RestaurantID)
	if err != nil {
		return nil, err
	}
	// A hostess (or a non-staff caller, whose role is the zero value) does not
	// outrank a hostess, so both are denied; a manager and an owner both do.
	if !ok || !role.Outranks(domain.StaffRoleHostess) {
		return nil, fmt.Errorf("%w: only this restaurant's manager or owner (or a superadmin) may reply to or moderate its reviews", domain.ErrForbidden)
	}
	return rv, nil
}

// staffRoleAt resolves userID's StaffRole at restaurantID. ok is false when
// userID is not staff of that restaurant at all.
func (f *facade) staffRoleAt(ctx context.Context, userID, restaurantID uuid.UUID) (domain.StaffRole, bool, error) {
	ms, err := f.staff.ListByUser(ctx, userID)
	if err != nil {
		return "", false, err
	}
	for _, m := range ms {
		if m.RestaurantID == restaurantID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

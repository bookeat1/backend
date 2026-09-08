package reviews

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fakes ---

type fakeReviewRepo struct {
	upserted   *domain.Review
	byID       map[uuid.UUID]*domain.Review
	setStatus  []domain.ReviewStatus
	setReply   []string
	statusErr  error
	replyErr   error
	upsertErr  error
	getByIDErr error
}

func (f *fakeReviewRepo) Upsert(_ context.Context, r *domain.Review) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	r.Status = domain.ReviewPublished
	f.upserted = r
	return nil
}

func (f *fakeReviewRepo) GetOwn(_ context.Context, _, _ uuid.UUID) (*domain.Review, error) {
	return nil, domain.ErrNotFound
}

func (f *fakeReviewRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Review, error) {
	if f.getByIDErr != nil {
		return nil, f.getByIDErr
	}
	rv, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return rv, nil
}

func (f *fakeReviewRepo) DeleteOwn(_ context.Context, _, _ uuid.UUID) error { return nil }

func (f *fakeReviewRepo) ListPublished(_ context.Context, _ uuid.UUID, _, _ int) ([]domain.ReviewListItem, int, error) {
	return nil, 0, nil
}

func (f *fakeReviewRepo) Aggregate(_ context.Context, rid uuid.UUID) (domain.RatingAggregate, error) {
	return domain.RatingAggregate{RestaurantID: rid}, nil
}

func (f *fakeReviewRepo) SetStatus(_ context.Context, _ uuid.UUID, s domain.ReviewStatus) error {
	if f.statusErr != nil {
		return f.statusErr
	}
	f.setStatus = append(f.setStatus, s)
	return nil
}

func (f *fakeReviewRepo) SetReply(_ context.Context, _ uuid.UUID, reply string, _ time.Time) error {
	if f.replyErr != nil {
		return f.replyErr
	}
	f.setReply = append(f.setReply, reply)
	return nil
}

type fakeBookings struct {
	total int
	err   error
	gotF  domain.BookingFilter
}

func (f *fakeBookings) List(_ context.Context, filter domain.BookingFilter) ([]domain.Booking, int, error) {
	f.gotF = filter
	return nil, f.total, f.err
}

type fakeStaff struct {
	rows []domain.RestaurantManager
	err  error
}

func (f *fakeStaff) ListByUser(_ context.Context, _ uuid.UUID) ([]domain.RestaurantManager, error) {
	return f.rows, f.err
}

// --- Submit / verified-review rule ---

func TestSubmit_RejectedWithoutCompletedBooking(t *testing.T) {
	repo := &fakeReviewRepo{}
	f := NewFacade(repo, &fakeBookings{total: 0}, &fakeStaff{})

	_, err := f.Submit(context.Background(), uuid.New(), SubmitInput{RestaurantID: uuid.New(), Rating: 5})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expected ErrForbidden without a completed booking, got %v", err)
	}
	if repo.upserted != nil {
		t.Fatal("no review must be written when the guest has no completed booking")
	}
}

func TestSubmit_AllowedWithCompletedBooking(t *testing.T) {
	repo := &fakeReviewRepo{}
	bk := &fakeBookings{total: 1}
	f := NewFacade(repo, bk, &fakeStaff{})

	uid, rid := uuid.New(), uuid.New()
	rv, err := f.Submit(context.Background(), uid, SubmitInput{RestaurantID: rid, Rating: 4, Body: "good"})
	if err != nil {
		t.Fatalf("Submit with completed booking: %v", err)
	}
	if rv == nil || repo.upserted == nil || repo.upserted.Rating != 4 || repo.upserted.Body != "good" {
		t.Fatalf("review not written as expected: %+v", repo.upserted)
	}
	// The booking lookup must be scoped to this user, this restaurant, completed only.
	if bk.gotF.UserID == nil || *bk.gotF.UserID != uid || bk.gotF.RestaurantID == nil || *bk.gotF.RestaurantID != rid {
		t.Fatalf("booking filter not scoped to (user, restaurant): %+v", bk.gotF)
	}
	if len(bk.gotF.Statuses) != 1 || bk.gotF.Statuses[0] != domain.BookingCompleted {
		t.Fatalf("booking filter must ask for completed only, got %+v", bk.gotF.Statuses)
	}
}

func TestSubmit_RatingOutOfRangeRejected(t *testing.T) {
	repo := &fakeReviewRepo{}
	// total:1 so the completed-booking rule would pass — the rating check must
	// fire FIRST and independently.
	f := NewFacade(repo, &fakeBookings{total: 1}, &fakeStaff{})

	for _, bad := range []int{0, 6, -1, 100} {
		_, err := f.Submit(context.Background(), uuid.New(), SubmitInput{RestaurantID: uuid.New(), Rating: bad})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("rating %d: expected ErrValidation, got %v", bad, err)
		}
	}
	if repo.upserted != nil {
		t.Fatal("no review must be written for an invalid rating")
	}
}

// --- staff moderation / reply authorization ---

func reviewAt(rid uuid.UUID) (uuid.UUID, *fakeReviewRepo) {
	reviewID := uuid.New()
	repo := &fakeReviewRepo{byID: map[uuid.UUID]*domain.Review{
		reviewID: {ID: reviewID, RestaurantID: rid, UserID: uuid.New(), Rating: 3, Status: domain.ReviewPublished},
	}}
	return reviewID, repo
}

func staffWith(userID, rid uuid.UUID, role domain.StaffRole) *fakeStaff {
	return &fakeStaff{rows: []domain.RestaurantManager{{UserID: userID, RestaurantID: rid, Role: role}}}
}

func TestModerate_HostessForbidden(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	actorID := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, rid, domain.StaffRoleHostess))

	_, err := f.Moderate(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, reviewID, domain.ReviewHidden)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not moderate, got %v", err)
	}
	if len(repo.setStatus) != 0 {
		t.Fatal("status must not change when a hostess is denied")
	}
}

func TestModerate_ManagerAllowed(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	actorID := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, rid, domain.StaffRoleManager))

	rv, err := f.Moderate(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, reviewID, domain.ReviewHidden)
	if err != nil {
		t.Fatalf("a manager must be able to moderate: %v", err)
	}
	if rv.Status != domain.ReviewHidden || len(repo.setStatus) != 1 || repo.setStatus[0] != domain.ReviewHidden {
		t.Fatalf("moderation not applied: %+v / %v", rv, repo.setStatus)
	}
}

func TestModerate_OwnerAllowed(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	actorID := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, rid, domain.StaffRoleOwner))

	if _, err := f.Moderate(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, reviewID, domain.ReviewHidden); err != nil {
		t.Fatalf("an owner must be able to moderate: %v", err)
	}
	if len(repo.setStatus) != 1 {
		t.Fatal("owner moderation not applied")
	}
}

func TestModerate_AdminBypassesStaffLookup(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	// staff repo would error if consulted — a superadmin must not need it.
	f := NewFacade(repo, &fakeBookings{}, &fakeStaff{err: errors.New("must not be called")})

	if _, err := f.Moderate(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleAdmin}, reviewID, domain.ReviewHidden); err != nil {
		t.Fatalf("superadmin must moderate without a staff lookup: %v", err)
	}
}

func TestModerate_StaffOfAnotherRestaurantForbidden(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	actorID := uuid.New()
	// Manager, but of a DIFFERENT restaurant.
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, uuid.New(), domain.StaffRoleManager))

	_, err := f.Moderate(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, reviewID, domain.ReviewHidden)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a manager of another restaurant must not moderate this review, got %v", err)
	}
}

func TestModerate_InvalidStatusRejected(t *testing.T) {
	rid := uuid.New()
	reviewID, repo := reviewAt(rid)
	actorID := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, rid, domain.StaffRoleOwner))

	_, err := f.Moderate(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, reviewID, domain.ReviewStatus("deleted"))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown status must be ErrValidation, got %v", err)
	}
}

func TestReply_ManagerAllowedHostessForbidden(t *testing.T) {
	rid := uuid.New()

	reviewID, repo := reviewAt(rid)
	mgr := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(mgr, rid, domain.StaffRoleManager))
	rv, err := f.Reply(context.Background(), Actor{UserID: mgr, Role: domain.RoleRestaurant}, reviewID, "thanks!")
	if err != nil {
		t.Fatalf("manager reply: %v", err)
	}
	if rv.OwnerReply == nil || *rv.OwnerReply != "thanks!" || rv.RepliedAt == nil || len(repo.setReply) != 1 {
		t.Fatalf("reply not applied: %+v", rv)
	}

	reviewID2, repo2 := reviewAt(rid)
	host := uuid.New()
	f2 := NewFacade(repo2, &fakeBookings{}, staffWith(host, rid, domain.StaffRoleHostess))
	if _, err := f2.Reply(context.Background(), Actor{UserID: host, Role: domain.RoleRestaurant}, reviewID2, "hi"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a hostess must not reply, got %v", err)
	}
	if len(repo2.setReply) != 0 {
		t.Fatal("no reply must be written when a hostess is denied")
	}
}

func TestReply_ReviewNotFound(t *testing.T) {
	repo := &fakeReviewRepo{byID: map[uuid.UUID]*domain.Review{}}
	actorID := uuid.New()
	f := NewFacade(repo, &fakeBookings{}, staffWith(actorID, uuid.New(), domain.StaffRoleOwner))
	if _, err := f.Reply(context.Background(), Actor{UserID: actorID, Role: domain.RoleAdmin}, uuid.New(), "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a missing review, got %v", err)
	}
}

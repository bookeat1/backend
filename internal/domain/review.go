package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ReviewStatus is a review's moderation state, stored as VARCHAR. A review is
// born Published; a venue's staff (PermStaffManage) may Hide it and unhide it
// again. Hidden reviews are excluded from every public read (listing and
// aggregate) but are never deleted — the guest can still see and edit their
// own hidden review.
type ReviewStatus string

const (
	ReviewPublished ReviewStatus = "published"
	ReviewHidden    ReviewStatus = "hidden"
)

// Valid reports whether s is a known review status.
func (s ReviewStatus) Valid() bool {
	return s == ReviewPublished || s == ReviewHidden
}

// Review rating bounds. Rating is an integer 1..5 — the mobile star widget's
// domain — validated by the shared ValidRating (see booking_survey.go) and by
// a CHECK constraint on the column. The bounds are named here purely for
// human-readable validation messages.
const (
	ReviewRatingMin = 1
	ReviewRatingMax = 5
)

// Review is a guest's rating + optional text for one restaurant, plus the
// venue's optional single reply. A review is "verified" by construction: the
// usecase only writes one when the guest has a COMPLETED booking at the
// restaurant, so there is no separate verified flag to keep in sync.
type Review struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	UserID       uuid.UUID
	Rating       int
	Body         string
	Status       ReviewStatus
	// OwnerReply / RepliedAt are set together or both nil (a DB CHECK enforces
	// the pairing). A venue posts at most one reply per review.
	OwnerReply *string
	RepliedAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ReviewListItem is one row of the public reviews listing: the review plus the
// reviewer's display name, joined from users so the client need not resolve it
// separately. Mirrors the RestaurantListItem embed-plus-extra pattern.
type ReviewListItem struct {
	Review
	AuthorName string
}

// RatingAggregate is a restaurant's published-review summary. Average is 0
// when Count is 0 (no reviews yet) — callers treat Count == 0 as "unrated",
// never dividing by it.
type RatingAggregate struct {
	RestaurantID uuid.UUID
	Average      float64
	Count        int
}

// ReviewRepository persists guest reviews. Get* return ErrNotFound when absent.
type ReviewRepository interface {
	// Upsert inserts the guest's review, or overwrites the rating/body of
	// their existing one for the same restaurant (ON CONFLICT on
	// (restaurant_id, user_id)). It deliberately does NOT change status or the
	// owner_reply on an edit: a review the venue hid stays hidden, and an
	// existing reply survives the guest tweaking their text. Returns
	// ErrNotFound if the restaurant does not exist.
	Upsert(ctx context.Context, r *Review) error
	// GetOwn returns userID's review for restaurantID, ErrNotFound if absent.
	GetOwn(ctx context.Context, restaurantID, userID uuid.UUID) (*Review, error)
	// GetByID returns a review by its id (staff moderation/reply resolve the
	// target restaurant from the row), ErrNotFound if absent.
	GetByID(ctx context.Context, id uuid.UUID) (*Review, error)
	// DeleteOwn removes userID's review for restaurantID. Idempotent: deleting
	// a review that isn't there is a silent no-op, not an error.
	DeleteOwn(ctx context.Context, restaurantID, userID uuid.UUID) error
	// ListPublished returns restaurantID's published reviews in a stable
	// created_at DESC, id DESC order, paginated, plus the total published
	// count. page is 1-based; perPage is caller-normalized.
	ListPublished(ctx context.Context, restaurantID uuid.UUID, page, perPage int) ([]ReviewListItem, int, error)
	// Aggregate returns the AVG(rating)+COUNT(*) over restaurantID's PUBLISHED
	// reviews only. A restaurant with no published reviews yields
	// {Average: 0, Count: 0}.
	Aggregate(ctx context.Context, restaurantID uuid.UUID) (RatingAggregate, error)
	// SetStatus moderates a review (hide/unhide). Returns ErrNotFound if id is
	// absent.
	SetStatus(ctx context.Context, id uuid.UUID, status ReviewStatus) error
	// SetReply writes the venue's reply and its timestamp. Returns ErrNotFound
	// if id is absent.
	SetReply(ctx context.Context, id uuid.UUID, reply string, at time.Time) error
}

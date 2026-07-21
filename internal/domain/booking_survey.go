package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// RestaurantSurvey is the post-visit questionnaire. Field names and nullability
// mirror the Supabase source table: all ratings are required (1..5), NPS is
// required (0..10), and BookingID is optional — a guest may rate a place
// without having booked through us. At most one survey per booking.
type RestaurantSurvey struct {
	ID             uuid.UUID
	BookingID      *uuid.UUID
	RestaurantID   uuid.UUID
	UserID         uuid.UUID
	RatingOverall  int
	RatingFood     int
	RatingService  int
	RatingAmbience int
	NPS            int
	Comment        *string
	Dismissed      bool
	CreatedAt      time.Time
}

// ValidRating reports whether a 1..5 rating is acceptable.
func ValidRating(v int) bool { return v >= 1 && v <= 5 }

// ValidNPS reports whether a 0..10 NPS score is acceptable.
func ValidNPS(v int) bool { return v >= 0 && v <= 10 }

// RestaurantSurveyRepository persists post-visit surveys.
type RestaurantSurveyRepository interface {
	// Create returns ErrAlreadyExists when the booking already has a survey.
	Create(ctx context.Context, s *RestaurantSurvey) error
	// GetByBooking returns ErrNotFound when the booking has no survey.
	GetByBooking(ctx context.Context, bookingID uuid.UUID) (*RestaurantSurvey, error)
	ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, limit, offset int) ([]RestaurantSurvey, error)
}

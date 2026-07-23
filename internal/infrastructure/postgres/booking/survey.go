package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Surveys implements domain.RestaurantSurveyRepository.
type Surveys struct{ pool sqltx.Querier }

// NewSurveys builds the post-visit survey repository.
func NewSurveys(pool sqltx.Querier) *Surveys { return &Surveys{pool: pool} }

var _ domain.RestaurantSurveyRepository = (*Surveys)(nil)

const surveyCols = `id, booking_id, restaurant_id, user_id, rating_overall, rating_food,
	rating_service, rating_ambience, nps, comment, dismissed, created_at`

func (r *Surveys) Create(ctx context.Context, s *domain.RestaurantSurvey) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	q := `INSERT INTO restaurant_surveys (` + surveyCols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, s.ID, s.BookingID, s.RestaurantID,
		s.UserID, s.RatingOverall, s.RatingFood, s.RatingService, s.RatingAmbience,
		s.NPS, s.Comment, s.Dismissed, s.CreatedAt); err != nil {
		return mapWrite(err, "create survey")
	}
	return nil
}

func (r *Surveys) GetByBooking(ctx context.Context, bookingID uuid.UUID) (*domain.RestaurantSurvey, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+surveyCols+` FROM restaurant_surveys WHERE booking_id=$1`, bookingID)
	s, err := scanSurvey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get survey: %w", err)
	}
	return s, nil
}

func (r *Surveys) ListByRestaurant(ctx context.Context, restaurantID uuid.UUID, limit, offset int) ([]domain.RestaurantSurvey, error) {
	limit, offset = window(limit, offset)
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+surveyCols+` FROM restaurant_surveys WHERE restaurant_id=$1
		 ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, restaurantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list surveys: %w", err)
	}
	defer rows.Close()
	var out []domain.RestaurantSurvey
	for rows.Next() {
		s, err := scanSurvey(rows)
		if err != nil {
			return nil, fmt.Errorf("list surveys: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func scanSurvey(row scanner) (*domain.RestaurantSurvey, error) {
	var s domain.RestaurantSurvey
	if err := row.Scan(&s.ID, &s.BookingID, &s.RestaurantID, &s.UserID, &s.RatingOverall,
		&s.RatingFood, &s.RatingService, &s.RatingAmbience, &s.NPS, &s.Comment,
		&s.Dismissed, &s.CreatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

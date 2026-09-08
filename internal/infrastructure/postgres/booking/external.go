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

// ExternalReservations implements domain.ExternalReservationRepository.
//
// It owns two tables at once: external_reservations (the source-of-record) and
// the external-owned rows of booking_tables (the physical exclusion). Create
// writes both in one call so the GiST exclusion constraint on booking_tables is
// the single arbiter of "is this slot free" for bookings and holds alike. slot
// is stored as a half-open tstzrange built in SQL; no range type crosses the
// driver boundary.
type ExternalReservations struct{ pool sqltx.Querier }

// NewExternalReservations builds the external-reservation repository.
func NewExternalReservations(pool sqltx.Querier) *ExternalReservations {
	return &ExternalReservations{pool: pool}
}

var _ domain.ExternalReservationRepository = (*ExternalReservations)(nil)

const externalCols = `id, restaurant_id, table_id, lower(slot), upper(slot),
	source, external_ref, note, created_by, active, created_at, updated_at`

func (r *ExternalReservations) Create(ctx context.Context, res *domain.ExternalReservation, enforceTableIDs []uuid.UUID) error {
	q := sqltx.From(ctx, r.pool)
	now := time.Now()
	if res.CreatedAt.IsZero() {
		res.CreatedAt = now
	}
	if res.ID == uuid.Nil {
		res.ID = uuid.New()
	}
	if _, err := q.Exec(ctx,
		`INSERT INTO external_reservations
		   (id, restaurant_id, table_id, slot, source, external_ref, note, created_by,
		    active, created_at, updated_at)
		 VALUES ($1,$2,$3,tstzrange($4,$5,'[)'),$6,$7,$8,$9,true,$10,$10)`,
		res.ID, res.RestaurantID, res.TableID, res.StartsAt, res.EndsAt,
		string(res.Source), res.ExternalRef, res.Note, res.CreatedBy, res.CreatedAt,
	); err != nil {
		return mapWrite(err, "create external reservation")
	}

	// One enforcement row per occupied table, booking_id NULL. These share the
	// exclusion constraint with native bookings, so an overlap with either a
	// booking or another hold is rejected here with 23P01 → ErrAlreadyExists.
	for _, tid := range enforceTableIDs {
		if _, err := q.Exec(ctx,
			`INSERT INTO booking_tables
			   (id, booking_id, external_reservation_id, table_id, slot, active, created_at)
			 VALUES ($1, NULL, $2, $3, tstzrange($4,$5,'[)'), true, $6)`,
			uuid.New(), res.ID, tid, res.StartsAt, res.EndsAt, res.CreatedAt,
		); err != nil {
			return mapWrite(err, "reserve table for external hold")
		}
	}
	res.Active = true
	return nil
}

func (r *ExternalReservations) GetByID(ctx context.Context, id uuid.UUID) (*domain.ExternalReservation, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT `+externalCols+` FROM external_reservations WHERE id=$1`, id)
	res, err := scanExternal(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: external reservation", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("get external reservation: %w", err)
	}
	return res, nil
}

func (r *ExternalReservations) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM external_reservations WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete external reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: external reservation", domain.ErrNotFound)
	}
	return nil
}

func (r *ExternalReservations) List(ctx context.Context, restaurantID uuid.UUID, from, to time.Time) ([]domain.ExternalReservation, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+externalCols+`
		 FROM external_reservations
		 WHERE restaurant_id=$1 AND active AND slot && tstzrange($2,$3,'[)')
		 ORDER BY lower(slot), id`, restaurantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list external reservations: %w", err)
	}
	defer rows.Close()
	var out []domain.ExternalReservation
	for rows.Next() {
		res, err := scanExternal(rows)
		if err != nil {
			return nil, fmt.Errorf("list external reservations: %w", err)
		}
		out = append(out, *res)
	}
	return out, rows.Err()
}

func scanExternal(s scanner) (*domain.ExternalReservation, error) {
	var res domain.ExternalReservation
	var source string
	if err := s.Scan(
		&res.ID, &res.RestaurantID, &res.TableID, &res.StartsAt, &res.EndsAt,
		&source, &res.ExternalRef, &res.Note, &res.CreatedBy, &res.Active,
		&res.CreatedAt, &res.UpdatedAt,
	); err != nil {
		return nil, err
	}
	res.Source = domain.ExternalSource(source)
	return &res, nil
}

package booking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Repository implements domain.BookingRepository.
type Repository struct{ pool sqltx.Querier }

// New builds the booking repository.
func New(pool sqltx.Querier) *Repository { return &Repository{pool: pool} }

var _ domain.BookingRepository = (*Repository)(nil)

const cols = `id, restaurant_id, user_id, name, phone, email, phone_normalized,
	guests, starts_at, ends_at, status, source, notes, promotion_id, event_id,
	created_by_admin, forced_placement, confirmed_at, arrived_at, cancelled_at,
	cancelled_by, cancellation_reason_code, cancellation_reason,
	late_notification_sent, user_notified_late_at, user_late_message,
	reminder_60_sent_at, reminder_30_sent_at, original_booking_time_text,
	created_at, updated_at`

func (r *Repository) Create(ctx context.Context, b *domain.Booking) error {
	now := time.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	q := `INSERT INTO bookings (` + cols + `) VALUES
		($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
		 $21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)`
	if _, err := sqltx.From(ctx, r.pool).Exec(ctx, q, r.args(b)...); err != nil {
		return mapWrite(err, "create booking")
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, b *domain.Booking) error {
	b.UpdatedAt = time.Now()
	// Built explicitly (not sliced out of r.args) so adding an INSERT column
	// can't silently shift the UPDATE placeholders. Update omits created_at,
	// restaurant_id and status: status moves only through UpdateStatus.
	q := `UPDATE bookings SET user_id=$2, name=$3, phone=$4, email=$5,
		phone_normalized=$6, guests=$7, starts_at=$8, ends_at=$9, source=$10,
		notes=$11, promotion_id=$12, event_id=$13, created_by_admin=$14,
		forced_placement=$15, confirmed_at=$16, arrived_at=$17, cancelled_at=$18,
		cancelled_by=$19, cancellation_reason_code=$20, cancellation_reason=$21,
		late_notification_sent=$22, user_notified_late_at=$23, user_late_message=$24,
		reminder_60_sent_at=$25, reminder_30_sent_at=$26,
		original_booking_time_text=$27, updated_at=$28
		WHERE id=$1`
	args := []any{
		b.ID, b.UserID, b.Name, b.Phone, b.Email, b.PhoneNormalized, b.Guests,
		b.StartsAt, b.EndsAt, string(b.Source), b.Notes, b.PromotionID, b.EventID,
		b.CreatedByAdmin, b.ForcedPlacement, b.ConfirmedAt, b.ArrivedAt,
		b.CancelledAt, cancelledByToDB(b.CancelledBy), b.CancellationReasonCode,
		b.CancellationReason, b.LateNotificationSent, b.UserNotifiedLateAt,
		b.UserLateMessage, b.Reminder60SentAt, b.Reminder30SentAt,
		b.OriginalBookingTime, b.UpdatedAt,
	}
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, args...)
	if err != nil {
		return mapWrite(err, "update booking")
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Booking, error) {
	row := sqltx.From(ctx, r.pool).QueryRow(ctx, `SELECT `+cols+` FROM bookings WHERE id=$1`, id)
	b, err := scanBooking(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get booking: %w", err)
	}
	return b, nil
}

func (r *Repository) List(ctx context.Context, f domain.BookingFilter) ([]domain.Booking, int, error) {
	where := []string{"true"}
	args := []any{}
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.RestaurantID != nil {
		add("restaurant_id = $%d", *f.RestaurantID)
	}
	if f.UserID != nil {
		add("user_id = $%d", *f.UserID)
	}
	if len(f.Statuses) > 0 {
		add("status = ANY($%d)", statusStrings(f.Statuses))
	}
	// Half-open interval on the bare column so idx_bookings_restaurant_starts
	// stays usable — casting starts_at to date would disable it.
	if f.From != nil {
		add("starts_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("starts_at < $%d", *f.To)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count bookings: %w", err)
	}

	limit, offset := page(f.Page, f.PerPage)
	args = append(args, limit, offset)
	q := `SELECT ` + cols + ` FROM bookings WHERE ` + whereSQL + `
		ORDER BY starts_at DESC, id
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()
	out, err := scanBookings(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("list bookings: %w", err)
	}
	return out, total, nil
}

// UpdateStatus writes the new status together with the timestamp column that
// belongs to it. booking_tables.active is NOT touched here — the DB trigger on
// bookings.status owns that column.
func (r *Repository) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.BookingStatus, at time.Time) error {
	set := []string{"status=$2", "updated_at=$3"}
	switch status {
	case domain.BookingConfirmed:
		set = append(set, "confirmed_at=$3")
	case domain.BookingArrived:
		set = append(set, "arrived_at=$3")
	case domain.BookingCancelled:
		set = append(set, "cancelled_at=$3")
	}
	q := `UPDATE bookings SET ` + strings.Join(set, ", ") + ` WHERE id=$1`
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx, q, id, string(status), at)
	if err != nil {
		return fmt.Errorf("update booking status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ClaimDue locks due bookings with FOR UPDATE SKIP LOCKED. It must run inside a
// TxManager transaction — outside one the locks are released immediately and
// two workers can pick up the same row.
//
// The caller picks the cutoff column: created_at is the confirm-SLA clock,
// ends_at the visit-window clock. The ORDER BY is that same column, oldest
// first — ordering by anything else lets a batch smaller than the candidate set
// starve the rows that have waited longest (a stale booking pushed out of every
// batch by fresher ones is never processed at all).
//
// The column reaches the SQL text, so it is taken from the closed
// domain.ClaimColumn set and rejected otherwise — never interpolated raw.
func (r *Repository) ClaimDue(ctx context.Context, statuses []domain.BookingStatus, by domain.ClaimColumn, before time.Time, limit int) ([]domain.Booking, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	if !by.Valid() {
		return nil, fmt.Errorf("%w: unknown claim column %q", domain.ErrValidation, by)
	}
	col := string(by)
	limit, _ = window(limit, 0)
	q := `SELECT ` + cols + ` FROM bookings
		WHERE status = ANY($1)
		  AND ` + col + ` < $2
		ORDER BY ` + col + `, id
		LIMIT $3
		FOR UPDATE SKIP LOCKED`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, statusStrings(statuses), before, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due bookings: %w", err)
	}
	defer rows.Close()
	out, err := scanBookings(rows)
	if err != nil {
		return nil, fmt.Errorf("claim due bookings: %w", err)
	}
	return out, nil
}

func (r *Repository) args(b *domain.Booking) []any {
	return []any{
		b.ID, b.RestaurantID, b.UserID, b.Name, b.Phone, b.Email, b.PhoneNormalized,
		b.Guests, b.StartsAt, b.EndsAt, string(b.Status), string(b.Source), b.Notes,
		b.PromotionID, b.EventID, b.CreatedByAdmin, b.ForcedPlacement, b.ConfirmedAt,
		b.ArrivedAt, b.CancelledAt, cancelledByToDB(b.CancelledBy),
		b.CancellationReasonCode, b.CancellationReason, b.LateNotificationSent,
		b.UserNotifiedLateAt, b.UserLateMessage, b.Reminder60SentAt,
		b.Reminder30SentAt, b.OriginalBookingTime, b.CreatedAt, b.UpdatedAt,
	}
}

func scanBookings(rows pgx.Rows) ([]domain.Booking, error) {
	var out []domain.Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func scanBooking(row scanner) (*domain.Booking, error) {
	var b domain.Booking
	var status, source string
	var cancelledBy *string
	if err := row.Scan(
		&b.ID, &b.RestaurantID, &b.UserID, &b.Name, &b.Phone, &b.Email,
		&b.PhoneNormalized, &b.Guests, &b.StartsAt, &b.EndsAt, &status, &source,
		&b.Notes, &b.PromotionID, &b.EventID, &b.CreatedByAdmin, &b.ForcedPlacement,
		&b.ConfirmedAt, &b.ArrivedAt, &b.CancelledAt, &cancelledBy,
		&b.CancellationReasonCode, &b.CancellationReason, &b.LateNotificationSent,
		&b.UserNotifiedLateAt, &b.UserLateMessage, &b.Reminder60SentAt,
		&b.Reminder30SentAt, &b.OriginalBookingTime, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	b.Status = domain.BookingStatus(status)
	b.Source = domain.BookingSource(source)
	if cancelledBy != nil {
		c := domain.CancelledBy(*cancelledBy)
		b.CancelledBy = &c
	}
	return &b, nil
}

func statusStrings(ss []domain.BookingStatus) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

func cancelledByToDB(c *domain.CancelledBy) any {
	if c == nil {
		return nil
	}
	return string(*c)
}

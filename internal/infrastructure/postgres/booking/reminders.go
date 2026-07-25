package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// Reminders implements domain.BookingReminderRepository over the same bookings
// table as Repository. It is a separate type so the reminder marker
// (guest_reminder_sent_at) has exactly two writers — MarkReminderSent and the
// migration's backfill — and never rides along in Repository.Update's column
// list, where a stale in-memory Booking could silently reset it.
type Reminders struct{ pool sqltx.Querier }

// NewReminders builds the guest-reminder repository.
func NewReminders(pool sqltx.Querier) *Reminders { return &Reminders{pool: pool} }

var _ domain.BookingReminderRepository = (*Reminders)(nil)

// reminderLiveStatuses are the statuses a booking must be in to deserve a
// reminder: the visit is still expected to happen. cancelled / no_show /
// completed are excluded here AND again in MarkReminderSent's WHERE clause.
var reminderLiveStatuses = []string{
	string(domain.BookingPending),
	string(domain.BookingConfirmed),
	string(domain.BookingWaitlist),
}

// ClaimDueReminders locks the bookings whose visit falls in (from, to].
//
// `created_at <= starts_at - ($2 - $1)` drops a booking made INSIDE its own
// reminder window: the interval is derived in SQL from the two bounds the caller
// already passes, so the reminder lead is never a third, separately-typed
// parameter (a bound parameter reused in two different type positions is what
// produced the SQLSTATE 42P08 we hit before).
func (r *Reminders) ClaimDueReminders(ctx context.Context, from, to time.Time, limit int) ([]domain.Booking, error) {
	limit, _ = window(limit, 0)
	q := `SELECT ` + cols + ` FROM bookings
		WHERE guest_reminder_sent_at IS NULL
		  AND user_id IS NOT NULL
		  AND status = ANY($3)
		  AND starts_at > $1
		  AND starts_at <= $2
		  AND created_at <= starts_at - ($2 - $1)
		ORDER BY starts_at, id
		LIMIT $4
		FOR UPDATE SKIP LOCKED`
	rows, err := sqltx.From(ctx, r.pool).Query(ctx, q, from, to, reminderLiveStatuses, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due reminders: %w", err)
	}
	defer rows.Close()
	out, err := scanBookings(rows)
	if err != nil {
		return nil, fmt.Errorf("claim due reminders: %w", err)
	}
	return out, nil
}

// MarkReminderSent stamps the marker only if it is still unset AND the booking
// is still live, and reports whether this call did it. The IS NULL predicate is
// the idempotency guard: a second pass (or a second process) updates zero rows
// and gets false, so the caller skips emitting a duplicate reminder event.
func (r *Reminders) MarkReminderSent(ctx context.Context, bookingID uuid.UUID, at time.Time) (bool, error) {
	tag, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`UPDATE bookings
		    SET guest_reminder_sent_at = $2
		  WHERE id = $1
		    AND guest_reminder_sent_at IS NULL
		    AND status = ANY($3)`,
		bookingID, at, reminderLiveStatuses)
	if err != nil {
		return false, fmt.Errorf("mark reminder sent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

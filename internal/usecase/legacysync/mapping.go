package legacysync

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

// bookingTableNamespace seeds the deterministic (UUIDv5) id of a booking_tables
// row synthesized from the old bookings.table_id single-table pointer. A fixed
// namespace + the (booking_id, table_id) pair means the same legacy assignment
// always maps to the same new-DB id, so re-runs upsert in place instead of
// inserting duplicates.
var bookingTableNamespace = uuid.MustParse("6ba7b814-9dad-11d1-80b4-00c04fd430c8")

// legacyBookingStatus maps the OLD booking_status enum to the NEW BookingStatus.
// The old enum is {pending, confirmed, waitlist, cancelled, completed, arrived};
// every value has an exact new equivalent (verified against the live old DB on
// 2026-07-24). The new-only value `no_show` never appears on the old side, so
// there is no ambiguous case to guess at here. An unknown label (a value added
// to the old enum after this was written) is reported so the row is Skipped and
// flagged rather than silently coerced.
func legacyBookingStatus(old string) (domain.BookingStatus, bool) {
	s := domain.BookingStatus(strings.TrimSpace(old))
	if s.Valid() {
		return s, true
	}
	return "", false
}

// mapBooking turns a raw old bookings row into a new-shaped Booking.
//
//   - status: old enum -> new enum, 1:1 (see legacyBookingStatus). ok=false
//     means an unrecognized status the caller must Skip+flag.
//   - phone_normalized: derived with the same normalizer the app uses
//     (internal/auth/phone), never taken from the old row (the old table has no
//     such column).
//   - ends_at: the old system stores only a single booking_date; the new schema
//     requires ends_at > starts_at. It is derived as starts_at + defaultDuration
//     (BOOKING_DEFAULT_DURATION_MINUTES, 90m). This is a display window for
//     historical rows, not a re-computation of the original reservation length,
//     which the old system did not persist.
//   - source: old rows carry no source; created_by_admin picks admin vs app
//     (both are valid new BookingSource values). Guest bookings from the old app
//     become `app`, which is the truth we care about.
//   - user_id: preserved but FK-guarded by the Sink — written only if that user
//     already exists in the new users table (old guest users are not synced in
//     increment 1), otherwise NULL. The guest identity survives via
//     name/phone/phone_normalized regardless.
//   - email is lower-cased to match the new domain convention.
//   - guests: the new schema enforces guests > 0. A non-positive value (none
//     exist in the live data today) is reported so the Sink Skips+flags it
//     rather than failing the CHECK.
func mapBooking(l LegacyBooking, defaultDuration time.Duration) (Booking, bool) {
	status, ok := legacyBookingStatus(l.Status)
	if !ok || l.Guests <= 0 {
		return Booking{}, false
	}

	source := domain.SourceApp
	if l.CreatedByAdmin {
		source = domain.SourceAdmin
	}

	return Booking{
		ID:                     l.ID,
		RestaurantID:           l.RestaurantID,
		UserID:                 l.UserID,
		Name:                   l.Name,
		Phone:                  l.Phone,
		Email:                  strings.ToLower(l.Email),
		PhoneNormalized:        phone.Normalize(l.Phone),
		Guests:                 l.Guests,
		StartsAt:               l.BookingDate,
		EndsAt:                 l.BookingDate.Add(defaultDuration),
		Status:                 string(status),
		Source:                 string(source),
		Notes:                  l.Notes,
		PromotionID:            l.PromotionID,
		EventID:                l.EventID,
		CreatedByAdmin:         l.CreatedByAdmin,
		CancelledAt:            l.CancelledAt,
		CancellationReasonCode: l.CancellationReasonCode,
		CancellationReason:     l.CancellationReason,
		LateNotificationSent:   l.LateNotificationSent,
		UserNotifiedLateAt:     l.UserNotifiedLateAt,
		UserLateMessage:        l.UserLateMessage,
		Reminder60SentAt:       l.Reminder60SentAt,
		Reminder30SentAt:       l.Reminder30SentAt,
		OriginalBookingTime:    l.OriginalBookingTime,
		CreatedAt:              l.CreatedAt,
		UpdatedAt:              l.UpdatedAt,
	}, true
}

// mapBookingTable turns a raw legacy table-assignment into a new-shaped
// booking_tables row.
//
//   - id: reused when present; for a row synthesized from bookings.table_id
//     (ID == uuid.Nil) a deterministic UUIDv5 over (booking_id, table_id) is
//     derived so re-runs are idempotent.
//   - slot: the new table stores a tstzrange; it is built as
//     [booking_date, booking_date+defaultDuration). No buffer is added — buffer
//     is a live-placement concern, and omitting it minimises false collisions
//     with the exclusion constraint on historical rows.
//   - active: mirrors the new schema's own trigger rule — a hold is active only
//     while the booking is pending/confirmed/arrived. Terminal bookings map to
//     active=false, which also keeps them clear of the exclusion constraint.
func mapBookingTable(l LegacyBookingTable, defaultDuration time.Duration) BookingTable {
	id := l.ID
	if id == uuid.Nil {
		name := "booking_table:" + l.BookingID.String() + ":" + l.TableID.String()
		id = uuid.NewSHA1(bookingTableNamespace, []byte(name))
	}
	status := domain.BookingStatus(strings.TrimSpace(l.Status))
	return BookingTable{
		ID:        id,
		BookingID: l.BookingID,
		TableID:   l.TableID,
		SlotStart: l.BookingDate,
		SlotEnd:   l.BookingDate.Add(defaultDuration),
		Active:    status.HoldsTable(),
		CreatedAt: l.CreatedAt,
		// The cursor uses the DERIVED id (unique per assignment), not the source
		// id which is uuid.Nil for a synthesized row. The Source filters/orders
		// booking_tables by updated_at only (it cannot compute the derived id in
		// SQL); the worker sorts by this cursor and drops rows <= it, so the
		// timestamp+id pair still walks each assignment exactly once.
		Cur: Cursor{UpdatedAt: l.UpdatedAt, ID: id},
	}
}

// Package legacysync (infrastructure) is the WRITE side of the one-way legacy
// sync: it upserts old-system rows into the new database by id and persists the
// per-entity cursor. Every write targets the NEW DB; nothing here ever touches
// the old one. A write that would violate a foreign key (the row's parent has
// not been synced yet) is reported as Parked so the worker retries it later; a
// write that can never land (exclusion / check constraint) is reported Skipped.
package legacysync

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/usecase/legacysync"
)

// Sink upserts into the new database.
type Sink struct{ pool sqltx.Querier }

// NewSink builds the sink over the new-DB pool.
func NewSink(pool sqltx.Querier) *Sink { return &Sink{pool: pool} }

var _ legacysync.Sink = (*Sink)(nil)

// classify turns a Postgres error into a sync Outcome:
//   - foreign_key_violation (23503): a parent is not synced yet -> Parked (retry).
//   - exclusion_violation (23P01) / check_violation (23514): the row can never
//     land -> Skipped (step over, do not retry forever).
//
// Any other error is a genuine failure the caller must surface.
func classify(err error) (legacysync.Outcome, bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return 0, false
	}
	switch pg.Code {
	case "23503":
		return legacysync.Parked, true
	case "23P01", "23514":
		return legacysync.Skipped, true
	}
	return 0, false
}

func exec(ctx context.Context, q sqltx.Querier, sql string, args ...any) (legacysync.Outcome, error) {
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		if outcome, ok := classify(err); ok {
			return outcome, nil
		}
		return 0, err
	}
	return legacysync.Written, nil
}

// RestaurantDurations returns booking_duration_minutes for every restaurant that
// has a set, positive override. The worker resolves each booking's ends_at from
// this (falling back to the env default), so a venue's own reservation length is
// honoured — same rule as cmd/etl's resolveDurationMinutes.
func (s *Sink) RestaurantDurations(ctx context.Context) (map[uuid.UUID]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, booking_duration_minutes FROM restaurants
		 WHERE booking_duration_minutes IS NOT NULL AND booking_duration_minutes > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			id  uuid.UUID
			min int
		)
		if err := rows.Scan(&id, &min); err != nil {
			return nil, err
		}
		out[id] = min
	}
	return out, rows.Err()
}

func (s *Sink) GetCursor(ctx context.Context, entity string) (legacysync.Cursor, error) {
	var (
		cur legacysync.Cursor
		id  *uuid.UUID
	)
	err := s.pool.QueryRow(ctx,
		`SELECT last_synced_at, last_synced_id FROM legacy_sync_cursor WHERE entity=$1`, entity).
		Scan(&cur.UpdatedAt, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return legacysync.Cursor{}, nil
	}
	if err != nil {
		return legacysync.Cursor{}, err
	}
	if id != nil {
		cur.ID = *id
	}
	return cur, nil
}

func (s *Sink) SetCursor(ctx context.Context, entity string, c legacysync.Cursor) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO legacy_sync_cursor (entity, last_synced_at, last_synced_id, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (entity) DO UPDATE
SET last_synced_at = EXCLUDED.last_synced_at,
    last_synced_id = EXCLUDED.last_synced_id,
    updated_at     = now()`,
		entity, c.UpdatedAt, c.ID)
	return err
}

// UpsertRestaurant writes a restaurant. category_id is left NULL: the old
// category_id points at old restaurant_categories, which increment 1 does not
// sync, so writing it would violate the new FK. The booking-policy columns are
// left at their NULL defaults (they mean "use the global env default").
func (s *Sink) UpsertRestaurant(ctx context.Context, r legacysync.Restaurant) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO restaurants
 (id, name, name_i18n, description, description_i18n, cuisine_type, cuisine_type_i18n,
  address, address_i18n, opening_hours, opening_hours_i18n, city, price_category,
  email, phone, latitude, longitude, kwaaka_restaurant_id, is_active, is_new,
  is_popular, is_premium, hidden_from_home, display_order, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
ON CONFLICT (id) DO UPDATE SET
 name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n, description=EXCLUDED.description,
 description_i18n=EXCLUDED.description_i18n, cuisine_type=EXCLUDED.cuisine_type,
 cuisine_type_i18n=EXCLUDED.cuisine_type_i18n, address=EXCLUDED.address,
 address_i18n=EXCLUDED.address_i18n, opening_hours=EXCLUDED.opening_hours,
 opening_hours_i18n=EXCLUDED.opening_hours_i18n, city=EXCLUDED.city,
 price_category=EXCLUDED.price_category, email=EXCLUDED.email, phone=EXCLUDED.phone,
 latitude=EXCLUDED.latitude, longitude=EXCLUDED.longitude,
 kwaaka_restaurant_id=EXCLUDED.kwaaka_restaurant_id, is_active=EXCLUDED.is_active,
 is_new=EXCLUDED.is_new, is_popular=EXCLUDED.is_popular, is_premium=EXCLUDED.is_premium,
 hidden_from_home=EXCLUDED.hidden_from_home, display_order=EXCLUDED.display_order,
 updated_at=EXCLUDED.updated_at`,
		r.ID, r.Name, jsonb(r.NameI18n), r.Description, jsonb(r.DescriptionI18n),
		r.CuisineType, jsonb(r.CuisineTypeI18n), r.Address, jsonb(r.AddressI18n),
		r.OpeningHours, jsonb(r.OpeningHoursI18n), r.City, r.PriceCategory, r.Email,
		r.Phone, r.Latitude, r.Longitude, r.KwaakaID, r.IsActive, r.IsNew,
		r.IsPopular, r.IsPremium, r.HiddenFromHome, r.DisplayOrder, r.CreatedAt, r.UpdatedAt)
}

func (s *Sink) UpsertTable(ctx context.Context, t legacysync.Table) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, description, is_active, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (id) DO UPDATE SET
 restaurant_id=EXCLUDED.restaurant_id, name=EXCLUDED.name, capacity=EXCLUDED.capacity,
 description=EXCLUDED.description, is_active=EXCLUDED.is_active, updated_at=EXCLUDED.updated_at`,
		t.ID, t.RestaurantID, t.Name, t.Capacity, t.Description, t.IsActive, t.CreatedAt, t.UpdatedAt)
}

func (s *Sink) UpsertMenuCategory(ctx context.Context, c legacysync.MenuCategory) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO menu_categories (id, name, name_i18n, parent_id, display_order, created_at)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (id) DO UPDATE SET
 name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n, parent_id=EXCLUDED.parent_id,
 display_order=EXCLUDED.display_order`,
		c.ID, c.Name, jsonb(c.NameI18n), c.ParentID, c.DisplayOrder, c.CreatedAt)
}

func (s *Sink) UpsertMenuItem(ctx context.Context, m legacysync.MenuItem) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO menu_items
 (id, restaurant_id, name, name_i18n, description, description_i18n, price, image_url,
  is_available, category, category_i18n, subcategory, subcategory_i18n, portion_size,
  portion_size_i18n, language, display_order, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
ON CONFLICT (id) DO UPDATE SET
 restaurant_id=EXCLUDED.restaurant_id, name=EXCLUDED.name, name_i18n=EXCLUDED.name_i18n,
 description=EXCLUDED.description, description_i18n=EXCLUDED.description_i18n,
 price=EXCLUDED.price, image_url=EXCLUDED.image_url, is_available=EXCLUDED.is_available,
 category=EXCLUDED.category, category_i18n=EXCLUDED.category_i18n,
 subcategory=EXCLUDED.subcategory, subcategory_i18n=EXCLUDED.subcategory_i18n,
 portion_size=EXCLUDED.portion_size, portion_size_i18n=EXCLUDED.portion_size_i18n,
 language=EXCLUDED.language, display_order=EXCLUDED.display_order, updated_at=EXCLUDED.updated_at`,
		m.ID, m.RestaurantID, m.Name, jsonb(m.NameI18n), m.Description, jsonb(m.DescriptionI18n),
		m.Price, m.ImageURL, m.IsAvailable, m.Category, jsonb(m.CategoryI18n), m.Subcategory,
		jsonb(m.SubcategoryI18n), m.PortionSize, jsonb(m.PortionSizeI18n), m.Language,
		m.DisplayOrder, m.CreatedAt, m.UpdatedAt)
}

// UpsertBooking writes a booking. user_id is FK-guarded by a scalar subquery:
// it is kept only if that user already exists in the new users table (old guest
// users are not synced in increment 1), otherwise NULL — so a missing user never
// parks the booking. restaurant_id is a hard FK: a missing restaurant Parks the
// row for a later tick. forced_placement / confirmed_at / arrived_at /
// cancelled_by have no old equivalent and take their schema defaults.
func (s *Sink) UpsertBooking(ctx context.Context, b legacysync.Booking) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO bookings
 (id, restaurant_id, user_id, name, phone, email, phone_normalized, guests, starts_at,
  ends_at, status, source, notes, promotion_id, event_id, created_by_admin, cancelled_at,
  cancellation_reason_code, cancellation_reason, late_notification_sent, user_notified_late_at,
  user_late_message, reminder_60_sent_at, reminder_30_sent_at, original_booking_time_text,
  created_at, updated_at)
VALUES ($1,$2,(SELECT id FROM users WHERE id=$3),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
        $16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
ON CONFLICT (id) DO UPDATE SET
 restaurant_id=EXCLUDED.restaurant_id, user_id=EXCLUDED.user_id, name=EXCLUDED.name,
 phone=EXCLUDED.phone, email=EXCLUDED.email, phone_normalized=EXCLUDED.phone_normalized,
 guests=EXCLUDED.guests, starts_at=EXCLUDED.starts_at, ends_at=EXCLUDED.ends_at,
 status=EXCLUDED.status, source=EXCLUDED.source, notes=EXCLUDED.notes,
 promotion_id=EXCLUDED.promotion_id, event_id=EXCLUDED.event_id,
 created_by_admin=EXCLUDED.created_by_admin, cancelled_at=EXCLUDED.cancelled_at,
 cancellation_reason_code=EXCLUDED.cancellation_reason_code,
 cancellation_reason=EXCLUDED.cancellation_reason,
 late_notification_sent=EXCLUDED.late_notification_sent,
 user_notified_late_at=EXCLUDED.user_notified_late_at,
 user_late_message=EXCLUDED.user_late_message,
 reminder_60_sent_at=EXCLUDED.reminder_60_sent_at,
 reminder_30_sent_at=EXCLUDED.reminder_30_sent_at,
 original_booking_time_text=EXCLUDED.original_booking_time_text,
 updated_at=EXCLUDED.updated_at`,
		b.ID, b.RestaurantID, b.UserID, b.Name, b.Phone, b.Email, b.PhoneNormalized,
		b.Guests, b.StartsAt, b.EndsAt, b.Status, b.Source, b.Notes, b.PromotionID,
		b.EventID, b.CreatedByAdmin, b.CancelledAt, b.CancellationReasonCode,
		b.CancellationReason, b.LateNotificationSent, b.UserNotifiedLateAt,
		b.UserLateMessage, b.Reminder60SentAt, b.Reminder30SentAt, b.OriginalBookingTime,
		b.CreatedAt, b.UpdatedAt)
}

// UpsertBookingTable writes a table hold. A missing booking or table Parks it; an
// overlap with an existing active hold on the same table (the exclusion
// constraint) Skips it — the booking itself is unaffected, only the table chip.
func (s *Sink) UpsertBookingTable(ctx context.Context, bt legacysync.BookingTable) (legacysync.Outcome, error) {
	return exec(ctx, s.pool, `
INSERT INTO booking_tables (id, booking_id, table_id, slot, active, created_at)
VALUES ($1,$2,$3, tstzrange($4::timestamptz, $5::timestamptz, '[)'), $6, $7)
ON CONFLICT (id) DO UPDATE SET
 booking_id=EXCLUDED.booking_id, table_id=EXCLUDED.table_id, slot=EXCLUDED.slot,
 active=EXCLUDED.active`,
		bt.ID, bt.BookingID, bt.TableID, bt.SlotStart, bt.SlotEnd, bt.Active, bt.CreatedAt)
}

// jsonb hands a raw JSON byte slice to a jsonb column, or NULL when empty. pgx's
// jsonb codec accepts []byte directly, so no re-marshal is needed.
func jsonb(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

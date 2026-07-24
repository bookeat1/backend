// Package legacysource is the READ-ONLY adapter over the OLD BookEat database
// (the live Supabase Postgres). It implements usecase/legacysync.Source with
// plain SELECT statements only. As defence in depth every connection is put into
// default_transaction_read_only, so even a coding mistake here can never write
// to the old system — the deploy is expected to hand it a read-only role on top.
package legacysource

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/usecase/legacysync"
)

// OpenReadOnlyPool builds a pgx pool against the old database from a connection
// URL (env LEGACY_DB_URL). Every connection is forced read-only. The caller owns
// Close.
func OpenReadOnlyPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse LEGACY_DB_URL: %w", err)
	}
	// A read-only source never needs a large pool; keep the footprint on the
	// live old DB small.
	if cfg.MaxConns > 4 {
		cfg.MaxConns = 4
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET default_transaction_read_only = on")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open legacy db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping legacy db: %w", err)
	}
	return pool, nil
}

// Source reads changed rows from the old database.
type Source struct{ pool *pgxpool.Pool }

// NewSource builds the source over an already-open read-only pool.
func NewSource(pool *pgxpool.Pool) *Source { return &Source{pool: pool} }

var _ legacysync.Source = (*Source)(nil)

func (s *Source) Restaurants(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.Restaurant, error) {
	const q = `
SELECT id, name, name_i18n, description, description_i18n, cuisine_type, cuisine_type_i18n,
       address, address_i18n, opening_hours, opening_hours_i18n, city::text, price_category::text,
       email, phone, latitude::double precision, longitude::double precision, kwaaka_restaurant_id,
       is_active, is_new, is_popular, is_premium, hidden_from_home, display_order, created_at, updated_at
FROM restaurants
WHERE (updated_at, id) > ($1::timestamptz, $2::uuid)
ORDER BY updated_at, id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.Restaurant
	for rows.Next() {
		var r legacysync.Restaurant
		if err := rows.Scan(
			&r.ID, &r.Name, &r.NameI18n, &r.Description, &r.DescriptionI18n,
			&r.CuisineType, &r.CuisineTypeI18n, &r.Address, &r.AddressI18n,
			&r.OpeningHours, &r.OpeningHoursI18n, &r.City, &r.PriceCategory,
			&r.Email, &r.Phone, &r.Latitude, &r.Longitude, &r.KwaakaID,
			&r.IsActive, &r.IsNew, &r.IsPopular, &r.IsPremium, &r.HiddenFromHome,
			&r.DisplayOrder, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Source) Tables(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.Table, error) {
	const q = `
SELECT id, restaurant_id, name, capacity, description, is_active, created_at, updated_at
FROM restaurant_tables
WHERE (updated_at, id) > ($1::timestamptz, $2::uuid)
ORDER BY updated_at, id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.Table
	for rows.Next() {
		var t legacysync.Table
		if err := rows.Scan(&t.ID, &t.RestaurantID, &t.Name, &t.Capacity,
			&t.Description, &t.IsActive, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Source) MenuCategories(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.MenuCategory, error) {
	// The old menu_categories has no updated_at; created_at is the cursor.
	const q = `
SELECT id, name, name_i18n, parent_id, display_order, created_at, created_at
FROM menu_categories
WHERE (created_at, id) > ($1::timestamptz, $2::uuid)
ORDER BY created_at, id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.MenuCategory
	for rows.Next() {
		var c legacysync.MenuCategory
		if err := rows.Scan(&c.ID, &c.Name, &c.NameI18n, &c.ParentID,
			&c.DisplayOrder, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Source) MenuItems(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.MenuItem, error) {
	const q = `
SELECT id, restaurant_id, name, name_i18n, description, description_i18n, price,
       image_url, is_available, category, category_i18n, subcategory, subcategory_i18n,
       portion_size, portion_size_i18n, language, display_order, created_at, updated_at
FROM menu_items
WHERE (updated_at, id) > ($1::timestamptz, $2::uuid)
ORDER BY updated_at, id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.MenuItem
	for rows.Next() {
		var m legacysync.MenuItem
		if err := rows.Scan(&m.ID, &m.RestaurantID, &m.Name, &m.NameI18n,
			&m.Description, &m.DescriptionI18n, &m.Price, &m.ImageURL, &m.IsAvailable,
			&m.Category, &m.CategoryI18n, &m.Subcategory, &m.SubcategoryI18n,
			&m.PortionSize, &m.PortionSizeI18n, &m.Language, &m.DisplayOrder,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Source) Bookings(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.LegacyBooking, error) {
	const q = `
SELECT id, user_id, restaurant_id, name, phone, email, guests, booking_date,
       status::text, notes, original_booking_time_text, created_by_admin,
       COALESCE(late_notification_sent, false), user_notified_late_at, user_late_message,
       promotion_id, event_id, reminder_60_sent_at, reminder_30_sent_at,
       cancellation_reason_code, cancellation_reason, cancelled_at, created_at, updated_at
FROM bookings
WHERE (updated_at, id) > ($1::timestamptz, $2::uuid)
ORDER BY updated_at, id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.LegacyBooking
	for rows.Next() {
		var b legacysync.LegacyBooking
		if err := rows.Scan(&b.ID, &b.UserID, &b.RestaurantID, &b.Name, &b.Phone,
			&b.Email, &b.Guests, &b.BookingDate, &b.Status, &b.Notes,
			&b.OriginalBookingTime, &b.CreatedByAdmin, &b.LateNotificationSent,
			&b.UserNotifiedLateAt, &b.UserLateMessage, &b.PromotionID, &b.EventID,
			&b.Reminder60SentAt, &b.Reminder30SentAt, &b.CancellationReasonCode,
			&b.CancellationReason, &b.CancelledAt, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Source) BookingTables(ctx context.Context, cur legacysync.Cursor, limit int) ([]legacysync.LegacyBookingTable, error) {
	// Two old sources of table assignment: the (rarely used) booking_tables join
	// table, and the per-booking bookings.table_id single-table pointer for
	// bookings that have no join row.
	//
	// sort_id = bt.id for a join row, the booking id for a synthesized one. It is
	// unique per row across both arms and available here in SQL (unlike the
	// synthesized new-DB id), so the UNION is paginated with real keyset
	// pagination on (updated_at, sort_id) > ($1, $2). This is a genuine,
	// deterministic tie-break: rows sharing an updated_at can never straddle a
	// batch boundary and be lost, exactly like every other entity's `(updated_at,
	// id) > ...`. row_id is the join id (NULL for synthesized rows) — the worker
	// reuses it as the new-DB id or derives one.
	const q = `
SELECT sort_id, row_id, booking_id, table_id, restaurant_id, booking_date, status, created_at, updated_at
FROM (
    SELECT bt.id AS sort_id, bt.id AS row_id, bt.booking_id, bt.table_id,
           b.restaurant_id, b.booking_date, b.status::text AS status,
           bt.created_at, b.updated_at
    FROM booking_tables bt
    JOIN bookings b ON b.id = bt.booking_id
    UNION ALL
    SELECT b.id AS sort_id, NULL::uuid AS row_id, b.id AS booking_id, b.table_id,
           b.restaurant_id, b.booking_date, b.status::text,
           b.created_at, b.updated_at
    FROM bookings b
    WHERE b.table_id IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM booking_tables bt2 WHERE bt2.booking_id = b.id)
) u
WHERE (u.updated_at, u.sort_id) > ($1::timestamptz, $2::uuid)
ORDER BY u.updated_at, u.sort_id
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, cur.UpdatedAt, cur.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacysync.LegacyBookingTable
	for rows.Next() {
		var (
			bt    legacysync.LegacyBookingTable
			rowID *uuid.UUID
		)
		if err := rows.Scan(&bt.SortID, &rowID, &bt.BookingID, &bt.TableID,
			&bt.RestaurantID, &bt.BookingDate, &bt.Status, &bt.CreatedAt, &bt.UpdatedAt); err != nil {
			return nil, err
		}
		if rowID != nil {
			bt.ID = *rowID
		}
		out = append(out, bt)
	}
	return out, rows.Err()
}

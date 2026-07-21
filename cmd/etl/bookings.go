package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/bootstrap"
	"backend-core/internal/domain"
)

// runBookings migrates the booking core from raw_supabase into the clean
// schema (spec §8). Idempotent: every row is upserted by its original id, so a
// second run produces the same result.
//
// It is deliberately row-by-row rather than one big INSERT … SELECT: phone
// normalization, the numeric → minor-unit conversion and the legacy
// bookings.table_id expansion need Go, and a booking whose restaurant or table
// no longer exists must be skipped with a counter instead of aborting the run.
func runBookings(ctx context.Context, db *sql.DB, cfg bootstrap.BookingConfig, log *slog.Logger) error {
	c := &bookingCounters{}

	migrated, err := migrateBookings(ctx, db, cfg, log, c)
	if err != nil {
		return err
	}
	if err := migrateBookingTables(ctx, db, migrated, log, c); err != nil {
		return err
	}
	if err := migrateBookingItems(ctx, db, migrated, log, c); err != nil {
		return err
	}
	for _, step := range bookingBulkSteps() {
		n, err := execCounted(ctx, db, step)
		if err != nil {
			return err
		}
		c.bulk = append(c.bulk, bulkResult{name: step.name, migrated: n.migrated, skipped: n.skipped})
	}
	c.log(log)
	return nil
}

// migratedBooking is what later steps need from a booking that made it into the
// clean schema: its visit window, the venue's buffer, whether it still holds a
// table, and the legacy single-table link to expand.
type migratedBooking struct {
	id            uuid.UUID
	startsAt      time.Time
	endsAt        time.Time
	buffer        time.Duration
	active        bool
	legacyTableID *uuid.UUID
}

type bookingCounters struct {
	bookings          int
	noRestaurant      int
	noDate            int
	badStatus         int
	orphanUser        int
	unnormalizedPhone int

	links        int
	linksNoTable int
	linksOverlap int

	items        int
	itemsSkipped int

	bulk []bulkResult
}

type bulkResult struct {
	name     string
	migrated int
	skipped  int
}

func (c *bookingCounters) log(log *slog.Logger) {
	log.Info("bookings etl summary",
		slog.Int("bookings", c.bookings),
		slog.Int("skipped_missing_restaurant", c.noRestaurant),
		slog.Int("skipped_missing_date", c.noDate),
		slog.Int("skipped_unknown_status", c.badStatus),
		slog.Int("user_id_dropped_not_in_users", c.orphanUser),
		slog.Int("phone_not_normalizable", c.unnormalizedPhone),
	)
	log.Info("booking_tables summary",
		slog.Int("migrated", c.links),
		slog.Int("skipped_missing_table", c.linksNoTable),
		slog.Int("skipped_slot_overlap", c.linksOverlap),
	)
	log.Info("booking_items summary",
		slog.Int("migrated", c.items),
		slog.Int("skipped", c.itemsSkipped),
	)
	for _, b := range c.bulk {
		log.Info("etl step summary", slog.String("step", b.name),
			slog.Int("migrated", b.migrated), slog.Int("skipped", b.skipped))
	}
}

const rawBookingsQuery = `
	SELECT b.id::text,
	       b.restaurant_id::text,
	       (r.id IS NOT NULL)                AS restaurant_exists,
	       u.id::text                        AS user_id,
	       (b.user_id IS NOT NULL)           AS had_user,
	       COALESCE(b.name, '')              AS name,
	       COALESCE(b.phone, '')             AS phone,
	       COALESCE(b.email, '')             AS email,
	       COALESCE(b.guests, 1)             AS guests,
	       b.booking_date,
	       COALESCE(b.status::text, 'pending') AS status,
	       b.notes,
	       b.promotion_id::text,
	       b.event_id::text,
	       COALESCE(b.created_by_admin, false),
	       COALESCE(b.late_notification_sent, false),
	       b.user_notified_late_at,
	       b.user_late_message,
	       b.reminder_60_sent_at,
	       b.reminder_30_sent_at,
	       b.cancellation_reason_code,
	       b.cancellation_reason,
	       b.cancelled_at,
	       b.original_booking_time_text,
	       b.table_id::text,
	       COALESCE(b.created_at, now()),
	       COALESCE(b.updated_at, now()),
	       r.booking_duration_minutes,
	       r.booking_buffer_minutes
	FROM raw_supabase.bookings b
	LEFT JOIN restaurants r ON r.id = b.restaurant_id
	LEFT JOIN users u ON u.id = b.user_id
	ORDER BY b.created_at`

const upsertBooking = `
	INSERT INTO bookings (id, restaurant_id, user_id, name, phone, email, phone_normalized,
	  guests, starts_at, ends_at, status, source, notes, promotion_id, event_id,
	  created_by_admin, forced_placement, confirmed_at, arrived_at, cancelled_at, cancelled_by,
	  cancellation_reason_code, cancellation_reason, late_notification_sent,
	  user_notified_late_at, user_late_message, reminder_60_sent_at, reminder_30_sent_at,
	  original_booking_time_text, created_at, updated_at)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,false,NULL,NULL,$17,NULL,
	        $18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
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
	  updated_at=EXCLUDED.updated_at`

// migrateBookings upserts raw_supabase.bookings and returns the rows that made
// it through, keyed by id, for the dependent steps.
func migrateBookings(ctx context.Context, db *sql.DB, cfg bootstrap.BookingConfig, log *slog.Logger, c *bookingCounters) (map[uuid.UUID]migratedBooking, error) {
	rows, err := db.QueryContext(ctx, rawBookingsQuery)
	if err != nil {
		return nil, fmt.Errorf("read raw_supabase.bookings: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]migratedBooking)
	for rows.Next() {
		var (
			id, restaurantID, name, rawPhone, email, status  string
			restaurantExists, hadUser, createdByAdmin, late  bool
			guests                                           int
			userID, promotionID, eventID, tableID            sql.NullString
			notes, lateMsg, reasonCode, reason, origTime     sql.NullString
			bookingDate, notifiedLate, r60, r30, cancelledAt sql.NullTime
			createdAt, updatedAt                             time.Time
			durationMin, bufferMin                           sql.NullInt64
		)
		if err := rows.Scan(&id, &restaurantID, &restaurantExists, &userID, &hadUser, &name,
			&rawPhone, &email, &guests, &bookingDate, &status, &notes, &promotionID, &eventID,
			&createdByAdmin, &late, &notifiedLate, &lateMsg, &r60, &r30, &reasonCode, &reason,
			&cancelledAt, &origTime, &tableID, &createdAt, &updatedAt,
			&durationMin, &bufferMin); err != nil {
			return nil, fmt.Errorf("scan raw booking: %w", err)
		}

		bookingID, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("parse booking id %q: %w", id, err)
		}
		if !restaurantExists {
			c.noRestaurant++
			log.Warn("skip booking: restaurant not migrated",
				slog.String("booking_id", id), slog.String("restaurant_id", restaurantID))
			continue
		}
		if !bookingDate.Valid {
			c.noDate++
			log.Warn("skip booking: booking_date is NULL", slog.String("booking_id", id))
			continue
		}
		bookingStatus := domain.BookingStatus(strings.TrimSpace(status))
		if !bookingStatus.Valid() {
			c.badStatus++
			log.Warn("skip booking: unknown status",
				slog.String("booking_id", id), slog.String("status", status))
			continue
		}
		if hadUser && !userID.Valid {
			// The guest was never migrated (deleted auth user) — keep the
			// booking as a guest booking rather than losing it to the FK.
			c.orphanUser++
		}

		normalized := phone.Normalize(rawPhone)
		if normalized == "" {
			c.unnormalizedPhone++
			normalized = strings.TrimSpace(rawPhone)
		}
		duration := resolveDurationMinutes(durationMin, cfg.DefaultDuration)
		buffer := resolveBufferMinutes(bufferMin, cfg.DefaultBuffer)
		startsAt := bookingDate.Time
		endsAt := startsAt.Add(duration)

		if _, err := db.ExecContext(ctx, upsertBooking,
			bookingID, restaurantID, nullStr(userID), name, rawPhone,
			strings.ToLower(strings.TrimSpace(email)), normalized,
			max(guests, 1), startsAt, endsAt, string(bookingStatus),
			bookingSource(createdByAdmin), nullStr(notes), nullStr(promotionID), nullStr(eventID),
			createdByAdmin, nullTime(cancelledAt), nullStr(reasonCode), nullStr(reason),
			late, nullTime(notifiedLate), nullStr(lateMsg), nullTime(r60), nullTime(r30),
			nullStr(origTime), createdAt, updatedAt,
		); err != nil {
			return nil, fmt.Errorf("upsert booking %s: %w", id, err)
		}
		c.bookings++

		mb := migratedBooking{
			id: bookingID, startsAt: startsAt, endsAt: endsAt,
			buffer: buffer, active: bookingStatus.HoldsTable(),
		}
		if tableID.Valid {
			if tid, err := uuid.Parse(tableID.String); err == nil {
				mb.legacyTableID = &tid
			}
		}
		out[bookingID] = mb
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read raw_supabase.bookings: %w", err)
	}
	if c.bookings == 0 && c.noRestaurant == 0 {
		return nil, errors.New("no bookings found in raw_supabase — is the dump loaded?")
	}
	return out, nil
}

// migrateBookingTables writes booking ↔ table links from two sources: the
// raw_supabase.booking_tables rows and the legacy single bookings.table_id,
// which the spec folds into the same table so that joined tables need no
// special case. Slots include the venue's buffer on both sides.
func migrateBookingTables(ctx context.Context, db *sql.DB, migrated map[uuid.UUID]migratedBooking, log *slog.Logger, c *bookingCounters) error {
	tables, err := existingTableIDs(ctx, db)
	if err != nil {
		return err
	}

	seen := map[[2]uuid.UUID]bool{}
	insert := func(linkID, bookingID, tableID uuid.UUID, mb migratedBooking, origin string) error {
		if !tables[tableID] {
			c.linksNoTable++
			log.Warn("skip booking table link: table not migrated",
				slog.String("booking_id", bookingID.String()),
				slog.String("table_id", tableID.String()), slog.String("origin", origin))
			return nil
		}
		if seen[[2]uuid.UUID{bookingID, tableID}] {
			return nil
		}
		from, to := slotBounds(mb.startsAt, mb.endsAt, mb.buffer)
		const q = `
			INSERT INTO booking_tables (id, booking_id, table_id, slot, active, created_at)
			VALUES ($1,$2,$3, tstzrange($4,$5,'[)'), $6, now())
			ON CONFLICT (id) DO UPDATE SET
			  booking_id=EXCLUDED.booking_id, table_id=EXCLUDED.table_id,
			  slot=EXCLUDED.slot, active=EXCLUDED.active`
		if _, err := db.ExecContext(ctx, q, linkID, bookingID, tableID, from, to, mb.active); err != nil {
			// 23P01 exclusion_violation: legacy Supabase data has no overlap
			// guard, so two active bookings may already share a table and slot.
			// Keep the first, report the rest.
			if isExclusionViolation(err) {
				c.linksOverlap++
				log.Warn("skip booking table link: slot overlaps an existing active booking",
					slog.String("booking_id", bookingID.String()),
					slog.String("table_id", tableID.String()))
				return nil
			}
			return fmt.Errorf("upsert booking table link %s: %w", linkID, err)
		}
		seen[[2]uuid.UUID{bookingID, tableID}] = true
		c.links++
		return nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT id::text, booking_id::text, table_id::text FROM raw_supabase.booking_tables ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("read raw_supabase.booking_tables: %w", err)
	}
	type rawLink struct{ id, bookingID, tableID uuid.UUID }
	var raw []rawLink
	for rows.Next() {
		var idStr, bidStr, tidStr string
		if err := rows.Scan(&idStr, &bidStr, &tidStr); err != nil {
			rows.Close()
			return fmt.Errorf("scan raw booking table: %w", err)
		}
		id, err1 := uuid.Parse(idStr)
		bid, err2 := uuid.Parse(bidStr)
		tid, err3 := uuid.Parse(tidStr)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		raw = append(raw, rawLink{id: id, bookingID: bid, tableID: tid})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read raw_supabase.booking_tables: %w", err)
	}
	rows.Close()

	for _, l := range raw {
		mb, ok := migrated[l.bookingID]
		if !ok {
			c.linksNoTable++
			continue
		}
		if err := insert(l.id, l.bookingID, l.tableID, mb, "booking_tables"); err != nil {
			return err
		}
	}

	// Legacy single-table bookings, in a stable order so a re-run resolves
	// overlaps the same way.
	for _, mb := range sortedBookings(migrated) {
		if mb.legacyTableID == nil {
			continue
		}
		if err := insert(legacyLinkID(mb.id, *mb.legacyTableID), mb.id, *mb.legacyTableID, mb, "legacy table_id"); err != nil {
			return err
		}
	}
	return nil
}

// migrateBookingItems converts the pre-order lines, turning the numeric price
// into minor units (tiyn) with banker's rounding.
func migrateBookingItems(ctx context.Context, db *sql.DB, migrated map[uuid.UUID]migratedBooking, log *slog.Logger, c *bookingCounters) error {
	rows, err := db.QueryContext(ctx, `
		SELECT bi.id::text, bi.booking_id::text, mi.id::text,
		       COALESCE(bi.item_name, ''), bi.item_price::text,
		       COALESCE(bi.quantity, 1), COALESCE(bi.status, 'pending'),
		       bi.comment, COALESCE(bi.created_at, now())
		FROM raw_supabase.booking_items bi
		LEFT JOIN menu_items mi ON mi.id = bi.menu_item_id
		ORDER BY bi.created_at`)
	if err != nil {
		return fmt.Errorf("read raw_supabase.booking_items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			idStr, bidStr, name, status string
			menuItemID, price, comment  sql.NullString
			quantity                    int
			createdAt                   time.Time
		)
		if err := rows.Scan(&idStr, &bidStr, &menuItemID, &name, &price,
			&quantity, &status, &comment, &createdAt); err != nil {
			return fmt.Errorf("scan raw booking item: %w", err)
		}
		id, err1 := uuid.Parse(idStr)
		bid, err2 := uuid.Parse(bidStr)
		if err1 != nil || err2 != nil {
			c.itemsSkipped++
			continue
		}
		if _, ok := migrated[bid]; !ok {
			c.itemsSkipped++
			log.Warn("skip booking item: booking not migrated", slog.String("booking_id", bidStr))
			continue
		}
		minor, err := priceToMinor(price.String)
		if err != nil {
			c.itemsSkipped++
			log.Warn("skip booking item: unparseable price",
				slog.String("item_id", idStr), slog.String("price", price.String))
			continue
		}
		itemStatus := domain.BookingItemStatus(strings.TrimSpace(status))
		if !itemStatus.Valid() {
			itemStatus = domain.BookingItemPending
		}
		const q = `
			INSERT INTO booking_items (id, booking_id, menu_item_id, item_name, item_price_minor,
			  currency, quantity, status, comment, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,'KZT',$6,$7,$8,$9,$9)
			ON CONFLICT (id) DO UPDATE SET
			  menu_item_id=EXCLUDED.menu_item_id, item_name=EXCLUDED.item_name,
			  item_price_minor=EXCLUDED.item_price_minor, quantity=EXCLUDED.quantity,
			  status=EXCLUDED.status, comment=EXCLUDED.comment, updated_at=now()`
		if _, err := db.ExecContext(ctx, q, id, bid, nullStr(menuItemID), name, minor,
			max(quantity, 1), string(itemStatus), nullStr(comment), createdAt); err != nil {
			return fmt.Errorf("upsert booking item %s: %w", idStr, err)
		}
		c.items++
	}
	return rows.Err()
}

// countedStep is a bulk INSERT … SELECT plus the raw-row count it is compared
// against, so the summary can report how many rows were dropped by the joins.
type countedStep struct {
	name  string
	sql   string
	count string
}

// bookingBulkSteps are the tables that need no per-row Go logic. Rows whose FK
// target was not migrated are dropped by the JOINs and end up in the skipped
// count.
func bookingBulkSteps() []countedStep {
	return []countedStep{
		{
			name:  "booking_messages",
			count: `SELECT count(*) FROM raw_supabase.booking_messages`,
			sql: `
			INSERT INTO booking_messages (id, booking_id, sender_type, sender_id, message, is_read, read_at, created_at)
			SELECT m.id, m.booking_id,
			       CASE lower(COALESCE(m.sender_type, ''))
			           WHEN 'user' THEN 'guest'
			           WHEN 'guest' THEN 'guest'
			           WHEN 'restaurant' THEN 'restaurant'
			           WHEN 'manager' THEN 'restaurant'
			           ELSE 'system'
			       END,
			       u.id, COALESCE(m.message, ''), COALESCE(m.is_read, false), NULL,
			       COALESCE(m.created_at, now())
			FROM raw_supabase.booking_messages m
			JOIN bookings b ON b.id = m.booking_id
			LEFT JOIN users u ON u.id = m.sender_id
			ON CONFLICT (id) DO UPDATE SET
			  sender_type=EXCLUDED.sender_type, sender_id=EXCLUDED.sender_id,
			  message=EXCLUDED.message, is_read=EXCLUDED.is_read`,
		},
		{
			name:  "booking_blacklist",
			count: `SELECT count(*) FROM raw_supabase.booking_blacklist`,
			// restaurant_id stays NULL: the Supabase list was global only.
			// DISTINCT ON keeps one active row per phone, which is what the
			// partial unique index allows.
			sql: `
			INSERT INTO booking_blacklist (id, restaurant_id, user_id, phone_normalized, email,
			  reason, created_by, is_active, created_at, updated_at)
			SELECT DISTINCT ON (COALESCE(bl.phone_normalized, bl.id::text))
			       bl.id, NULL, u.id, bl.phone_normalized, lower(bl.email), bl.reason, cb.id,
			       COALESCE(bl.is_active, true), COALESCE(bl.created_at, now()), COALESCE(bl.created_at, now())
			FROM raw_supabase.booking_blacklist bl
			LEFT JOIN users u ON u.id = bl.user_id
			LEFT JOIN users cb ON cb.id = bl.created_by
			WHERE bl.user_id IS NOT NULL OR bl.phone_normalized IS NOT NULL OR bl.email IS NOT NULL
			ORDER BY COALESCE(bl.phone_normalized, bl.id::text), bl.created_at DESC
			ON CONFLICT (id) DO UPDATE SET
			  user_id=EXCLUDED.user_id, phone_normalized=EXCLUDED.phone_normalized,
			  email=EXCLUDED.email, reason=EXCLUDED.reason, is_active=EXCLUDED.is_active,
			  updated_at=now()`,
		},
		{
			name:  "booking_rate_log",
			count: `SELECT count(*) FROM raw_supabase.booking_rate_log`,
			sql: `
			INSERT INTO booking_rate_log (id, user_id, phone_normalized, email, restaurant_id, action, created_at)
			SELECT rl.id, u.id, rl.phone_normalized, lower(rl.email), NULL, 'create',
			       COALESCE(rl.created_at, now())
			FROM raw_supabase.booking_rate_log rl
			LEFT JOIN users u ON u.id = rl.user_id
			ON CONFLICT (id) DO UPDATE SET
			  user_id=EXCLUDED.user_id, phone_normalized=EXCLUDED.phone_normalized,
			  email=EXCLUDED.email`,
		},
		{
			name:  "restaurant_surveys",
			count: `SELECT count(*) FROM raw_supabase.restaurant_surveys`,
			// The clean schema requires a user, a restaurant and in-range
			// ratings; rows failing that are dropped (they cannot be repaired
			// without inventing data). booking_id is unique, hence DISTINCT ON.
			sql: `
			INSERT INTO restaurant_surveys (id, booking_id, restaurant_id, user_id, rating_overall,
			  rating_food, rating_service, rating_ambience, nps, comment, dismissed, created_at)
			SELECT DISTINCT ON (COALESCE(s.booking_id::text, s.id::text))
			       s.id, b.id, r.id, u.id, s.rating_overall, s.rating_food, s.rating_service,
			       s.rating_ambience, s.nps, s.comment, COALESCE(s.dismissed, false),
			       COALESCE(s.created_at, now())
			FROM raw_supabase.restaurant_surveys s
			JOIN restaurants r ON r.id = s.restaurant_id
			JOIN users u ON u.id = s.user_id
			LEFT JOIN bookings b ON b.id = s.booking_id
			WHERE s.rating_overall BETWEEN 1 AND 5
			  AND s.rating_food BETWEEN 1 AND 5
			  AND s.rating_service BETWEEN 1 AND 5
			  AND s.rating_ambience BETWEEN 1 AND 5
			  AND s.nps BETWEEN 0 AND 10
			ORDER BY COALESCE(s.booking_id::text, s.id::text), s.created_at DESC
			ON CONFLICT (id) DO UPDATE SET
			  rating_overall=EXCLUDED.rating_overall, rating_food=EXCLUDED.rating_food,
			  rating_service=EXCLUDED.rating_service, rating_ambience=EXCLUDED.rating_ambience,
			  nps=EXCLUDED.nps, comment=EXCLUDED.comment, dismissed=EXCLUDED.dismissed`,
		},
	}
}

// execCounted runs one bulk step and reports migrated/skipped against the raw
// row count.
func execCounted(ctx context.Context, db *sql.DB, s countedStep) (bulkResult, error) {
	var total int
	if err := db.QueryRowContext(ctx, s.count).Scan(&total); err != nil {
		return bulkResult{}, fmt.Errorf("count %s: %w", s.name, err)
	}
	res, err := db.ExecContext(ctx, s.sql)
	if err != nil {
		return bulkResult{}, fmt.Errorf("etl step %s: %w", s.name, err)
	}
	n, _ := res.RowsAffected()
	return bulkResult{name: s.name, migrated: int(n), skipped: total - int(n)}, nil
}

func existingTableIDs(ctx context.Context, db *sql.DB) (map[uuid.UUID]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id::text FROM restaurant_tables`)
	if err != nil {
		return nil, fmt.Errorf("read restaurant_tables: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if id, err := uuid.Parse(s); err == nil {
			out[id] = true
		}
	}
	return out, rows.Err()
}

func nullTime(t sql.NullTime) any {
	if t.Valid {
		return t.Time
	}
	return nil
}

func isExclusionViolation(err error) bool {
	// pgx surfaces the SQLSTATE in the message; matching on the code avoids a
	// pgconn import in this one-off tool.
	return err != nil && strings.Contains(err.Error(), "23P01")
}

// --- pure mapping helpers (unit-tested in bookings_test.go) ---

// resolveDurationMinutes applies the spec §8 rule for ends_at: the venue's
// booking_duration_minutes when set and sane, otherwise the env default.
func resolveDurationMinutes(override sql.NullInt64, def time.Duration) time.Duration {
	if override.Valid && override.Int64 > 0 {
		return time.Duration(override.Int64) * time.Minute
	}
	if def <= 0 {
		return 90 * time.Minute
	}
	return def
}

// resolveBufferMinutes is the same for the cleanup buffer, where 0 is a
// meaningful override.
func resolveBufferMinutes(override sql.NullInt64, def time.Duration) time.Duration {
	if override.Valid && override.Int64 >= 0 {
		return time.Duration(override.Int64) * time.Minute
	}
	if def < 0 {
		return 0
	}
	return def
}

// slotBounds widens the visit window by the venue's buffer on both sides — the
// stored slot is the interval during which the table is unavailable.
func slotBounds(startsAt, endsAt time.Time, buffer time.Duration) (time.Time, time.Time) {
	return startsAt.Add(-buffer), endsAt.Add(buffer)
}

// legacyLinkID derives a stable id for a booking_tables row synthesised from
// the legacy bookings.table_id, so re-running the ETL upserts the same row
// instead of inserting a duplicate.
func legacyLinkID(bookingID, tableID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("booking_tables:"+bookingID.String()+":"+tableID.String()))
}

// bookingSource maps the legacy created_by_admin flag onto the new source
// column. Supabase had no other origin information.
func bookingSource(createdByAdmin bool) string {
	if createdByAdmin {
		return string(domain.SourceAdmin)
	}
	return string(domain.SourceApp)
}

// priceToMinor converts a numeric price rendered as a decimal string into minor
// units (tiyn), using banker's rounding (half to even) as required by spec §8.
// It works on the decimal digits directly — going through float64 would make
// the rounding of exact halves unreproducible.
func priceToMinor(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg, s = true, s[1:]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if !isDigits(intPart) || !isDigits(fracPart) {
		return 0, fmt.Errorf("invalid numeric %q", raw)
	}
	// Two kept decimal places plus the remainder that decides the rounding.
	kept := fracPart
	rest := ""
	if len(kept) > 2 {
		kept, rest = fracPart[:2], fracPart[2:]
	}
	kept += strings.Repeat("0", 2-len(kept))

	minor, err := strconvAtoi64(intPart + kept)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric %q: %w", raw, err)
	}
	minor += roundIncrement(rest, minor)
	if neg {
		minor = -minor
	}
	return minor, nil
}

// roundIncrement decides whether the dropped digits round the value up, using
// half-to-even on an exact half.
func roundIncrement(rest string, minor int64) int64 {
	if rest == "" {
		return 0
	}
	if rest[0] > '5' {
		return 1
	}
	if rest[0] < '5' {
		return 0
	}
	if strings.Trim(rest[1:], "0") != "" {
		return 1 // more than a half
	}
	if minor%2 == 0 {
		return 0 // exact half, already even
	}
	return 1
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func strconvAtoi64(s string) (int64, error) {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// sortedBookings gives the migrated bookings a deterministic order (by start
// time, then id) so that a re-run resolves slot overlaps identically.
func sortedBookings(m map[uuid.UUID]migratedBooking) []migratedBooking {
	out := make([]migratedBooking, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].startsAt.Equal(out[j].startsAt) {
			return out[i].startsAt.Before(out[j].startsAt)
		}
		return out[i].id.String() < out[j].id.String()
	})
	return out
}

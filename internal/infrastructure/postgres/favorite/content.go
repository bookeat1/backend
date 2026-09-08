package favorite

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/event"
	"backend-core/internal/infrastructure/postgres/promo"
	"backend-core/internal/infrastructure/sqltx"
)

// AddEvent stores the bookmark against the event's SERIES when the event is a
// generated occurrence (events.recurrence_id), and against the event itself
// otherwise — see migration 0076 for why a recurring event is saved as a series.
//
// One statement, no read-then-write: the CTE resolves the target and the INSERT
// selects from it, so nothing can slip between the two. ON CONFLICT DO NOTHING
// is untargeted on purpose — it has to absorb BOTH unique indexes (the same
// event twice, and a second occurrence of an already-saved series). The
// returned count of the resolving CTE is the only thing that distinguishes
// "event does not exist" (0) from "already favorited" (1, zero rows inserted) —
// rows affected cannot, both are 0. The `ins` CTE is not referenced by the
// final SELECT and does not need to be: Postgres runs a data-modifying WITH
// exactly once and to completion whether or not its output is read.
func (r *Repository) AddEvent(ctx context.Context, userID, eventID uuid.UUID) error {
	var found int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`WITH tgt AS (SELECT id, recurrence_id FROM events WHERE id = $2),
		      ins AS (
		          INSERT INTO event_favorites (user_id, event_id, recurrence_id)
		          SELECT $1, CASE WHEN t.recurrence_id IS NULL THEN t.id END, t.recurrence_id
		          FROM tgt t
		          ON CONFLICT DO NOTHING
		          RETURNING 1)
		 SELECT count(*) FROM tgt`,
		userID, eventID).Scan(&found)
	if err != nil {
		return fmt.Errorf("add event favorite: %w", err)
	}
	if found == 0 {
		return fmt.Errorf("add event favorite: %w", domain.ErrNotFound)
	}
	return nil
}

// RemoveEvent drops the bookmark eventID stands for: the series row when
// eventID is a generated occurrence, the event row otherwise. The guest taps a
// filled heart on whichever occurrence the Афиша is currently showing, which is
// not necessarily the one they saved — matching on the series is what makes
// that un-save work at all.
//
// Zero rows deleted (not saved, or an id that never existed) is not an error.
func (r *Repository) RemoveEvent(ctx context.Context, userID, eventID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`WITH tgt AS (SELECT id, recurrence_id FROM events WHERE id = $2)
		 DELETE FROM event_favorites f
		 USING tgt t
		 WHERE f.user_id = $1
		   AND (f.event_id = t.id OR f.recurrence_id = t.recurrence_id)`,
		userID, eventID)
	if err != nil {
		return fmt.Errorf("remove event favorite: %w", err)
	}
	return nil
}

// ListEventsByUser resolves every bookmark to the card the guest can open now.
//
// The LATERAL is the whole design: for a series bookmark it picks the NEAREST
// UPCOMING occurrence, for a one-off bookmark it can only pick that one event.
// Visibility (published, not ended, active venue) lives INSIDE the lateral for
// the series case — filtering it outside would let the lateral choose a past or
// unpublished occurrence and then drop the whole series from the screen,
// exactly the bug this feature exists to avoid.
//
// The column list and the scan come from the event package so a favorited event
// serializes identically to one from the public Афиша.
func (r *Repository) ListEventsByUser(ctx context.Context, userID uuid.UUID, now time.Time) ([]domain.FavoriteEventItem, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+event.ListColumns+`, f.created_at, f.recurrence_id
		 FROM event_favorites f
		 JOIN LATERAL (
		     SELECT ev.* FROM events ev
		     WHERE ((f.event_id IS NOT NULL AND ev.id = f.event_id)
		         OR (f.recurrence_id IS NOT NULL AND ev.recurrence_id = f.recurrence_id))
		       AND ev.status = 'published'
		       AND ev.ends_at > $2
		     ORDER BY ev.starts_at ASC, ev.id ASC
		     LIMIT 1
		 ) e ON true
		 -- LEFT JOIN since migration 0085: a PLATFORM event has no venue, and
		 -- an inner join would drop it from the guest's own saved list — the
		 -- one screen where the guest already decided they want it. The active
		 -- check moves into COALESCE for the same reason: "no venue" is not
		 -- "an inactive venue".
		 LEFT JOIN restaurants r ON r.id = e.restaurant_id
		 WHERE f.user_id = $1
		   AND COALESCE(r.is_active, true) = true
		 ORDER BY f.created_at DESC`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list event favorites: %w", err)
	}
	defer rows.Close()

	var items []domain.FavoriteEventItem
	for rows.Next() {
		var favoritedAt time.Time
		var seriesID *uuid.UUID
		li, err := event.ScanListItem(trailingScanner{row: rows, extra: []any{&favoritedAt, &seriesID}})
		if err != nil {
			return nil, fmt.Errorf("list event favorites: %w", err)
		}
		items = append(items, domain.FavoriteEventItem{
			EventListItem: *li,
			SeriesID:      seriesID,
			FavoritedAt:   favoritedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list event favorites: %w", err)
	}
	return items, nil
}

// AddPromo bookmarks a promo. Same single-statement, DB-enforced-idempotency
// shape as AddEvent, minus the series resolution a promo has no concept of.
func (r *Repository) AddPromo(ctx context.Context, userID, promoID uuid.UUID) error {
	var found int
	err := sqltx.From(ctx, r.pool).QueryRow(ctx,
		`WITH tgt AS (SELECT id FROM promos WHERE id = $2),
		      ins AS (
		          INSERT INTO promo_favorites (user_id, promo_id)
		          SELECT $1, t.id FROM tgt t
		          ON CONFLICT (user_id, promo_id) DO NOTHING
		          RETURNING 1)
		 SELECT count(*) FROM tgt`,
		userID, promoID).Scan(&found)
	if err != nil {
		return fmt.Errorf("add promo favorite: %w", err)
	}
	if found == 0 {
		return fmt.Errorf("add promo favorite: %w", domain.ErrNotFound)
	}
	return nil
}

// RemovePromo is a plain DELETE: zero rows affected is not an error.
func (r *Repository) RemovePromo(ctx context.Context, userID, promoID uuid.UUID) error {
	_, err := sqltx.From(ctx, r.pool).Exec(ctx,
		`DELETE FROM promo_favorites WHERE user_id = $1 AND promo_id = $2`, userID, promoID)
	if err != nil {
		return fmt.Errorf("remove promo favorite: %w", err)
	}
	return nil
}

// ListPromosByUser applies the public listing's exact visibility rule
// (published, starts_at <= now < ends_at, active venue — see
// promo.Repository.ListActive) so the favorites screen can never show an offer
// the rest of the app considers over.
func (r *Repository) ListPromosByUser(ctx context.Context, userID uuid.UUID, now time.Time) ([]domain.FavoritePromoItem, error) {
	rows, err := sqltx.From(ctx, r.pool).Query(ctx,
		`SELECT `+promo.ListColumns+`, f.created_at
		 FROM promo_favorites f
		 JOIN promos p ON p.id = f.promo_id
		 -- LEFT JOIN: see ListEventsByUser above — a platform promo has no
		 -- venue and must not fall out of the guest's saved list.
		 LEFT JOIN restaurants r ON r.id = p.restaurant_id
		 WHERE f.user_id = $1
		   AND COALESCE(r.is_active, true) = true
		   AND p.status = 'published'
		   AND p.starts_at <= $2 AND p.ends_at > $2
		 ORDER BY f.created_at DESC`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list promo favorites: %w", err)
	}
	defer rows.Close()

	var items []domain.FavoritePromoItem
	for rows.Next() {
		var favoritedAt time.Time
		li, err := promo.ScanListItem(trailingScanner{row: rows, extra: []any{&favoritedAt}})
		if err != nil {
			return nil, fmt.Errorf("list promo favorites: %w", err)
		}
		items = append(items, domain.FavoritePromoItem{PromoListItem: *li, FavoritedAt: favoritedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list promo favorites: %w", err)
	}
	return items, nil
}

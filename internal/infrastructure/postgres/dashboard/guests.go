package dashboard

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

// PLATFORM GUEST LIST.
//
// Одна строка = один человек, склеенный по нормализованному телефону. Источников
// два и они равноправны, поэтому FULL OUTER JOIN, а не JOIN от users:
//   - bookings дают тех, кто бронировал (в том числе БЕЗ регистрации),
//   - users дают тех, кто зарегистрировался (в том числе НИ РАЗУ не бронировав).
// Внутренний JOIN потерял бы ровно те две группы, ради которых список и нужен.
//
// Телефоны в обеих таблицах уже в E.164 (проверено на живой базе: users.phone
// и bookings.phone_normalized начинаются с «+7…»), поэтому соединяем как есть,
// без нормализации в SQL — она уже сделана на записи (internal/auth/phone).
// Брони с ПУСТЫМ phone_normalized (в тестовой базе такие есть, наследство
// импорта) в агрегат не попадают: у строки без телефона нет идентичности, и
// склеить её было бы не с чем — она стала бы фантомным «гостем без имени».
//
// Персонал (owner/manager/hostess/admin) отфильтрован: это список ГОСТЕЙ, а
// сотрудник в рассылке акций — не гость.

// guestAggregate — общая часть запроса: агрегат по броням и подклеенные к нему
// аккаунты. Возвращает SQL и параметры; вызывающий добавляет свои SELECT/ORDER.
func guestAggregate(q domain.PlatformGuestQuery) (string, []any) {
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	sql := `WITH b AS (
			SELECT phone_normalized AS phone,
				count(*)                                                   AS bookings_count,
				count(*) FILTER (WHERE status IN ('arrived','completed'))  AS visits_count,
				count(*) FILTER (WHERE status = 'cancelled')               AS cancelled_count,
				count(*) FILTER (WHERE status = 'no_show')                 AS no_show_count,
				count(DISTINCT restaurant_id)                              AS venues_count,
				min(created_at)                                            AS first_booking_at,
				max(created_at)                                            AS last_booking_at,
				(array_agg(name  ORDER BY created_at DESC))[1]             AS last_name,
				(array_agg(email ORDER BY created_at DESC))[1]             AS last_email
			FROM bookings
			WHERE phone_normalized <> ''
			GROUP BY phone_normalized
		),
		u AS (
			SELECT id, phone, full_name, email, city, preferred_language, created_at
			FROM users
			WHERE role = 'user' AND phone IS NOT NULL AND phone <> ''
		),
		guests AS (
			SELECT
				COALESCE(u.phone, b.phone)                    AS phone,
				u.id                                          AS user_id,
				COALESCE(NULLIF(u.full_name, ''), b.last_name, '') AS name,
				COALESCE(NULLIF(u.email, ''), b.last_email, '')    AS email,
				COALESCE(u.city, '')                          AS city,
				COALESCE(u.preferred_language, '')            AS language,
				u.created_at                                  AS registered_at,
				COALESCE(b.bookings_count, 0)                 AS bookings_count,
				COALESCE(b.visits_count, 0)                   AS visits_count,
				COALESCE(b.cancelled_count, 0)                AS cancelled_count,
				COALESCE(b.no_show_count, 0)                  AS no_show_count,
				COALESCE(b.venues_count, 0)                   AS venues_count,
				b.first_booking_at,
				b.last_booking_at
			FROM b FULL OUTER JOIN u ON u.phone = b.phone
		)
		SELECT %s FROM guests WHERE true`

	where := &strings.Builder{}
	switch q.Segment {
	case domain.GuestSegmentRegistered:
		where.WriteString(" AND user_id IS NOT NULL")
	case domain.GuestSegmentBooked:
		where.WriteString(" AND bookings_count > 0")
	case domain.GuestSegmentVisited:
		where.WriteString(" AND visits_count > 0")
	case domain.GuestSegmentNeverVisited:
		where.WriteString(" AND bookings_count > 0 AND visits_count = 0")
	case domain.GuestSegmentNoBookings:
		where.WriteString(" AND user_id IS NOT NULL AND bookings_count = 0")
	case domain.GuestSegmentCancelled:
		where.WriteString(" AND cancelled_count > 0")
	}

	if s := strings.TrimSpace(q.Search); s != "" {
		// ILIKE по трём полям: человека ищут и по имени, и по номеру, и по почте,
		// и заранее неизвестно, что из этого помнит тот, кто ищет.
		p := arg("%" + s + "%")
		fmt.Fprintf(where, " AND (name ILIKE %s OR phone ILIKE %s OR email ILIKE %s)", p, p, p)
	}
	if c := strings.TrimSpace(q.City); c != "" {
		fmt.Fprintf(where, " AND city = %s", arg(c))
	}
	if l := strings.TrimSpace(q.Language); l != "" {
		fmt.Fprintf(where, " AND language = %s", arg(l))
	}
	if q.RegisteredFrom != nil {
		fmt.Fprintf(where, " AND registered_at >= %s", arg(*q.RegisteredFrom))
	}
	if q.RegisteredTo != nil {
		fmt.Fprintf(where, " AND registered_at <= %s", arg(*q.RegisteredTo))
	}
	if q.BookedFrom != nil {
		fmt.Fprintf(where, " AND last_booking_at >= %s", arg(*q.BookedFrom))
	}
	if q.BookedTo != nil {
		fmt.Fprintf(where, " AND last_booking_at <= %s", arg(*q.BookedTo))
	}
	if q.MinBookings > 0 {
		fmt.Fprintf(where, " AND bookings_count >= %s", arg(q.MinBookings))
	}

	return sql + where.String(), args
}

// orderBy переводит сорт домена в SQL. NULLS LAST везде: гость без броней (или
// без аккаунта) не должен занимать верх списка только потому, что у него пусто.
func orderBy(s domain.PlatformGuestSort) string {
	switch s {
	case domain.GuestSortBookings:
		return " ORDER BY bookings_count DESC, last_booking_at DESC NULLS LAST, phone"
	case domain.GuestSortRegistered:
		return " ORDER BY registered_at DESC NULLS LAST, phone"
	default:
		return " ORDER BY last_booking_at DESC NULLS LAST, registered_at DESC NULLS LAST, phone"
	}
}

// Guests возвращает страницу списка и ОБЩЕЕ число подходящих строк (для
// пагинации в панели). Считаем двумя запросами по одному и тому же WHERE —
// как в остальных списках этого проекта.
func (r *Repository) Guests(ctx context.Context, q domain.PlatformGuestQuery) ([]domain.PlatformGuest, int, error) {
	base, args := guestAggregate(q)
	db := sqltx.From(ctx, r.pool)

	var total int
	if err := db.QueryRow(ctx, fmt.Sprintf(base, "count(*)"), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count platform guests: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	page, perPage := q.Page, q.PerPage
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 50
	}
	listSQL := fmt.Sprintf(base,
		`phone, user_id, name, email, city, language, registered_at,
		 bookings_count, visits_count, cancelled_count, no_show_count, venues_count,
		 first_booking_at, last_booking_at`) +
		orderBy(q.Sort) +
		fmt.Sprintf(" LIMIT %d OFFSET %d", perPage, (page-1)*perPage)

	rows, err := db.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list platform guests: %w", err)
	}
	defer rows.Close()

	var out []domain.PlatformGuest
	for rows.Next() {
		var g domain.PlatformGuest
		if err := rows.Scan(&g.Phone, &g.UserID, &g.Name, &g.Email, &g.City, &g.Language,
			&g.RegisteredAt, &g.BookingsCount, &g.VisitsCount, &g.CancelledCount,
			&g.NoShowCount, &g.VenuesCount, &g.FirstBookingAt, &g.LastBookingAt); err != nil {
			return nil, 0, fmt.Errorf("scan platform guest: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate platform guests: %w", err)
	}
	return out, total, nil
}

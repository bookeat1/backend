package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/testdb"
)

// Список гостей склеивает ДВА источника — брони и аккаунты — по номеру
// телефона. Всё, что здесь проверяется, — это границы этой склейки: человек без
// аккаунта, аккаунт без броней, один человек с бронями в разных заведениях.
// Проверяется на настоящем Postgres, потому что вся логика живёт в SQL.

func seedUser(t *testing.T, pool *pgxpool.Pool, phone, name, city, lang string, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, phone, full_name, role, city, preferred_language, created_at)
		 VALUES ($1,$2,$3,'user',$4,$5,$6)`,
		id, phone, name, city, lang, createdAt); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func seedStaff(t *testing.T, pool *pgxpool.Pool, phone, role string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, phone, full_name, role, preferred_language)
		 VALUES ($1,$2,'Сотрудник',$3,'ru')`, id, phone, role); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	return id
}

func seedGuestBooking(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID, phone, name, status string, createdAt time.Time) {
	t.Helper()
	start := createdAt.Add(24 * time.Hour)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status, created_at)
		 VALUES ($1,$2,$3,$4,$4,2,$5::timestamptz,$5::timestamptz + interval '2 hours',$6,$7)`,
		uuid.New(), rid, name, phone, start, status, createdAt); err != nil {
		t.Fatalf("seed guest booking: %v", err)
	}
}

func guestSetup(t *testing.T) (*pgxpool.Pool, *Repository, context.Context) {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, tables...)
	return pool, New(pool), context.Background()
}

func byPhone(list []domain.PlatformGuest, phone string) *domain.PlatformGuest {
	for i := range list {
		if list[i].Phone == phone {
			return &list[i]
		}
	}
	return nil
}

func TestGuests_JoinsAccountsAndBookingsByPhone(t *testing.T) {
	pool, repo, ctx := guestSetup(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	rid := uuid.New()
	seedRestaurant(t, pool, rid, "Mongol", true)

	// Зарегистрирован и дошёл.
	uid := seedUser(t, pool, "+77010000001", "Дамир", "Алматы", "ru", base)
	seedGuestBooking(t, pool, rid, "+77010000001", "Дамир", "completed", base.Add(time.Hour))
	// Бронировал без регистрации.
	seedGuestBooking(t, pool, rid, "+77010000002", "Гость с улицы", "confirmed", base.Add(2*time.Hour))
	// Зарегистрировался и ни разу не бронировал.
	seedUser(t, pool, "+77010000003", "Молчун", "Астана", "kk", base)

	list, total, err := repo.Guests(ctx, domain.PlatformGuestQuery{})
	if err != nil {
		t.Fatalf("guests: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("total = %d, rows = %d, want 3 и 3", total, len(list))
	}

	withAccount := byPhone(list, "+77010000001")
	if withAccount == nil || withAccount.UserID == nil || *withAccount.UserID != uid {
		t.Fatalf("аккаунт не подклеился к броням: %+v", withAccount)
	}
	if withAccount.BookingsCount != 1 || withAccount.VisitsCount != 1 {
		t.Fatalf("счётчики = %d броней / %d визитов, want 1/1", withAccount.BookingsCount, withAccount.VisitsCount)
	}
	if withAccount.City != "Алматы" || withAccount.Language != "ru" {
		t.Fatalf("город/язык берутся из аккаунта, получено %q/%q", withAccount.City, withAccount.Language)
	}

	anon := byPhone(list, "+77010000002")
	if anon == nil || anon.UserID != nil {
		t.Fatalf("гость без регистрации должен быть в списке без аккаунта: %+v", anon)
	}
	if anon.Name != "Гость с улицы" {
		t.Fatalf("имя без аккаунта берётся с брони, получено %q", anon.Name)
	}
	if anon.RegisteredAt != nil {
		t.Fatalf("у гостя без аккаунта не может быть даты регистрации")
	}

	silent := byPhone(list, "+77010000003")
	if silent == nil || silent.BookingsCount != 0 || silent.LastBookingAt != nil {
		t.Fatalf("зарегистрированный без броней должен быть с нулями: %+v", silent)
	}
}

func TestGuests_SegmentsSplitVisitedFromRegistered(t *testing.T) {
	pool, repo, ctx := guestSetup(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rid := uuid.New()
	seedRestaurant(t, pool, rid, "Mongol", true)

	seedUser(t, pool, "+77020000001", "Дошёл", "Алматы", "ru", base)
	seedGuestBooking(t, pool, rid, "+77020000001", "Дошёл", "completed", base)

	seedUser(t, pool, "+77020000002", "Не дошёл", "Алматы", "ru", base)
	seedGuestBooking(t, pool, rid, "+77020000002", "Не дошёл", "cancelled", base)

	seedUser(t, pool, "+77020000003", "Только регистрация", "Алматы", "ru", base)

	seedGuestBooking(t, pool, rid, "+77020000004", "Аноним", "no_show", base)

	cases := []struct {
		segment domain.PlatformGuestSegment
		want    []string
	}{
		{domain.GuestSegmentAll, []string{"+77020000001", "+77020000002", "+77020000003", "+77020000004"}},
		{domain.GuestSegmentRegistered, []string{"+77020000001", "+77020000002", "+77020000003"}},
		{domain.GuestSegmentBooked, []string{"+77020000001", "+77020000002", "+77020000004"}},
		{domain.GuestSegmentVisited, []string{"+77020000001"}},
		{domain.GuestSegmentNeverVisited, []string{"+77020000002", "+77020000004"}},
		{domain.GuestSegmentNoBookings, []string{"+77020000003"}},
		{domain.GuestSegmentCancelled, []string{"+77020000002"}},
	}
	for _, tc := range cases {
		list, total, err := repo.Guests(ctx, domain.PlatformGuestQuery{Segment: tc.segment})
		if err != nil {
			t.Fatalf("сегмент %s: %v", tc.segment, err)
		}
		if total != len(tc.want) {
			t.Fatalf("сегмент %s: total = %d, want %d", tc.segment, total, len(tc.want))
		}
		for _, phone := range tc.want {
			if byPhone(list, phone) == nil {
				t.Fatalf("сегмент %s: не хватает %s (получено %d строк)", tc.segment, phone, len(list))
			}
		}
	}
}

func TestGuests_CountsVenuesAndKeepsStaffOut(t *testing.T) {
	pool, repo, ctx := guestSetup(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	first, second := uuid.New(), uuid.New()
	seedRestaurant(t, pool, first, "Первое", true)
	seedRestaurant(t, pool, second, "Второе", true)

	seedGuestBooking(t, pool, first, "+77030000001", "Ходок", "completed", base)
	seedGuestBooking(t, pool, second, "+77030000001", "Ходок", "completed", base.Add(time.Hour))
	seedGuestBooking(t, pool, first, "+77030000001", "Ходок", "cancelled", base.Add(2*time.Hour))

	// Сотрудник ресторана — не гость, в списке ему не место.
	seedStaff(t, pool, "+77030000009", "manager")

	list, total, err := repo.Guests(ctx, domain.PlatformGuestQuery{})
	if err != nil {
		t.Fatalf("guests: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1 (сотрудник не должен попасть в список)", total)
	}
	g := byPhone(list, "+77030000001")
	if g == nil {
		t.Fatal("гость потерялся")
	}
	if g.BookingsCount != 3 || g.VisitsCount != 2 || g.CancelledCount != 1 {
		t.Fatalf("счётчики = %d/%d/%d, want 3 брони, 2 визита, 1 отмена",
			g.BookingsCount, g.VisitsCount, g.CancelledCount)
	}
	if g.VenuesCount != 2 {
		t.Fatalf("заведений = %d, want 2 (три брони в двух заведениях)", g.VenuesCount)
	}
}

func TestGuests_SearchFiltersAndPaginates(t *testing.T) {
	pool, repo, ctx := guestSetup(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rid := uuid.New()
	seedRestaurant(t, pool, rid, "Mongol", true)

	seedUser(t, pool, "+77040000001", "Алия", "Алматы", "ru", base)
	seedUser(t, pool, "+77040000002", "Асхат", "Астана", "kk", base.Add(time.Hour))
	seedUser(t, pool, "+77040000003", "Борис", "Алматы", "ru", base.Add(2*time.Hour))

	if _, total, _ := repo.Guests(ctx, domain.PlatformGuestQuery{Search: "ал"}); total != 1 {
		t.Fatalf("поиск по имени: total = %d, want 1", total)
	}
	if _, total, _ := repo.Guests(ctx, domain.PlatformGuestQuery{Search: "+7704000000"}); total != 3 {
		t.Fatalf("поиск по номеру: total = %d, want 3", total)
	}
	if _, total, _ := repo.Guests(ctx, domain.PlatformGuestQuery{City: "Алматы"}); total != 2 {
		t.Fatalf("фильтр по городу: total = %d, want 2", total)
	}
	if _, total, _ := repo.Guests(ctx, domain.PlatformGuestQuery{Language: "kk"}); total != 1 {
		t.Fatalf("фильтр по языку: total = %d, want 1", total)
	}

	// Страница меньше выборки: total остаётся ОБЩИМ, строк приходит ровно
	// столько, сколько влезло, — иначе панель нарисует одну страницу вместо трёх.
	list, total, err := repo.Guests(ctx, domain.PlatformGuestQuery{
		Sort: domain.GuestSortRegistered, Page: 1, PerPage: 2,
	})
	if err != nil {
		t.Fatalf("guests: %v", err)
	}
	if total != 3 || len(list) != 2 {
		t.Fatalf("total = %d, строк = %d, want 3 и 2", total, len(list))
	}
	if list[0].Phone != "+77040000003" {
		t.Fatalf("сортировка по регистрации: первым ожидался самый свежий, получено %s", list[0].Phone)
	}

	second, _, err := repo.Guests(ctx, domain.PlatformGuestQuery{
		Sort: domain.GuestSortRegistered, Page: 2, PerPage: 2,
	})
	if err != nil {
		t.Fatalf("вторая страница: %v", err)
	}
	if len(second) != 1 || second[0].Phone != "+77040000001" {
		t.Fatalf("вторая страница = %+v, want один самый старый аккаунт", second)
	}
}

func TestGuests_BookingWithoutPhoneIsNotAGuest(t *testing.T) {
	pool, repo, ctx := guestSetup(t)
	rid := uuid.New()
	seedRestaurant(t, pool, rid, "Mongol", true)

	// Наследство импорта: бронь без телефона. Идентичности у неё нет, склеить
	// не с чем — в списке гостей ей делать нечего.
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, name, phone, phone_normalized, guests, starts_at, ends_at, status)
		 VALUES ($1,$2,'Без телефона','','',2, now(), now() + interval '2 hours','confirmed')`,
		uuid.New(), rid); err != nil {
		t.Fatalf("seed booking without phone: %v", err)
	}

	_, total, err := repo.Guests(ctx, domain.PlatformGuestQuery{})
	if err != nil {
		t.Fatalf("guests: %v", err)
	}
	if total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
}

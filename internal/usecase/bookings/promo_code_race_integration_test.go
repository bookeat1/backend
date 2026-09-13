package bookings

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	promocoderepo "backend-core/internal/infrastructure/postgres/promocode"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/usecase/promocodes"
)

// racers is how many guests press «Забронировать» at the same instant. The
// code's limit is racers-1, so exactly one of them must lose.
const racers = 6

// uniqueCode returns a fresh normalized code string per test run. Codes are
// globally unique (promo_codes_code_key) and this package's convention is to
// seed fresh ids instead of truncating shared tables, so a fixed "MARATHON26"
// makes the second run of the day fail on the seed rather than on the logic.
// The returned value is already in normalized form; typedSpelling turns it
// back into something a human would type.
func uniqueCode() string {
	return strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:16]
}

// uniquePhone returns a phone nobody else in this database has. users.phone is
// unique and the package convention is not to truncate users, so a fixed number
// makes the second run of the day fail on the seed. The prefix stays a real
// Kazakh mobile one because the create path normalizes and validates it.
func uniquePhone(suffix int) string {
	return fmt.Sprintf("+7707%04d%03d", rand.Intn(10000), suffix%1000)
}

// typedSpelling is the same code the sloppy way: lower case, a dash in the
// middle and stray spaces around it. Passing this through the booking path
// exercises domain.NormalizePromoCode end to end (spec criterion 5) instead of
// only in its own unit test.
func typedSpelling(code string) string {
	low := strings.ToLower(code)
	return " " + low[:8] + "-" + low[8:] + " "
}

// TestPromoCodeLimitUnderConcurrentBookings is spec criterion 7: N guests book
// the same slot with the same code at the same moment, the code allows N-1,
// and exactly N-1 bookings come out carrying it.
//
// The venue is set up so the ONLY scarce resource is the code's limit —
// capacity mode with far more seats than the parties need. A table-mode venue
// (or a tight seat count) would make the losers fail with slot_taken and the
// test would be green without ever exercising the lock.
//
// Every guest is a DIFFERENT user with a DIFFERENT normalized phone: the
// anti-fraud counter in createUseCase.checkRate is per phone, and one shared
// phone would start refusing bookings at BOOKING_RATE_LIMIT for a reason that
// has nothing to do with promo codes.
//
// FALSE-GREEN CHECK (run by hand 12.09.2026, not automated): dropping
// `FOR UPDATE` from LockByID in infrastructure/postgres/promocode (one word,
// repository.go:104) makes this test fail with "ok=6 refused=0, want ok=5
// refused=1" — so the assertion really is held up by the row lock and not by
// something else in the create path. Redo that edit if you ever doubt this
// test; it takes half a minute.
func TestPromoCodeLimitUnderConcurrentBookings(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, integrationTables...)
	ctx := context.Background()

	rid, promoID, codeID := uuid.New(), uuid.New(), uuid.New()
	// Seats mode with room to spare: guests*racers seats and then some.
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active,
			booking_capacity_mode, booking_capacity_seats)
		 VALUES ($1,'R','Алматы','₸',true,'seats',$2)`, rid, racers*10); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	now := time.Now()
	if _, err := pool.Exec(ctx,
		`INSERT INTO promos (id, title, description, terms, starts_at, ends_at, status)
		 VALUES ($1,'Марафон Алматы','Бегите с нами','Покажите бронь',$2,$3,'published')`,
		promoID, now.Add(-24*time.Hour), now.Add(60*24*time.Hour)); err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	limit := racers - 1
	code := uniqueCode()
	if _, err := pool.Exec(ctx,
		`INSERT INTO promo_codes (id, code, promotion_id, starts_at, expires_at,
			max_uses_total, max_uses_per_user, status)
		 VALUES ($1,$6,$2,$3,$4,$5,1,'active')`,
		codeID, promoID, now.Add(-time.Hour), now.Add(30*24*time.Hour), limit, code); err != nil {
		t.Fatalf("seed promo code: %v", err)
	}

	users := make([]uuid.UUID, racers)
	phones := make([]string, racers)
	for i := range users {
		uid := uuid.New()
		users[i] = uid
		// Distinct normalized phones, unique per racer AND per run.
		phones[i] = uniquePhone(i)
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
			uid, uid.String()+"@example.com", phones[i]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM promo_codes WHERE id=$1`, codeID)
		_, _ = pool.Exec(bg, `DELETE FROM promos WHERE id=$1`, promoID)
		_, _ = pool.Exec(bg, `DELETE FROM restaurants WHERE id=$1`, rid)
		for _, uid := range users {
			_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, uid)
		}
	})

	txm := sqltx.NewManager(pool)
	bookingsRepo := bookingrepo.New(pool)
	codesFacade := promocodes.NewFacade(promocoderepo.New(pool), livePromos{promoID: promoID}, bookingsRepo)
	create := NewCreateUseCase(
		bookingsRepo, bookingrepo.NewTables(pool), bookingrepo.NewCapacity(pool),
		bookingrepo.NewItems(pool), bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool), newFakeManagers(),
		livePromos{promoID: promoID}, codesFacade, txm, testConfig(),
	)

	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)
	startsAt := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC()

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := users[i]
			<-start
			_, errs[i] = create.Create(context.Background(),
				Actor{UserID: uid, Role: domain.RoleUser}, CreateInput{
					RestaurantID: rid, UserID: &uid, Name: "Гость", Phone: phones[i],
					Guests: 2, StartsAt: startsAt, Source: domain.SourceApp,
					PromoCode: typedSpelling(code),
				})
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, refused int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		default:
			errCode, _ := domain.CodeOf(err)
			if errCode != domain.CodePromoCodeLimitReached {
				t.Fatalf("racer %d failed for the wrong reason: code=%q err=%v", i, errCode, err)
			}
			refused++
		}
	}
	if ok != limit || refused != racers-limit {
		t.Fatalf("ok=%d refused=%d, want ok=%d refused=%d", ok, refused, limit, racers-limit)
	}

	var tagged, distinct int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT user_id) FROM bookings
		  WHERE promo_code_id=$1 AND status NOT IN ('cancelled','no_show')`, codeID).
		Scan(&tagged, &distinct); err != nil {
		t.Fatalf("count tagged bookings: %v", err)
	}
	if tagged != limit || distinct != limit {
		t.Fatalf("%d bookings by %d guests carry the code, want %d/%d", tagged, distinct, limit, limit)
	}
	// The refused guest's booking must not be half-written: the whole
	// transaction rolls back, so the row count above is also the total.
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE restaurant_id=$1`, rid).Scan(&total); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if total != limit {
		t.Fatalf("bookings = %d, want %d — a refused code must leave nothing behind", total, limit)
	}
}

// TestBookingWithoutPromoCodePassesWhenTheCodeIsExhausted is the other half of
// criterion 7 and the product promise: the guest who lost the race books again
// WITHOUT the code and gets a booking.
func TestBookingWithoutPromoCodePassesWhenTheCodeIsExhausted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, integrationTables...)
	ctx := context.Background()

	rid, promoID, codeID, uid := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	guestPhone, otherPhone := uniquePhone(1), uniquePhone(2)
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active,
			booking_capacity_mode, booking_capacity_seats)
		 VALUES ($1,'R','Алматы','₸',true,'seats',50)`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	now := time.Now()
	if _, err := pool.Exec(ctx,
		`INSERT INTO promos (id, title, description, terms, starts_at, ends_at, status)
		 VALUES ($1,'Марафон','x','y',$2,$3,'published')`,
		promoID, now.Add(-24*time.Hour), now.Add(60*24*time.Hour)); err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	// A code nobody may use any more: the campaign-wide limit is spent by a
	// booking that already exists.
	code := uniqueCode()
	if _, err := pool.Exec(ctx,
		`INSERT INTO promo_codes (id, code, promotion_id, starts_at, expires_at,
			max_uses_total, max_uses_per_user, status)
		 VALUES ($1,$5,$2,$3,$4,1,1,'active')`,
		codeID, promoID, now.Add(-time.Hour), now.Add(30*24*time.Hour), code); err != nil {
		t.Fatalf("seed promo code: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'Guest')`,
		uid, uid.String()+"@example.com", guestPhone); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// The guest who already spent the code's only place. bookings.user_id DOES
	// carry a foreign key on users, so this row has to exist.
	other := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'First')`,
		other, other.String()+"@example.com", otherPhone); err != nil {
		t.Fatalf("seed the other guest: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, user_id, name, phone, email,
			phone_normalized, guests, starts_at, ends_at, status, source,
			promotion_id, promo_code_id, promo_code)
		 VALUES ($1,$2,$3,'Первый',$7,'a@b.c',$7,2,
			now()+interval '2 day', now()+interval '2 day 2 hour','confirmed','app',$4,$5,$6)`,
		uuid.New(), rid, other, promoID, codeID, code, otherPhone); err != nil {
		t.Fatalf("seed the booking that spends the limit: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM promo_codes WHERE id=$1`, codeID)
		_, _ = pool.Exec(bg, `DELETE FROM promos WHERE id=$1`, promoID)
		_, _ = pool.Exec(bg, `DELETE FROM restaurants WHERE id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, uid)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id=$1`, other)
	})

	txm := sqltx.NewManager(pool)
	bookingsRepo := bookingrepo.New(pool)
	create := NewCreateUseCase(
		bookingsRepo, bookingrepo.NewTables(pool), bookingrepo.NewCapacity(pool),
		bookingrepo.NewItems(pool), bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool), newFakeManagers(),
		livePromos{promoID: promoID},
		promocodes.NewFacade(promocoderepo.New(pool), livePromos{promoID: promoID}, bookingsRepo),
		txm, testConfig(),
	)

	loc, _ := time.LoadLocation("Asia/Almaty")
	day := time.Now().In(loc).AddDate(0, 0, 2)
	startsAt := time.Date(day.Year(), day.Month(), day.Day(), 13, 0, 0, 0, loc).UTC()
	in := CreateInput{
		RestaurantID: rid, UserID: &uid, Name: "Гость", Phone: guestPhone,
		Guests: 2, StartsAt: startsAt, Source: domain.SourceApp, PromoCode: code,
	}
	actor := Actor{UserID: uid, Role: domain.RoleUser}

	_, err := create.Create(ctx, actor, in)
	if errCode, _ := domain.CodeOf(err); errCode != domain.CodePromoCodeLimitReached {
		t.Fatalf("code = %q, want %q (err %v)", errCode, domain.CodePromoCodeLimitReached, err)
	}

	in.PromoCode = ""
	details, err := create.Create(ctx, actor, in)
	if err != nil {
		t.Fatalf("the same booking without the code must go through: %v", err)
	}
	if details.Booking.PromoCodeID != nil || details.Booking.PromotionID != nil {
		t.Fatalf("the fallback booking carries %v/%v, want neither",
			details.Booking.PromoCodeID, details.Booking.PromotionID)
	}
}

// livePromos is the promoReader both usecases need, answering "live" for one
// campaign id. The real promos facade is not wired here on purpose: this file
// is about the promo-code lock, and the campaign's own visibility rule already
// has its own tests.
type livePromos struct{ promoID uuid.UUID }

func (p livePromos) GetPublicDetail(_ context.Context, id uuid.UUID) (*domain.PromoListItem, error) {
	if id != p.promoID {
		return nil, domain.ErrNotFound
	}
	return &domain.PromoListItem{Promo: domain.Promo{ID: id, Title: "Марафон Алматы"}}, nil
}

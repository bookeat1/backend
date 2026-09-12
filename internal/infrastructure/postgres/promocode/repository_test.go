package promocode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/postgres/promo"
	"backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
)

// reset clears everything this package writes. promo_codes goes before promos
// on purpose: the FK is ON DELETE RESTRICT, and TRUNCATE ... CASCADE would
// otherwise take the codes along with the promos without the test ever
// noticing the order matters.
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	testdb.Truncate(t, pool, "bookings", "promo_codes", "promos", "restaurants", "users")
}

func seedRestaurant(ctx context.Context, t *testing.T, pool sqltx.Querier) uuid.UUID {
	t.Helper()
	r := &domain.Restaurant{
		ID: uuid.New(), Name: "Bistro", City: domain.CityAlmaty,
		PriceCategory: domain.PriceMid, IsActive: true,
	}
	if err := restaurant.New(pool).Create(ctx, r); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	return r.ID
}

func seedPromo(ctx context.Context, t *testing.T, pool sqltx.Querier, rid uuid.UUID) uuid.UUID {
	t.Helper()
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	p := &domain.Promo{
		ID: uuid.New(), RestaurantID: &rid, Title: "Марафон",
		StartsAt: start, EndsAt: start.Add(90 * 24 * time.Hour),
		Status: domain.PromoPublished,
	}
	if err := promo.New(pool).Create(ctx, p); err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	return p.ID
}

func seedUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, full_name, phone) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("Guest %d", n), fmt.Sprintf("+7700000%04d", n)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func mkCode(promotionID uuid.UUID, code string) *domain.PromoCode {
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	return &domain.PromoCode{
		Code:           code,
		PromotionID:    promotionID,
		StartsAt:       start,
		ExpiresAt:      start.Add(45 * 24 * time.Hour),
		MaxUsesPerUser: 1,
		Status:         domain.PromoCodeActive,
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	author := seedUser(ctx, t, pool, 1)
	repo := New(pool)

	total := 25
	c := mkCode(pid, "MARATHON26")
	c.MaxUsesTotal = &total
	c.MaxUsesPerUser = 2
	c.CreatedBy = &author
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Fatal("Create must fill in the id it generated")
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Fatal("Create must return the row's timestamps")
	}

	for _, tc := range []struct {
		name string
		get  func() (*domain.PromoCode, error)
	}{
		{"by id", func() (*domain.PromoCode, error) { return repo.GetByID(ctx, c.ID) }},
		{"by code", func() (*domain.PromoCode, error) { return repo.GetByCode(ctx, "MARATHON26") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.get()
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.ID != c.ID || got.Code != "MARATHON26" || got.PromotionID != pid {
				t.Fatalf("identity round trip broken: %+v", *got)
			}
			if got.MaxUsesTotal == nil || *got.MaxUsesTotal != total {
				t.Errorf("MaxUsesTotal = %v, want %d", got.MaxUsesTotal, total)
			}
			if got.MaxUsesPerUser != 2 {
				t.Errorf("MaxUsesPerUser = %d, want 2", got.MaxUsesPerUser)
			}
			if got.Status != domain.PromoCodeActive {
				t.Errorf("Status = %q, want active", got.Status)
			}
			if got.CreatedBy == nil || *got.CreatedBy != author {
				t.Errorf("CreatedBy = %v, want %v", got.CreatedBy, author)
			}
			if !got.StartsAt.Equal(c.StartsAt) || !got.ExpiresAt.Equal(c.ExpiresAt) {
				t.Errorf("window round trip broken: %v..%v", got.StartsAt, got.ExpiresAt)
			}
		})
	}

	t.Run("unlimited code keeps a NULL total", func(t *testing.T) {
		u := mkCode(pid, "UNLIMITED")
		if err := repo.Create(ctx, u); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.GetByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.MaxUsesTotal != nil {
			// NULL is "no limit" — a 0 here would mean "nobody may use it".
			t.Fatalf("MaxUsesTotal = %v, want nil", *got.MaxUsesTotal)
		}
	})
}

func TestGetMissingIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	repo := New(pool)

	if _, err := repo.GetByID(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByID = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByCode(ctx, "NOSUCHCODE"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByCode = %v, want ErrNotFound", err)
	}
}

// TestCreateRejectsDuplicateCode: uniqueness is the database's job, not a
// SELECT-then-INSERT — two admins creating the same code at the same time
// must not both succeed.
func TestCreateRejectsDuplicateCode(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	if err := repo.Create(ctx, mkCode(pid, "MARATHON26")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := repo.Create(ctx, mkCode(pid, "MARATHON26"))
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
}

func TestCreateRejectsUnknownPromo(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()

	err := New(pool).Create(ctx, mkCode(uuid.New(), "ORPHAN26"))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Create with an unknown promo = %v, want ErrNotFound", err)
	}
}

// TestCreateRejectsWhatTheCheckConstraintsRefuse. domain.PromoCode.Validate
// catches these first; this proves the database is a real second line and
// that its refusal comes back as ErrValidation (422), not a 500.
func TestCreateRejectsWhatTheCheckConstraintsRefuse(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	zero := 0
	tests := []struct {
		name  string
		spoil func(*domain.PromoCode)
	}{
		{"window closes before it opens", func(c *domain.PromoCode) { c.ExpiresAt = c.StartsAt.Add(-time.Hour) }},
		{"zero total limit", func(c *domain.PromoCode) { c.MaxUsesTotal = &zero }},
		{"zero per-user limit", func(c *domain.PromoCode) { c.MaxUsesPerUser = 0 }},
		{"unnormalized code", func(c *domain.PromoCode) { c.Code = "marathon26" }},
		{"unknown status", func(c *domain.PromoCode) { c.Status = "published" }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mkCode(pid, fmt.Sprintf("BADCODE%d", i))
			tt.spoil(c)
			if err := repo.Create(ctx, c); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Create = %v, want ErrValidation", err)
			}
		})
	}
}

func TestUpdateChangesLimitsAndStatusButNotTheCode(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	c := mkCode(pid, "MARATHON26")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}

	total := 3
	c.MaxUsesTotal = &total
	c.MaxUsesPerUser = 2
	c.Status = domain.PromoCodePaused
	c.ExpiresAt = c.ExpiresAt.Add(24 * time.Hour)
	// A caller that tries to rewrite the code string must not get away with
	// it: the code is snapshotted on every booking already taken with it.
	c.Code = "SOMETHINGELSE"
	if err := repo.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Code != "MARATHON26" {
		t.Errorf("Update rewrote the code string: %q", got.Code)
	}
	if got.MaxUsesTotal == nil || *got.MaxUsesTotal != total || got.MaxUsesPerUser != 2 {
		t.Errorf("limits not updated: %+v", *got)
	}
	if got.Status != domain.PromoCodePaused {
		t.Errorf("Status = %q, want paused", got.Status)
	}
	if !got.ExpiresAt.Equal(c.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, c.ExpiresAt)
	}

	t.Run("unknown id", func(t *testing.T) {
		missing := mkCode(pid, "MISSING26")
		missing.ID = uuid.New()
		if err := repo.Update(ctx, missing); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Update = %v, want ErrNotFound", err)
		}
	})
}

func TestDelete(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	c := mkCode(pid, "MARATHON26")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(ctx, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByID after Delete = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

// TestDeletingAPromoWithCodesIsRefused is the FK RESTRICT from migration 0108
// seen from the application side: a promo a live code points at cannot be
// deleted, so a code can never be left meaning nothing.
func TestDeletingAPromoWithCodesIsRefused(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)

	if err := New(pool).Create(ctx, mkCode(pid, "MARATHON26")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := promo.New(pool).Delete(ctx, pid); err == nil {
		t.Fatal("deleting a promo referenced by a promo code must be refused")
	}
}

func TestListFilters(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	other := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	active := mkCode(pid, "ACTIVE26")
	paused := mkCode(pid, "PAUSED26")
	paused.Status = domain.PromoCodePaused
	elsewhere := mkCode(other, "OTHER26")
	for _, c := range []*domain.PromoCode{active, paused, elsewhere} {
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create %s: %v", c.Code, err)
		}
	}

	all, err := repo.List(ctx, domain.PromoCodeFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() returned %d codes, want 3", len(all))
	}

	byPromo, err := repo.List(ctx, domain.PromoCodeFilter{PromotionID: &pid})
	if err != nil {
		t.Fatalf("List by promo: %v", err)
	}
	if len(byPromo) != 2 {
		t.Fatalf("List by promo returned %d, want 2", len(byPromo))
	}
	for _, c := range byPromo {
		if c.PromotionID != pid {
			t.Errorf("%s belongs to another promo", c.Code)
		}
	}

	byStatus, err := repo.List(ctx, domain.PromoCodeFilter{
		Statuses: []domain.PromoCodeStatus{domain.PromoCodePaused},
	})
	if err != nil {
		t.Fatalf("List by status: %v", err)
	}
	if len(byStatus) != 1 || byStatus[0].Code != "PAUSED26" {
		t.Fatalf("List by status returned %+v, want only PAUSED26", byStatus)
	}

	both, err := repo.List(ctx, domain.PromoCodeFilter{
		PromotionID: &pid,
		Statuses:    []domain.PromoCodeStatus{domain.PromoCodeActive, domain.PromoCodePaused},
	})
	if err != nil {
		t.Fatalf("List by promo and statuses: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("combined filter returned %d, want 2", len(both))
	}
}

// TestLockByIDOutsideATransactionIsRefused. Outside a transaction the FOR
// UPDATE lock is gone the moment the statement ends, so a caller that skipped
// WithinTx would serialize nothing while believing it did. Loud failure beats
// handing out the last place twice.
func TestLockByIDOutsideATransactionIsRefused(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	c := mkCode(pid, "MARATHON26")
	if err := repo.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.LockByID(ctx, c.ID); !errors.Is(err, ErrNotInTransaction) {
		t.Fatalf("LockByID outside a tx = %v, want ErrNotInTransaction", err)
	}
}

func TestLockByIDMissingIsNotFound(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	repo := New(pool)

	err := sqltx.NewManager(pool).WithinTx(ctx, func(ctx context.Context) error {
		_, err := repo.LockByID(ctx, uuid.New())
		return err
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("LockByID = %v, want ErrNotFound", err)
	}
}

// TestLockByIDSerializesConcurrentRedemptions is the ADR-047 mechanism under
// real concurrency: N goroutines run the exact sequence a booking creation
// will run (lock the code row, count activations from bookings, insert a
// booking if the limit allows) against ONE code whose total limit is N-1.
//
// Without the FOR UPDATE lock every goroutine reads the same count of 0 and
// all N get in — the campaign then owes merch to more people than it has. The
// assertion is therefore exact: N-1 bookings, one refusal.
func TestLockByIDSerializesConcurrentRedemptions(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)
	tx := sqltx.NewManager(pool)

	const guests = 5
	limit := guests - 1
	code := mkCode(pid, "MARATHON26")
	code.MaxUsesTotal = &limit
	if err := repo.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	users := make([]uuid.UUID, guests)
	for i := range users {
		users[i] = seedUser(ctx, t, pool, i)
	}

	errLimit := errors.New("promo code limit reached")
	redeem := func(userID uuid.UUID) error {
		return tx.WithinTx(ctx, func(ctx context.Context) error {
			// 1. lock the code row — nothing else about this code may run
			//    between the count below and the insert that follows it.
			c, err := repo.LockByID(ctx, code.ID)
			if err != nil {
				return err
			}
			// 2. count the activations (ADR-047: distinct guests, cancelled
			//    and no-show bookings do not hold a place).
			var used int
			if err := sqltx.From(ctx, pool).QueryRow(ctx,
				`SELECT count(DISTINCT user_id) FROM bookings
				  WHERE promo_code_id = $1 AND status NOT IN ('cancelled', 'no_show')`,
				c.ID).Scan(&used); err != nil {
				return err
			}
			if c.MaxUsesTotal != nil && used >= *c.MaxUsesTotal {
				return errLimit
			}
			// 3. the booking itself.
			start := time.Now().Add(24 * time.Hour)
			_, err = sqltx.From(ctx, pool).Exec(ctx,
				`INSERT INTO bookings (id, restaurant_id, user_id, name, phone, phone_normalized,
					guests, starts_at, ends_at, promotion_id, promo_code_id, promo_code)
				 VALUES ($1, $2, $3, 'Guest', '+7700', $4, 2, $5, $6, $7, $8, $9)`,
				uuid.New(), rid, userID, fmt.Sprintf("+7700000%04d", userID.ID()%10000),
				start, start.Add(2*time.Hour), c.PromotionID, c.ID, c.Code)
			return err
		})
	}

	var wg sync.WaitGroup
	results := make([]error, guests)
	for i := 0; i < guests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = redeem(users[i])
		}(i)
	}
	wg.Wait()

	var ok, refused int
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errLimit):
			refused++
		default:
			t.Fatalf("guest %d failed for an unrelated reason: %v", i, err)
		}
	}
	if ok != limit || refused != guests-limit {
		t.Fatalf("got %d redemptions and %d refusals, want %d and %d",
			ok, refused, limit, guests-limit)
	}

	var stored int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT user_id) FROM bookings WHERE promo_code_id = $1`, code.ID).Scan(&stored); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if stored != limit {
		t.Fatalf("%d bookings carry the code, want %d", stored, limit)
	}
}

// TestCancelledBookingFreesItsPlace is the property the missing counter buys
// (ADR-047): a cancellation or a no-show drops out of the count with no
// decrement code at all, so the freed place is immediately reusable.
func TestCancelledBookingFreesItsPlace(t *testing.T) {
	pool := testdb.Connect(t)
	reset(t, pool)
	ctx := context.Background()
	rid := seedRestaurant(ctx, t, pool)
	pid := seedPromo(ctx, t, pool, rid)
	repo := New(pool)

	code := mkCode(pid, "MARATHON26")
	one := 1
	code.MaxUsesTotal = &one
	if err := repo.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	user := seedUser(ctx, t, pool, 1)

	start := time.Now().Add(24 * time.Hour)
	bookingID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO bookings (id, restaurant_id, user_id, name, phone, phone_normalized,
			guests, starts_at, ends_at, promotion_id, promo_code_id, promo_code)
		 VALUES ($1, $2, $3, 'Guest', '+7700', '+77000000001', 2, $4, $5, $6, $7, $8)`,
		bookingID, rid, user, start, start.Add(2*time.Hour), pid, code.ID, code.Code); err != nil {
		t.Fatalf("insert booking: %v", err)
	}

	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(DISTINCT user_id) FROM bookings
			  WHERE promo_code_id = $1 AND status NOT IN ('cancelled', 'no_show')`,
			code.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if got := count(); got != 1 {
		t.Fatalf("a live booking counts %d, want 1", got)
	}

	for _, status := range []string{"cancelled", "no_show"} {
		if _, err := pool.Exec(ctx, `UPDATE bookings SET status = $2 WHERE id = $1`, bookingID, status); err != nil {
			t.Fatalf("set %s: %v", status, err)
		}
		if got := count(); got != 0 {
			t.Fatalf("a %s booking still counts %d, want 0", status, got)
		}
	}
}

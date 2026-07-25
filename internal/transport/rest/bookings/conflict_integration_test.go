package bookings

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	bookingrepo "backend-core/internal/infrastructure/postgres/booking"
	idemrepo "backend-core/internal/infrastructure/postgres/idempotency"
	restrepo "backend-core/internal/infrastructure/postgres/restaurant"
	"backend-core/internal/infrastructure/postgres/testdb"
	"backend-core/internal/infrastructure/sqltx"
	"backend-core/internal/transport/rest/middleware"
	uc "backend-core/internal/usecase/bookings"
)

// This file answers a contract question the fake-based handler tests cannot:
// what exactly does a client receive when two DIFFERENT conflicts both come out
// as 409? Before the code field existed the two bodies were byte-identical, so
// the mobile app read a lost slot race as "your earlier submit went through" and
// sent the guest looking for a reservation that does not exist.
//
// What these tests actually exercise, precisely:
//
//   - the slot conflict is raised by Postgres itself — the GiST exclusion
//     constraint on booking_tables rejects the INSERT;
//   - the idempotency conflict is NOT a constraint violation. The stored key is
//     read back with a plain SELECT (idempotency.replay → keys.Get) and the
//     request hash is compared in Go; the 409 is a decision the usecase makes,
//     not an error the database returns.
//
// Both run over the real usecases and the real Postgres repositories — no
// usecase, repository or error is stubbed.
//
// NOT covered here, and not covered anywhere on this branch: the concurrent
// first-attempt race, where two simultaneous requests collide on the unique
// index of idempotency_keys and the loser replays the winner's stored response
// (described at idempotency.go:40-46, handled at :96-106). Reproducing it needs
// two in-flight transactions whose bookings do NOT collide on tables first, so
// it does not belong in this file; treat that path as untested.

var conflictTables = []string{
	"booking_outbox", "booking_status_history", "booking_rate_log",
	"booking_blacklist", "booking_items", "booking_tables", "bookings",
	"idempotency_keys",
}

type conflictHarness struct {
	pool   *pgxpool.Pool
	router *gin.Engine

	restaurantID uuid.UUID
	tableID      uuid.UUID
	guestID      uuid.UUID
	managerID    uuid.UUID
	startsAt     time.Time
}

// newConflictHarness seeds a venue open 10:00–23:00 with exactly ONE table and
// mounts the real handler over real repositories. The router carries both the
// guest routes and the venue-scoped ones, so one test can drive a guest booking
// and a staff booking against the same data.
func newConflictHarness(t *testing.T) *conflictHarness {
	t.Helper()
	pool := testdb.Connect(t)
	testdb.Truncate(t, pool, conflictTables...)
	ctx := context.Background()

	rid, tid := uuid.New(), uuid.New()
	guestID, managerID := uuid.New(), uuid.New()

	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurants (id, name, city, price_category, is_active)
		 VALUES ($1,'R','Алматы','₸',true)`, rid); err != nil {
		t.Fatalf("seed restaurant: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM restaurant_tables WHERE restaurant_id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM restaurants WHERE id=$1`, rid)
		_, _ = pool.Exec(bg, `DELETE FROM users WHERE id = ANY($1)`, []uuid.UUID{guestID, managerID})
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO restaurant_tables (id, restaurant_id, name, capacity, is_active)
		 VALUES ($1,$2,'T1',4,true)`, tid, rid); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	for day := 0; day < 7; day++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO restaurant_working_hours (id, restaurant_id, day_of_week, open_time, close_time, is_open)
			 VALUES ($1,$2,$3,'10:00','23:00',true)`, uuid.New(), rid, day); err != nil {
			t.Fatalf("seed working hours: %v", err)
		}
	}
	for _, u := range []uuid.UUID{guestID, managerID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, phone, full_name) VALUES ($1,$2,$3,'U')`,
			u, u.String()+"@example.com", "+7777"+u.String()[:7]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	txm := sqltx.NewManager(pool)
	create := uc.NewCreateUseCase(
		bookingrepo.New(pool), bookingrepo.NewTables(pool), bookingrepo.NewItems(pool),
		bookingrepo.NewHistory(pool), bookingrepo.NewOutbox(pool),
		bookingrepo.NewBlacklist(pool), bookingrepo.NewRateLog(pool),
		restrepo.New(pool), restrepo.NewRelated(pool),
		fakeManagers{manages: true}, txm, conflictConfig(),
	)
	idempotent := uc.NewIdempotentCreateUseCase(create, idemrepo.New(pool), txm)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(&fakeFacade{}, create, idempotent, &fakeStatus{}, &fakeUpdate{},
		&fakeAvail{}, &fakeBlacklist{}, &fakePolicy{}, &fakeExternal{})
	api := r.Group("/api/v1")
	h.RegisterPublic(api)
	authed := api.Group("")
	// The role is resolved per request from the token's user id, so the same
	// router serves the guest and the manager.
	authed.Use(middleware.Auth(fakeIssuer{}, roleByID{manager: managerID}))
	h.RegisterRoutes(authed)
	scoped := authed.Group("")
	scoped.Use(middleware.RequireRestaurantManager(fakeManagers{manages: true}, "id"))
	h.RegisterRestaurantScoped(scoped)

	loc, err := time.LoadLocation("Asia/Almaty")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)

	return &conflictHarness{
		pool: pool, router: r,
		restaurantID: rid, tableID: tid,
		guestID: guestID, managerID: managerID,
		startsAt: time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc).UTC(),
	}
}

func conflictConfig() uc.Config {
	return uc.Config{
		DefaultDuration:       120 * time.Minute,
		DefaultBuffer:         15 * time.Minute,
		DefaultLead:           60 * time.Minute,
		DefaultHorizonDays:    60,
		DefaultCancelDeadline: 180 * time.Minute,
		DefaultConfirmSLA:     120 * time.Minute,
		DefaultMaxGuests:      20,
		DefaultAutoConfirm:    true,
		TimezoneFallback:      "Asia/Almaty",
		RateWindow:            time.Hour,
		RateLimit:             100,
		SlotStep:              30 * time.Minute,
	}
}

// roleByID hands out RoleRestaurant to one specific user and RoleUser to
// everybody else.
type roleByID struct{ manager uuid.UUID }

func (r roleByID) Create(context.Context, *domain.User) error { return nil }
func (r roleByID) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	role := domain.RoleUser
	if id == r.manager {
		role = domain.RoleRestaurant
	}
	return &domain.User{ID: id, Role: role, IsActive: true}, nil
}
func (r roleByID) GetByEmail(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (r roleByID) GetByPhone(context.Context, string) (*domain.User, error) {
	return nil, domain.ErrNotFound
}
func (r roleByID) Update(context.Context, *domain.User) error { return nil }
func (r roleByID) Delete(context.Context, uuid.UUID) error    { return nil }

func (h *conflictHarness) guestBody(guests int) gin.H {
	return gin.H{
		"restaurant_id": h.restaurantID.String(),
		"name":          "Дамир",
		"phone":         "+7 (707) 123-45-67",
		"guests":        guests,
		"starts_at":     h.startsAt.Format(time.RFC3339),
	}
}

// errorBody is the error half of response.Envelope as a client sees it.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func decodeError(t *testing.T, raw []byte) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode error body %s: %v", raw, err)
	}
	return got
}

// TestConflictCodesAreDistinct is the whole point of the change: a lost slot
// race and a replayed Idempotency-Key must not look the same on the wire.
func TestConflictCodesAreDistinct(t *testing.T) {
	h := newConflictHarness(t)

	// --- 1. a real capacity conflict.
	//
	// The guest books the venue's only table. The manager then pins THAT table
	// for another party at the same time: manual placement skips the
	// availability search, so the write reaches Postgres and the GiST exclusion
	// constraint on booking_tables is what rejects it — the same failure a
	// guest hits when someone else commits first.
	guestHeaders := authHeader(h.guestID)
	guestHeaders[idempotencyHeader] = "slot-key-1"
	if w := do(h.router, http.MethodPost, "/api/v1/bookings", h.guestBody(2), guestHeaders); w.Code != http.StatusCreated {
		t.Fatalf("first booking: status = %d, want 201 (body %s)", w.Code, w.Body)
	}

	staffBody := h.guestBody(2)
	staffBody["table_ids"] = []string{h.tableID.String()}
	staff := do(h.router, http.MethodPost,
		"/api/v1/restaurants/"+h.restaurantID.String()+"/bookings",
		staffBody, authHeader(h.managerID))
	if staff.Code != http.StatusConflict {
		t.Fatalf("slot conflict: status = %d, want 409 (body %s)", staff.Code, staff.Body)
	}
	slot := decodeError(t, staff.Body.Bytes())
	if slot.Code != string(domain.CodeSlotTaken) {
		t.Errorf("slot conflict: code = %q, want %q (body %s)", slot.Code, domain.CodeSlotTaken, staff.Body)
	}
	if slot.Error != "already exists" {
		t.Errorf("slot conflict: error = %q, want the unchanged %q", slot.Error, "already exists")
	}

	// --- 2. a real Idempotency-Key replayed with a different body.
	//
	// slot-key-1 is already stored for the two-guest request above; sending
	// four guests under the same key hits the stored request hash.
	replay := do(h.router, http.MethodPost, "/api/v1/bookings", h.guestBody(4), guestHeaders)
	if replay.Code != http.StatusConflict {
		t.Fatalf("key reuse: status = %d, want 409 (body %s)", replay.Code, replay.Body)
	}
	idem := decodeError(t, replay.Body.Bytes())
	if idem.Code != string(domain.CodeIdempotencyKeyReused) {
		t.Errorf("key reuse: code = %q, want %q (body %s)", idem.Code, domain.CodeIdempotencyKeyReused, replay.Body)
	}
	if idem.Error != "already exists" {
		t.Errorf("key reuse: error = %q, want the unchanged %q", idem.Error, "already exists")
	}

	// --- 3. the regression itself: the two responses must differ.
	if staff.Body.String() == replay.Body.String() {
		t.Fatalf("both conflicts returned the identical body %s — a client cannot tell "+
			"a lost slot from a replayed key", staff.Body)
	}
}

// TestNoTableAvailableHasItsOwnCode covers the third 409 the booking endpoint
// can produce: the party simply does not fit at that time. Nothing was booked
// here either, so it must not share the idempotency code.
func TestNoTableAvailableHasItsOwnCode(t *testing.T) {
	h := newConflictHarness(t)

	headers := authHeader(h.guestID)
	headers[idempotencyHeader] = "first"
	if w := do(h.router, http.MethodPost, "/api/v1/bookings", h.guestBody(2), headers); w.Code != http.StatusCreated {
		t.Fatalf("first booking: status = %d, want 201 (body %s)", w.Code, w.Body)
	}

	headers[idempotencyHeader] = "second"
	w := do(h.router, http.MethodPost, "/api/v1/bookings", h.guestBody(2), headers)
	if w.Code != http.StatusConflict {
		t.Fatalf("second booking: status = %d, want 409 (body %s)", w.Code, w.Body)
	}
	if got := decodeError(t, w.Body.Bytes()); got.Code != string(domain.CodeNoTableAvailable) {
		t.Errorf("code = %q, want %q (body %s)", got.Code, domain.CodeNoTableAvailable, w.Body)
	}
}

package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/usecase/bookings"
	"backend-core/internal/usecase/menu"
	"backend-core/internal/usecase/restaurants"
)

// ---- fakes -----------------------------------------------------------------

// fakePerms is the RBAC matrix under test: a permission is granted only for the
// exact (user, restaurant, perm) triples seeded into grant. Anything else — a
// wrong permission, or the RIGHT permission but a DIFFERENT restaurant (the
// cross-tenant case) — is denied, exactly as the real (actor, restaurant)
// lookup would deny it.
type fakePerms struct{ grant map[string]bool }

func key(userID, restaurantID uuid.UUID, perm domain.Permission) string {
	return userID.String() + "|" + restaurantID.String() + "|" + string(perm)
}

func (f fakePerms) HasPermission(_ context.Context, userID, restaurantID uuid.UUID, perm domain.Permission) (bool, error) {
	return f.grant[key(userID, restaurantID, perm)], nil
}

type fakeRest struct{ updated bool }

func (f *fakeRest) Get(_ context.Context, id uuid.UUID) (*domain.RestaurantAggregate, error) {
	a := &domain.RestaurantAggregate{}
	a.ID = id
	return a, nil
}
func (f *fakeRest) Update(_ context.Context, id uuid.UUID, _ restaurants.SaveInput) (*domain.RestaurantAggregate, error) {
	f.updated = true
	a := &domain.RestaurantAggregate{}
	a.ID = id
	return a, nil
}

type fakeMenu struct {
	created, updated, deleted, availSet bool
	bulkIDs                             []uuid.UUID
}

func (f *fakeMenu) ListByRestaurant(_ context.Context, _ uuid.UUID, _ *string) ([]domain.MenuItem, error) {
	return nil, nil
}
func (f *fakeMenu) Categories(_ context.Context) ([]domain.MenuCategory, error) { return nil, nil }
func (f *fakeMenu) Create(_ context.Context, rid uuid.UUID, _ menu.ItemInput) (*domain.MenuItem, error) {
	f.created = true
	return &domain.MenuItem{ID: uuid.New(), RestaurantID: rid}, nil
}
func (f *fakeMenu) Update(_ context.Context, rid, itemID uuid.UUID, _ menu.ItemInput) (*domain.MenuItem, error) {
	f.updated = true
	return &domain.MenuItem{ID: itemID, RestaurantID: rid}, nil
}
func (f *fakeMenu) Delete(_ context.Context, _, _ uuid.UUID) error { f.deleted = true; return nil }
func (f *fakeMenu) SetAvailable(_ context.Context, _, _ uuid.UUID, _ bool) error {
	f.availSet = true
	return nil
}
func (f *fakeMenu) SetAvailableBulk(_ context.Context, _ uuid.UUID, ids []uuid.UUID, _ bool) (int, error) {
	f.bulkIDs = ids
	return len(ids), nil
}

type fakeWH struct{ replaced bool }

func (f *fakeWH) ListWorkingHours(_ context.Context, _ uuid.UUID) ([]domain.WorkingHours, error) {
	return nil, nil
}
func (f *fakeWH) ReplaceWorkingHours(_ context.Context, _ uuid.UUID, _ []domain.WorkingHours) error {
	f.replaced = true
	return nil
}

type fakeOverrides struct{ upserted, deleted bool }

func (f *fakeOverrides) ListByRestaurant(_ context.Context, _ uuid.UUID) ([]domain.ScheduleOverride, error) {
	return nil, nil
}
func (f *fakeOverrides) Upsert(_ context.Context, _ *domain.ScheduleOverride) error {
	f.upserted = true
	return nil
}
func (f *fakeOverrides) Delete(_ context.Context, _ uuid.UUID, _ time.Time) error {
	f.deleted = true
	return nil
}

type fakeGuests struct{ listed bool }

func (f *fakeGuests) ListByRestaurant(_ context.Context, _ uuid.UUID) ([]domain.RestaurantGuest, error) {
	f.listed = true
	return nil, nil
}

type fakeBookingList struct{ listed bool }

func (f *fakeBookingList) ListByRestaurant(_ context.Context, _ bookings.Actor, _ uuid.UUID, _ domain.BookingFilter) ([]domain.Booking, int, error) {
	f.listed = true
	return nil, 0, nil
}

type fakeBookingTx struct{ confirmed, rejected, cancelled, noShow bool }

func (f *fakeBookingTx) Confirm(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	f.confirmed = true
	return &domain.Booking{ID: id}, nil
}
func (f *fakeBookingTx) Reject(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	f.rejected = true
	return &domain.Booking{ID: id}, nil
}
func (f *fakeBookingTx) Cancel(_ context.Context, _ bookings.Actor, id uuid.UUID, _ bookings.CancelInput) (*domain.Booking, error) {
	f.cancelled = true
	return &domain.Booking{ID: id}, nil
}
func (f *fakeBookingTx) NoShow(_ context.Context, _ bookings.Actor, id uuid.UUID, _ *string) (*domain.Booking, error) {
	f.noShow = true
	return &domain.Booking{ID: id}, nil
}

// harness bundles the usecase with the fakes so a test can both drive it and
// assert which delegate was reached.
type harness struct {
	uc        *UseCase
	perms     fakePerms
	rest      *fakeRest
	menu      *fakeMenu
	wh        *fakeWH
	overrides *fakeOverrides
	guests    *fakeGuests
	bookList  *fakeBookingList
	bookTx    *fakeBookingTx
	paySet    *fakePaymentSettings
}

func newHarness(grant map[string]bool) *harness {
	h := &harness{
		perms:     fakePerms{grant: grant},
		rest:      &fakeRest{},
		menu:      &fakeMenu{},
		wh:        &fakeWH{},
		overrides: &fakeOverrides{},
		guests:    &fakeGuests{},
		bookList:  &fakeBookingList{},
		bookTx:    &fakeBookingTx{},
		paySet:    &fakePaymentSettings{},
	}
	h.uc = NewUseCase(h.perms, h.rest, h.menu, h.wh, h.overrides, h.guests, h.bookList, h.bookTx, h.paySet)
	return h
}

// fakePaymentSettings records the last free-cancel-window write.
type fakePaymentSettings struct {
	lastRestaurant uuid.UUID
	lastMinutes    int
	calls          int
	err            error
}

func (f *fakePaymentSettings) UpdateFreeCancelWindow(_ context.Context, restaurantID uuid.UUID, minutes int) error {
	if f.err != nil {
		return f.err
	}
	f.calls++
	f.lastRestaurant = restaurantID
	f.lastMinutes = minutes
	return nil
}

// grantAll seeds every permission a role holds at restaurantID for userID, from
// the SINGLE source of truth (domain.staffPermissions via HasPermission), so the
// test's expectations can never silently drift from the real matrix.
func grantAll(userID, restaurantID uuid.UUID, role domain.StaffRole) map[string]bool {
	g := map[string]bool{}
	for _, p := range []domain.Permission{
		domain.PermBookingManage, domain.PermPaymentCapture, domain.PermPaymentRefund,
		domain.PermStaffManage, domain.PermRestaurantManage, domain.PermMenuStopList,
	} {
		if role.HasPermission(p) {
			g[key(userID, restaurantID, p)] = true
		}
	}
	return g
}

// ---- tests -----------------------------------------------------------------

func staffActor(id uuid.UUID) Actor { return Actor{UserID: id, Role: domain.RoleRestaurant} }

// TestHostessRejectedFromProfileAndMenuEdit is the core RBAC requirement: a
// hostess must NOT be able to edit the venue profile or the menu, and no
// delegate is reached when authorization fails.
func TestHostessRejectedFromProfileAndMenuEdit(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, rid, domain.StaffRoleHostess))
	actor := staffActor(uid)
	ctx := context.Background()

	if _, err := h.uc.UpdateProfile(ctx, actor, rid, ProfileInput{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hostess UpdateProfile: got %v, want ErrForbidden", err)
	}
	if h.rest.updated {
		t.Fatal("hostess reached the restaurant update delegate")
	}
	if _, err := h.uc.UpdateMenuItem(ctx, actor, rid, uuid.New(), menu.ItemInput{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hostess UpdateMenuItem: got %v, want ErrForbidden", err)
	}
	if _, err := h.uc.CreateMenuItem(ctx, actor, rid, menu.ItemInput{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hostess CreateMenuItem: got %v, want ErrForbidden", err)
	}
	if h.menu.created || h.menu.updated {
		t.Fatal("hostess reached a menu-edit delegate")
	}
}

// TestHostessAllowedStopListAndBookings confirms the hostess CAN do the two
// things the matrix grants her: the fast stop list and booking transitions.
func TestHostessAllowedStopListAndBookings(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, rid, domain.StaffRoleHostess))
	actor := staffActor(uid)
	ctx := context.Background()

	if _, err := h.uc.SetStopList(ctx, actor, rid, []uuid.UUID{uuid.New()}, false); err != nil {
		t.Fatalf("hostess SetStopList: unexpected error %v", err)
	}
	if len(h.menu.bulkIDs) != 1 {
		t.Fatal("hostess stop list did not reach the bulk delegate")
	}
	if _, err := h.uc.ConfirmBooking(ctx, actor, rid, uuid.New(), nil); err != nil {
		t.Fatalf("hostess ConfirmBooking: unexpected error %v", err)
	}
	if !h.bookTx.confirmed {
		t.Fatal("hostess confirm did not reach the transition delegate")
	}
}

// TestManagerAllowedProfileAndMenuEdit is the positive control: a manager holds
// restaurant.manage and reaches the delegates.
func TestManagerAllowedProfileAndMenuEdit(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, rid, domain.StaffRoleManager))
	actor := staffActor(uid)
	ctx := context.Background()

	if _, err := h.uc.UpdateProfile(ctx, actor, rid, ProfileInput{}); err != nil {
		t.Fatalf("manager UpdateProfile: unexpected error %v", err)
	}
	if !h.rest.updated {
		t.Fatal("manager UpdateProfile did not reach the delegate")
	}
	if _, err := h.uc.CreateMenuItem(ctx, actor, rid, menu.ItemInput{}); err != nil {
		t.Fatalf("manager CreateMenuItem: unexpected error %v", err)
	}
	if !h.menu.created {
		t.Fatal("manager CreateMenuItem did not reach the delegate")
	}
	if _, err := h.uc.ListGuests(ctx, actor, rid); err != nil {
		t.Fatalf("manager ListGuests: unexpected error %v", err)
	}
	if !h.guests.listed {
		t.Fatal("manager ListGuests did not reach the delegate")
	}
}

// TestCrossTenantRejected is the IDOR guard: a manager of restaurant A holding
// full rights at A is still rejected when acting on restaurant B, because the
// permission lookup is keyed by (actor, restaurant) — the path id can never be
// trusted on its own.
func TestCrossTenantRejected(t *testing.T) {
	uid, restA, restB := uuid.New(), uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, restA, domain.StaffRoleManager)) // rights at A only
	actor := staffActor(uid)
	ctx := context.Background()

	if _, err := h.uc.UpdateProfile(ctx, actor, restB, ProfileInput{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-tenant UpdateProfile: got %v, want ErrForbidden", err)
	}
	if _, err := h.uc.GetProfile(ctx, actor, restB); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-tenant GetProfile: got %v, want ErrForbidden", err)
	}
	if _, err := h.uc.SetStopList(ctx, actor, restB, []uuid.UUID{uuid.New()}, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-tenant SetStopList: got %v, want ErrForbidden", err)
	}
	if _, _, err := h.uc.ListBookings(ctx, actor, restB, domain.BookingFilter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("cross-tenant ListBookings: got %v, want ErrForbidden", err)
	}
	if h.rest.updated || len(h.menu.bulkIDs) != 0 || h.bookList.listed {
		t.Fatal("a cross-tenant call reached a delegate")
	}
}

// TestSuperadminBypass confirms a global admin passes every gate without any
// staff row (empty grant map).
func TestSuperadminBypass(t *testing.T) {
	rid := uuid.New()
	h := newHarness(map[string]bool{})
	admin := Actor{UserID: uuid.New(), Role: domain.RoleAdmin}
	ctx := context.Background()

	if _, err := h.uc.UpdateProfile(ctx, admin, rid, ProfileInput{}); err != nil {
		t.Fatalf("superadmin UpdateProfile: unexpected error %v", err)
	}
	if _, err := h.uc.SetScheduleOverride(ctx, admin, rid, ScheduleOverrideInput{
		Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), IsClosed: true,
	}); err != nil {
		t.Fatalf("superadmin SetScheduleOverride: unexpected error %v", err)
	}
	if !h.rest.updated || !h.overrides.upserted {
		t.Fatal("superadmin did not reach a delegate")
	}
}

// TestUnauthenticatedRejected guards the nil-actor path.
func TestUnauthenticatedRejected(t *testing.T) {
	h := newHarness(map[string]bool{})
	if _, err := h.uc.GetProfile(context.Background(), Actor{}, uuid.New()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("nil actor: got %v, want ErrUnauthorized", err)
	}
}

// TestScheduleOverrideValidation rejects an "open" override with no/invalid
// times before any repo write, and clears times for a closed day.
func TestScheduleOverrideValidation(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, rid, domain.StaffRoleManager))
	actor := staffActor(uid)
	ctx := context.Background()
	day := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)

	if _, err := h.uc.SetScheduleOverride(ctx, actor, rid, ScheduleOverrideInput{Date: day, IsClosed: false}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("open override without times: got %v, want ErrValidation", err)
	}
	if h.overrides.upserted {
		t.Fatal("invalid override reached the repo")
	}
	bad := "25:00"
	if _, err := h.uc.SetScheduleOverride(ctx, actor, rid, ScheduleOverrideInput{Date: day, OpenTime: &bad, CloseTime: &bad}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("open override with bad time: got %v, want ErrValidation", err)
	}
	closed, err := h.uc.SetScheduleOverride(ctx, actor, rid, ScheduleOverrideInput{Date: day, IsClosed: true})
	if err != nil {
		t.Fatalf("closed override: unexpected error %v", err)
	}
	if closed.OpenTime != nil || closed.CloseTime != nil {
		t.Fatal("closed override kept times")
	}
}

// TestWorkingHoursValidation rejects an out-of-range day and an open day with
// no times.
func TestWorkingHoursValidation(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	h := newHarness(grantAll(uid, rid, domain.StaffRoleManager))
	actor := staffActor(uid)
	ctx := context.Background()

	if err := h.uc.SetWorkingHours(ctx, actor, rid, []domain.WorkingHours{{DayOfWeek: 9, IsOpen: false}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad day_of_week: got %v, want ErrValidation", err)
	}
	if err := h.uc.SetWorkingHours(ctx, actor, rid, []domain.WorkingHours{{DayOfWeek: 1, IsOpen: true}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("open day without times: got %v, want ErrValidation", err)
	}
	if h.wh.replaced {
		t.Fatal("invalid working hours reached the repo")
	}
}

func TestSetFreeCancelWindow(t *testing.T) {
	uid, rid := uuid.New(), uuid.New()
	ctx := context.Background()

	// Manager holds restaurant.manage → allowed, delegated with the value.
	h := newHarness(grantAll(uid, rid, domain.StaffRoleManager))
	if err := h.uc.SetFreeCancelWindow(ctx, staffActor(uid), rid, 300); err != nil {
		t.Fatalf("manager SetFreeCancelWindow: %v", err)
	}
	if h.paySet.calls != 1 || h.paySet.lastMinutes != 300 || h.paySet.lastRestaurant != rid {
		t.Fatalf("writer got calls=%d minutes=%d restaurant=%s, want 1/300/%s",
			h.paySet.calls, h.paySet.lastMinutes, h.paySet.lastRestaurant, rid)
	}

	// Out-of-range value is rejected BEFORE the writer is touched.
	if err := h.uc.SetFreeCancelWindow(ctx, staffActor(uid), rid, -1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative window: got %v, want ErrValidation", err)
	}
	if err := h.uc.SetFreeCancelWindow(ctx, staffActor(uid), rid, maxFreeCancelWindowMinutes+1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("too-large window: got %v, want ErrValidation", err)
	}
	if h.paySet.calls != 1 {
		t.Fatalf("writer called %d times, want it untouched by the rejected values", h.paySet.calls)
	}

	// A hostess (no restaurant.manage) is forbidden.
	hh := newHarness(grantAll(uid, rid, domain.StaffRoleHostess))
	if err := hh.uc.SetFreeCancelWindow(ctx, staffActor(uid), rid, 120); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("hostess SetFreeCancelWindow: got %v, want ErrForbidden", err)
	}
	if hh.paySet.calls != 0 {
		t.Fatalf("writer reached despite forbidden actor")
	}
}

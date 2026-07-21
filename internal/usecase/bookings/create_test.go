package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// createHarness wires the create usecase over fakes, with a venue that is open
// 12:00–22:00 every day in Asia/Almaty and has one 4-seat and one 2-seat table.
type createHarness struct {
	uc        CreateUseCase
	bookings  *fakeBookings
	links     *fakeLinks
	items     *fakeItems
	history   *fakeHistory
	outbox    *fakeOutbox
	blacklist *fakeBlacklist
	rateLog   *fakeRateLog
	schedule  *fakeSchedule
	tx        *fakeTx

	restaurantID uuid.UUID
	guest        Actor
	manager      Actor
	tableBig     domain.RestaurantTable
	tableSmall   domain.RestaurantTable
	startsAt     time.Time
}

func newCreateHarness(t *testing.T, override domain.BookingPolicyOverride) *createHarness {
	t.Helper()
	loc := mustLoad(t, "Asia/Almaty")
	rid := uuid.New()
	big, small := table("big", 4), table("small", 2)

	h := &createHarness{
		bookings:  newFakeBookings(),
		links:     &fakeLinks{},
		items:     &fakeItems{},
		history:   &fakeHistory{},
		outbox:    &fakeOutbox{},
		blacklist: &fakeBlacklist{},
		rateLog:   &fakeRateLog{},
		schedule: &fakeSchedule{
			hours:  openAllWeek("12:00", "22:00"),
			tables: []domain.RestaurantTable{big, small},
		},
		tx:           &fakeTx{},
		restaurantID: rid,
		guest:        Actor{UserID: uuid.New(), Role: domain.RoleUser},
		manager:      Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		tableBig:     big,
		tableSmall:   small,
	}
	day := time.Now().In(loc).AddDate(0, 0, 2)
	h.startsAt = time.Date(day.Year(), day.Month(), day.Day(), 13, 0, 0, 0, loc).UTC()

	h.uc = NewCreateUseCase(
		h.bookings, h.links, h.items, h.history, h.outbox, h.blacklist, h.rateLog,
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{
			ID: rid, IsActive: true, BookingPolicy: override,
		}}},
		h.schedule,
		newFakeManagers([2]uuid.UUID{h.manager.UserID, rid}),
		h.tx, testConfig(),
	)
	return h
}

func (h *createHarness) input() CreateInput {
	uid := h.guest.UserID
	return CreateInput{
		RestaurantID: h.restaurantID, UserID: &uid, Name: "Дамир",
		Phone: "8 (707) 123-45-67", Email: "  Damir@Example.COM ", Guests: 2,
		StartsAt: h.startsAt, Source: domain.SourceApp,
	}
}

func TestCreateAutoConfirm(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})

	got, err := h.uc.Create(context.Background(), h.guest, h.input())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Booking.Status != domain.BookingConfirmed || got.Booking.ConfirmedAt == nil {
		t.Fatalf("auto-confirm did not apply: %+v", got.Booking)
	}
	// Contacts normalized.
	if got.Booking.PhoneNormalized != "+77071234567" || got.Booking.Email != "damir@example.com" {
		t.Fatalf("contacts not normalized: %+v", got.Booking)
	}
	if got.Booking.Phone != "8 (707) 123-45-67" {
		t.Fatalf("raw phone must be preserved, got %q", got.Booking.Phone)
	}
	// Duration and buffered slot.
	if got.Booking.EndsAt.Sub(got.Booking.StartsAt) != 2*time.Hour {
		t.Fatalf("duration = %v", got.Booking.EndsAt.Sub(got.Booking.StartsAt))
	}
	if len(h.links.created) != 1 {
		t.Fatalf("got %d table links, want 1", len(h.links.created))
	}
	l := h.links.created[0]
	if l.TableID != h.tableSmall.ID {
		t.Fatalf("party of 2 seated at the wrong table")
	}
	if !l.SlotStart.Equal(got.Booking.StartsAt.Add(-15*time.Minute)) ||
		!l.SlotEnd.Equal(got.Booking.EndsAt.Add(15*time.Minute)) {
		t.Fatalf("slot does not include the buffer: %v..%v", l.SlotStart, l.SlotEnd)
	}
	// Two transitions recorded: created(pending) + auto-confirm.
	if len(h.history.created) != 2 || h.history.created[1].ActorType != domain.ActorSystem {
		t.Fatalf("history = %+v", h.history.created)
	}
	if h.history.created[0].FromStatus != nil || h.history.created[0].ToStatus != domain.BookingPending {
		t.Fatalf("first history row = %+v", h.history.created[0])
	}
	want := []domain.BookingEventType{domain.EventBookingCreated, domain.EventBookingConfirmed}
	if got := h.outbox.types(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("outbox = %v, want %v", got, want)
	}
	if h.tx.calls != 1 {
		t.Fatalf("mutations must happen in exactly one transaction, got %d", h.tx.calls)
	}
	if len(h.rateLog.entries) != 1 || *h.rateLog.entries[0].PhoneNormalized != "+77071234567" {
		t.Fatalf("rate log = %+v", h.rateLog.entries)
	}
}

func TestCreateWithoutAutoConfirmStaysPending(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{AutoConfirm: bptr(false)})

	got, err := h.uc.Create(context.Background(), h.guest, h.input())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Booking.Status != domain.BookingPending {
		t.Fatalf("status = %s, want pending", got.Booking.Status)
	}
	if len(h.history.created) != 1 || len(h.outbox.created) != 1 {
		t.Fatalf("expected exactly one transition, got %d history / %d events",
			len(h.history.created), len(h.outbox.created))
	}
}

func TestCreateWithItems(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	in := h.input()
	in.Items = []ItemInput{{Name: "Плов", PriceMinor: 350000, Quantity: 2}}

	got, err := h.uc.Create(context.Background(), h.guest, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Currency != "KZT" ||
		got.Items[0].Status != domain.BookingItemPending || got.Items[0].TotalMinor() != 700000 {
		t.Fatalf("items = %+v", got.Items)
	}
}

// Every rejection branch of Create, in the order the usecase applies them.
func TestCreateRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(h *createHarness, in *CreateInput)
		actor   func(h *createHarness) Actor
		wantErr error
	}{
		{
			name:    "no guests",
			mutate:  func(_ *createHarness, in *CreateInput) { in.Guests = 0 },
			wantErr: domain.ErrValidation,
		},
		{
			name:    "unknown source",
			mutate:  func(_ *createHarness, in *CreateInput) { in.Source = "carrier-pigeon" },
			wantErr: domain.ErrValidation,
		},
		{
			name:    "more guests than the policy allows",
			mutate:  func(_ *createHarness, in *CreateInput) { in.Guests = 21 },
			wantErr: domain.ErrValidation,
		},
		{
			name:    "unusable phone",
			mutate:  func(_ *createHarness, in *CreateInput) { in.Phone = "no digits" },
			wantErr: domain.ErrValidation,
		},
		{
			name: "blacklisted guest",
			mutate: func(h *createHarness, _ *CreateInput) {
				h.blacklist.match = &domain.BlacklistEntry{ID: uuid.New(), IsActive: true}
			},
			wantErr: domain.ErrForbidden,
		},
		{
			name:    "anti-fraud limit reached",
			mutate:  func(h *createHarness, _ *CreateInput) { h.rateLog.count = 10 },
			wantErr: domain.ErrValidation,
		},
		{
			name: "inside the lead time",
			mutate: func(h *createHarness, in *CreateInput) {
				in.StartsAt = time.Now().Add(10 * time.Minute)
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "beyond the horizon",
			mutate: func(h *createHarness, in *CreateInput) {
				in.StartsAt = h.startsAt.AddDate(0, 0, 90)
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "outside opening hours",
			mutate: func(h *createHarness, in *CreateInput) {
				in.StartsAt = in.StartsAt.Add(-6 * time.Hour) // 07:00 local
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "not a slot start",
			mutate: func(h *createHarness, in *CreateInput) {
				in.StartsAt = in.StartsAt.Add(7 * time.Minute)
			},
			wantErr: domain.ErrValidation,
		},
		{
			name: "all tables occupied",
			mutate: func(h *createHarness, in *CreateInput) {
				h.links.busy = []domain.TableBusyInterval{
					{TableID: h.tableBig.ID, From: h.startsAt, To: h.startsAt.Add(time.Hour)},
					{TableID: h.tableSmall.ID, From: h.startsAt, To: h.startsAt.Add(time.Hour)},
				}
			},
			wantErr: domain.ErrAlreadyExists,
		},
		{
			name:    "guest may not force placement",
			mutate:  func(_ *createHarness, in *CreateInput) { in.Force = true },
			wantErr: domain.ErrForbidden,
		},
		{
			name: "guest may not pin tables",
			mutate: func(h *createHarness, in *CreateInput) {
				in.TableIDs = []uuid.UUID{h.tableBig.ID}
			},
			wantErr: domain.ErrForbidden,
		},
		{
			name: "table of another restaurant",
			mutate: func(h *createHarness, in *CreateInput) {
				in.TableIDs = []uuid.UUID{uuid.New()}
			},
			actor:   func(h *createHarness) Actor { return h.manager },
			wantErr: domain.ErrValidation,
		},
		{
			name: "pinned tables cannot seat the party",
			mutate: func(h *createHarness, in *CreateInput) {
				in.Guests = 4
				in.TableIDs = []uuid.UUID{h.tableSmall.ID}
			},
			actor:   func(h *createHarness) Actor { return h.manager },
			wantErr: domain.ErrValidation,
		},
		{
			name: "manager of another restaurant may not force",
			mutate: func(h *createHarness, in *CreateInput) {
				in.Force = true
			},
			actor: func(h *createHarness) Actor {
				return Actor{UserID: uuid.New(), Role: domain.RoleRestaurant}
			},
			wantErr: domain.ErrForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCreateHarness(t, domain.BookingPolicyOverride{})
			in := h.input()
			actor := h.guest
			if tc.actor != nil {
				actor = tc.actor(h)
			}
			tc.mutate(h, &in)

			_, err := h.uc.Create(context.Background(), actor, in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Create = %v, want %v", err, tc.wantErr)
			}
			if len(h.bookings.created) != 0 {
				t.Fatalf("a rejected request must not create a booking")
			}
		})
	}
}

// The blacklist must be queried with the normalized contacts — matching raw
// input would never hit the stop list.
func TestCreateBlacklistQueryIsNormalized(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	if _, err := h.uc.Create(context.Background(), h.guest, h.input()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	q := h.blacklist.lastQry
	if q.PhoneNormalized != "+77071234567" || q.Email != "damir@example.com" {
		t.Fatalf("blacklist query = %+v", q)
	}
	if q.RestaurantID == nil || *q.RestaurantID != h.restaurantID {
		t.Fatalf("blacklist query must be venue-scoped: %+v", q)
	}
}

// force=true skips table selection only: the booking is recorded as unassigned
// seating, and the blacklist still applies.
func TestCreateForcedPlacement(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	h.links.busy = []domain.TableBusyInterval{
		{TableID: h.tableBig.ID, From: h.startsAt, To: h.startsAt.Add(time.Hour)},
		{TableID: h.tableSmall.ID, From: h.startsAt, To: h.startsAt.Add(time.Hour)},
	}
	in := h.input()
	in.Force = true

	got, err := h.uc.Create(context.Background(), h.manager, in)
	if err != nil {
		t.Fatalf("forced create: %v", err)
	}
	if !got.Booking.ForcedPlacement || !got.Booking.CreatedByAdmin {
		t.Fatalf("booking = %+v", got.Booking)
	}
	if len(h.links.created) != 0 {
		t.Fatalf("forced placement must not create table links, got %d", len(h.links.created))
	}

	// …but a blacklisted guest is still refused, force or not.
	h2 := newCreateHarness(t, domain.BookingPolicyOverride{})
	h2.blacklist.match = &domain.BlacklistEntry{ID: uuid.New(), IsActive: true}
	in2 := h2.input()
	in2.Force = true
	if _, err := h2.uc.Create(context.Background(), h2.manager, in2); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("forced create for a blacklisted guest = %v, want ErrForbidden", err)
	}
}

// Manual placement by a manager pins the requested tables.
func TestCreateManualPlacement(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	in := h.input()
	in.Guests = 5
	in.TableIDs = []uuid.UUID{h.tableBig.ID, h.tableSmall.ID}
	in.Source = domain.SourceAdmin

	got, err := h.uc.Create(context.Background(), h.manager, in)
	if err != nil {
		t.Fatalf("manual create: %v", err)
	}
	if len(h.links.created) != 2 {
		t.Fatalf("got %d links, want 2", len(h.links.created))
	}
	if got.Booking.Source != domain.SourceAdmin {
		t.Fatalf("source = %s", got.Booking.Source)
	}
}

// A slot lost to a concurrent request surfaces as ErrAlreadyExists (409), not
// as a raw exclusion-violation error.
func TestCreateLosesRace(t *testing.T) {
	h := newCreateHarness(t, domain.BookingPolicyOverride{})
	h.links.createErr = domain.ErrAlreadyExists

	_, err := h.uc.Create(context.Background(), h.guest, h.input())
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("Create = %v, want ErrAlreadyExists", err)
	}
	if err.Error() == domain.ErrAlreadyExists.Error() {
		t.Fatal("expected a message explaining the slot is taken")
	}
}

func TestCreateInactiveRestaurant(t *testing.T) {
	rid := uuid.New()
	uc := NewCreateUseCase(
		newFakeBookings(), &fakeLinks{}, &fakeItems{}, &fakeHistory{}, &fakeOutbox{},
		&fakeBlacklist{}, &fakeRateLog{},
		&fakeRestaurants{agg: &domain.RestaurantAggregate{Restaurant: domain.Restaurant{ID: rid}}},
		&fakeSchedule{}, newFakeManagers(), &fakeTx{}, testConfig(),
	)
	_, err := uc.Create(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser}, CreateInput{
		RestaurantID: rid, Name: "x", Phone: "+77071234567", Guests: 2,
		StartsAt: time.Now().Add(48 * time.Hour), Source: domain.SourceApp,
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("inactive restaurant = %v, want ErrForbidden", err)
	}
}

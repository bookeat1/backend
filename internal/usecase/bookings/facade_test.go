package bookings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type facadeHarness struct {
	f        Facade
	bookings *fakeBookings
	links    *fakeLinks
	items    *fakeItems
	messages *fakeMessages
	surveys  *fakeSurveys
	history  *fakeHistory
	outbox   *fakeOutbox
	tx       *fakeTx

	restaurantID uuid.UUID
	booking      *domain.Booking
	guest        Actor
	manager      Actor
	admin        Actor
	outsider     Actor
	otherManager Actor
}

func newFacadeHarness(t *testing.T, status domain.BookingStatus) *facadeHarness {
	t.Helper()
	rid := uuid.New()
	guestID := uuid.New()
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Name: "Дамир",
		Guests: 2, Status: status, StartsAt: time.Now().Add(24 * time.Hour),
		EndsAt: time.Now().Add(26 * time.Hour), Source: domain.SourceApp,
	}
	h := &facadeHarness{
		bookings: newFakeBookings(b), links: &fakeLinks{}, items: &fakeItems{},
		messages: &fakeMessages{}, surveys: &fakeSurveys{}, history: &fakeHistory{},
		outbox: &fakeOutbox{}, tx: &fakeTx{},
		restaurantID: rid, booking: b,
		guest:        Actor{UserID: guestID, Role: domain.RoleUser},
		manager:      Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		admin:        Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		outsider:     Actor{UserID: uuid.New(), Role: domain.RoleUser},
		otherManager: Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
	}
	h.f = NewFacade(h.bookings, h.links, h.items, h.messages, h.surveys,
		h.history, h.outbox, newFakeManagers([2]uuid.UUID{h.manager.UserID, rid}), h.tx)
	return h
}

func TestFacadeGetAccess(t *testing.T) {
	cases := []struct {
		name    string
		actor   func(h *facadeHarness) Actor
		wantErr error
	}{
		{"owner", func(h *facadeHarness) Actor { return h.guest }, nil},
		{"manager of the venue", func(h *facadeHarness) Actor { return h.manager }, nil},
		{"admin", func(h *facadeHarness) Actor { return h.admin }, nil},
		{"another guest", func(h *facadeHarness) Actor { return h.outsider }, domain.ErrNotFound},
		{"manager of another venue", func(h *facadeHarness) Actor { return h.otherManager }, domain.ErrForbidden},
		{"unauthenticated", func(h *facadeHarness) Actor { return Actor{} }, domain.ErrUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFacadeHarness(t, domain.BookingConfirmed)
			_, err := h.f.Get(context.Background(), tc.actor(h), h.booking.ID)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Get = %v, want success", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Get = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestFacadeGetUnknownBooking(t *testing.T) {
	h := newFacadeHarness(t, domain.BookingPending)
	if _, err := h.f.Get(context.Background(), h.guest, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("= %v, want ErrNotFound", err)
	}
}

// ListMine must pin the user filter to the caller — a client-supplied user_id
// may never widen the result set.
func TestFacadeListMinePinsUser(t *testing.T) {
	h := newFacadeHarness(t, domain.BookingPending)
	other := uuid.New()
	rid := h.restaurantID

	if _, _, err := h.f.ListMine(context.Background(), h.guest, domain.BookingFilter{
		UserID: &other, RestaurantID: &rid,
	}); err != nil {
		t.Fatalf("ListMine: %v", err)
	}
	got := h.bookings.lastFlt
	if got.UserID == nil || *got.UserID != h.guest.UserID || got.RestaurantID != nil {
		t.Fatalf("filter = %+v", got)
	}
	if _, _, err := h.f.ListMine(context.Background(), Actor{}, domain.BookingFilter{}); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("anonymous ListMine must be rejected")
	}
}

func TestFacadeListByRestaurant(t *testing.T) {
	h := newFacadeHarness(t, domain.BookingPending)
	from := time.Now()
	to := from.Add(24 * time.Hour)
	flt := domain.BookingFilter{
		Statuses: []domain.BookingStatus{domain.BookingPending}, From: &from, To: &to,
		Page: 2, PerPage: 50,
	}

	if _, _, err := h.f.ListByRestaurant(context.Background(), h.manager, h.restaurantID, flt); err != nil {
		t.Fatalf("ListByRestaurant: %v", err)
	}
	got := h.bookings.lastFlt
	if got.RestaurantID == nil || *got.RestaurantID != h.restaurantID || got.UserID != nil {
		t.Fatalf("filter = %+v", got)
	}
	if got.Page != 2 || got.PerPage != 50 || len(got.Statuses) != 1 {
		t.Fatalf("filter lost its narrowing: %+v", got)
	}

	// Foreign manager and plain guest → 403.
	for _, actor := range []Actor{h.otherManager, h.guest} {
		if _, _, err := h.f.ListByRestaurant(context.Background(), actor, h.restaurantID, flt); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("actor %v = %v, want ErrForbidden", actor.Role, err)
		}
	}
	// Admin sees any venue.
	if _, _, err := h.f.ListByRestaurant(context.Background(), h.admin, h.restaurantID, flt); err != nil {
		t.Fatalf("admin listing: %v", err)
	}
	// Bad filters are rejected before the query.
	bad := domain.BookingFilter{Statuses: []domain.BookingStatus{"nonsense"}}
	if _, _, err := h.f.ListByRestaurant(context.Background(), h.manager, h.restaurantID, bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown status = %v, want ErrValidation", err)
	}
	inverted := domain.BookingFilter{From: &to, To: &from}
	if _, _, err := h.f.ListByRestaurant(context.Background(), h.manager, h.restaurantID, inverted); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("inverted range = %v, want ErrValidation", err)
	}
}

func TestFacadeMessages(t *testing.T) {
	h := newFacadeHarness(t, domain.BookingConfirmed)

	m, err := h.f.PostMessage(context.Background(), h.guest, h.booking.ID, "  во сколько кухня закрывается? ")
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if m.SenderType != domain.SenderGuest || m.Message != "во сколько кухня закрывается?" {
		t.Fatalf("message = %+v", m)
	}
	if h.tx.calls != 1 || len(h.outbox.created) != 1 || h.outbox.types()[0] != domain.EventBookingMessage {
		t.Fatalf("message must be published transactionally: tx=%d outbox=%v", h.tx.calls, h.outbox.types())
	}

	// The sender type comes from the caller's relation, never from the body.
	sm, err := h.f.PostMessage(context.Background(), h.manager, h.booking.ID, "до 23:00")
	if err != nil {
		t.Fatalf("PostMessage(manager): %v", err)
	}
	if sm.SenderType != domain.SenderRestaurant {
		t.Fatalf("manager sender type = %s", sm.SenderType)
	}

	if _, err := h.f.PostMessage(context.Background(), h.guest, h.booking.ID, "   "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty message = %v, want ErrValidation", err)
	}
	if _, err := h.f.PostMessage(context.Background(), h.outsider, h.booking.ID, "hi"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider message = %v, want ErrNotFound", err)
	}
	if _, err := h.f.Messages(context.Background(), h.otherManager, h.booking.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("foreign manager thread = %v, want ErrForbidden", err)
	}
	got, err := h.f.Messages(context.Background(), h.guest, h.booking.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("Messages = %d, %v", len(got), err)
	}
	if _, err := h.f.MarkMessagesRead(context.Background(), h.manager, h.booking.ID); err != nil {
		t.Fatalf("MarkMessagesRead: %v", err)
	}
	if h.messages.readReader != domain.SenderRestaurant {
		t.Fatalf("reader = %s", h.messages.readReader)
	}
}

func TestFacadeSurvey(t *testing.T) {
	valid := SurveyInput{RatingOverall: 5, RatingFood: 5, RatingService: 4, RatingAmbience: 4, NPS: 9}

	t.Run("guest rates a completed visit", func(t *testing.T) {
		h := newFacadeHarness(t, domain.BookingCompleted)
		s, err := h.f.SubmitSurvey(context.Background(), h.guest, h.booking.ID, valid)
		if err != nil {
			t.Fatalf("SubmitSurvey: %v", err)
		}
		if s.UserID != h.guest.UserID || s.RestaurantID != h.restaurantID || *s.BookingID != h.booking.ID {
			t.Fatalf("survey = %+v", s)
		}
	})
	t.Run("visit has not happened yet", func(t *testing.T) {
		h := newFacadeHarness(t, domain.BookingConfirmed)
		if _, err := h.f.SubmitSurvey(context.Background(), h.guest, h.booking.ID, valid); !errors.Is(err, domain.ErrInvalidStatus) {
			t.Fatalf("= %v, want ErrInvalidStatus", err)
		}
	})
	t.Run("staff may not rate on the guest's behalf", func(t *testing.T) {
		h := newFacadeHarness(t, domain.BookingCompleted)
		if _, err := h.f.SubmitSurvey(context.Background(), h.manager, h.booking.ID, valid); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("= %v, want ErrForbidden", err)
		}
	})
	t.Run("out-of-range scores", func(t *testing.T) {
		h := newFacadeHarness(t, domain.BookingCompleted)
		bad := valid
		bad.RatingFood = 6
		if _, err := h.f.SubmitSurvey(context.Background(), h.guest, h.booking.ID, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("rating 6 = %v, want ErrValidation", err)
		}
		bad = valid
		bad.NPS = 11
		if _, err := h.f.SubmitSurvey(context.Background(), h.guest, h.booking.ID, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("nps 11 = %v, want ErrValidation", err)
		}
	})
	t.Run("reading a survey is access-checked", func(t *testing.T) {
		h := newFacadeHarness(t, domain.BookingCompleted)
		if _, err := h.f.Survey(context.Background(), h.outsider, h.booking.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("= %v, want ErrNotFound", err)
		}
	})
}

func TestFacadeHistoryAccess(t *testing.T) {
	h := newFacadeHarness(t, domain.BookingConfirmed)
	if _, err := h.f.History(context.Background(), h.outsider, h.booking.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("= %v, want ErrNotFound", err)
	}
	if _, err := h.f.History(context.Background(), h.manager, h.booking.ID); err != nil {
		t.Fatalf("manager history: %v", err)
	}
}

// fakeWindowResolver computes starts_at − window, standing in for the
// per-restaurant free_cancel_window_minutes resolution.
type fakeWindowResolver struct{ window time.Duration }

func (r fakeWindowResolver) CancelDeadlineFor(_ context.Context, b domain.Booking) (time.Time, error) {
	return b.StartsAt.Add(-r.window), nil
}

// Item 4: the guest booking detail exposes free_cancel_deadline = start − the
// restaurant's window, and two restaurants with different windows yield
// different deadlines for the same booking start.
func TestFacadeGet_FreeCancelDeadline(t *testing.T) {
	rid, guestID := uuid.New(), uuid.New()
	start := time.Now().Add(24 * time.Hour)
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Guests: 2,
		Status: domain.BookingConfirmed, StartsAt: start, EndsAt: start.Add(2 * time.Hour), Source: domain.SourceApp,
	}
	guest := Actor{UserID: guestID, Role: domain.RoleUser}
	build := func(window time.Duration) Facade {
		return NewFacade(newFakeBookings(b), &fakeLinks{}, &fakeItems{}, &fakeMessages{}, &fakeSurveys{},
			&fakeHistory{}, &fakeOutbox{}, newFakeManagers([2]uuid.UUID{uuid.New(), rid}), &fakeTx{},
			WithFreeCancelDeadlineResolver(fakeWindowResolver{window: window}))
	}

	d1, err := build(120*time.Minute).Get(context.Background(), guest, b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d1.FreeCancelDeadline == nil {
		t.Fatalf("free_cancel_deadline missing for an active booking")
	}
	if !d1.FreeCancelDeadline.Equal(start.Add(-120 * time.Minute)) {
		t.Fatalf("deadline = %v, want start−120m %v", d1.FreeCancelDeadline, start.Add(-120*time.Minute))
	}

	d2, err := build(300*time.Minute).Get(context.Background(), guest, b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !d2.FreeCancelDeadline.Equal(start.Add(-300 * time.Minute)) {
		t.Fatalf("deadline = %v, want start−300m", d2.FreeCancelDeadline)
	}
	if d2.FreeCancelDeadline.Equal(*d1.FreeCancelDeadline) {
		t.Fatalf("two different restaurant windows produced the SAME deadline")
	}
}

// A terminal booking (past cancellation) has no countdown.
func TestFacadeGet_FreeCancelDeadline_NilForTerminal(t *testing.T) {
	rid, guestID := uuid.New(), uuid.New()
	start := time.Now().Add(24 * time.Hour)
	b := &domain.Booking{
		ID: uuid.New(), RestaurantID: rid, UserID: &guestID, Guests: 2,
		Status: domain.BookingCancelled, StartsAt: start, EndsAt: start.Add(2 * time.Hour), Source: domain.SourceApp,
	}
	f := NewFacade(newFakeBookings(b), &fakeLinks{}, &fakeItems{}, &fakeMessages{}, &fakeSurveys{},
		&fakeHistory{}, &fakeOutbox{}, newFakeManagers([2]uuid.UUID{uuid.New(), rid}), &fakeTx{},
		WithFreeCancelDeadlineResolver(fakeWindowResolver{window: 120 * time.Minute}))
	got, err := f.Get(context.Background(), Actor{UserID: guestID, Role: domain.RoleUser}, b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.FreeCancelDeadline != nil {
		t.Fatalf("terminal booking should have no free_cancel_deadline, got %v", *got.FreeCancelDeadline)
	}
}

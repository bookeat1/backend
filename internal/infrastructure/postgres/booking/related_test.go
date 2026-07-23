package booking

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

func seedMenuItem(t *testing.T, pool *pgxpool.Pool, rid uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO menu_items (id, restaurant_id, name, price) VALUES ($1,$2,'Плов',3500.00)`,
		id, rid); err != nil {
		t.Fatalf("seed menu item: %v", err)
	}
	return id
}

func TestBookingItems(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	mid := seedMenuItem(t, pool, rid)
	repo := NewItems(pool)
	txm := sqltx.NewManager(pool)

	b := newBooking(rid, time.Now().Add(24*time.Hour))
	if err := New(pool).Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}

	items := []domain.BookingItem{
		{BookingID: b.ID, MenuItemID: &mid, ItemName: "Плов", PriceMinor: 350000, Quantity: 2},
		{BookingID: b.ID, ItemName: "Чай", PriceMinor: 90000, Quantity: 1, Comment: ptr("без сахара")},
	}
	if err := repo.Create(ctx, items); err != nil {
		t.Fatalf("create items: %v", err)
	}

	got, err := repo.ListByBooking(ctx, b.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("list = %d rows err=%v", len(got), err)
	}
	byName := map[string]domain.BookingItem{}
	for _, i := range got {
		byName[i.ItemName] = i
	}
	plov, tea := byName["Плов"], byName["Чай"]
	if plov.Currency != "KZT" || plov.Status != domain.BookingItemPending {
		t.Errorf("defaults not applied: %+v", plov)
	}
	if plov.TotalMinor() != 700000 {
		t.Errorf("total = %d, want 700000", plov.TotalMinor())
	}
	if plov.MenuItemID == nil || *plov.MenuItemID != mid {
		t.Errorf("menu_item_id not persisted: %+v", plov)
	}
	if tea.MenuItemID != nil || tea.Comment == nil || *tea.Comment != "без сахара" {
		t.Errorf("free-form line mismatch: %+v", tea)
	}

	if err := repo.SetStatus(ctx, plov.ID, domain.BookingItemConfirmed); err != nil {
		t.Fatalf("set status: %v", err)
	}
	got, _ = repo.ListByBooking(ctx, b.ID)
	for _, i := range got {
		if i.ID == plov.ID && i.Status != domain.BookingItemConfirmed {
			t.Errorf("status = %q, want confirmed", i.Status)
		}
	}
	if err := repo.SetStatus(ctx, uuid.New(), domain.BookingItemServed); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("set status(missing) = %v, want ErrNotFound", err)
	}

	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		return repo.ReplaceForBooking(ctx, b.ID, []domain.BookingItem{
			{ItemName: "Лагман", PriceMinor: 420000, Quantity: 1},
		})
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = repo.ListByBooking(ctx, b.ID)
	if len(got) != 1 || got[0].ItemName != "Лагман" || got[0].BookingID != b.ID {
		t.Errorf("replace mismatch: %+v", got)
	}
}

func TestBookingMessages(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	repo := NewMessages(pool)

	b := newBooking(rid, time.Now().Add(24*time.Hour))
	b.UserID = &uid
	if err := New(pool).Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}

	guest := &domain.BookingMessage{BookingID: b.ID, SenderType: domain.SenderGuest,
		SenderID: &uid, Message: "Можно столик у окна?"}
	if err := repo.Create(ctx, guest); err != nil {
		t.Fatalf("create guest message: %v", err)
	}
	venue := &domain.BookingMessage{BookingID: b.ID, SenderType: domain.SenderRestaurant,
		Message: "Да, забронировали", CreatedAt: time.Now().Add(time.Minute)}
	if err := repo.Create(ctx, venue); err != nil {
		t.Fatalf("create venue message: %v", err)
	}

	thread, err := repo.ListByBooking(ctx, b.ID)
	if err != nil || len(thread) != 2 {
		t.Fatalf("list = %d rows err=%v", len(thread), err)
	}
	if thread[0].ID != guest.ID || thread[1].ID != venue.ID {
		t.Error("thread is not ordered by created_at ascending")
	}

	at := time.Now()
	n, err := repo.MarkRead(ctx, b.ID, domain.SenderGuest, at)
	if err != nil || n != 1 {
		t.Fatalf("mark read = %d err=%v, want 1", n, err)
	}
	thread, _ = repo.ListByBooking(ctx, b.ID)
	if thread[0].IsRead {
		t.Error("guest's own message must not be marked read")
	}
	if !thread[1].IsRead || thread[1].ReadAt == nil {
		t.Error("venue message should be read")
	}

	// Idempotent: nothing left unread.
	if n, _ := repo.MarkRead(ctx, b.ID, domain.SenderGuest, at); n != 0 {
		t.Errorf("second mark read = %d, want 0", n)
	}
}

func TestBookingBlacklistAndRateLog(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	other := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	repo := NewBlacklist(pool)

	globalEntry := &domain.BlacklistEntry{
		PhoneNormalized: ptr("+77770000001"), Reason: ptr("фрод"), IsActive: true,
	}
	if err := repo.Create(ctx, globalEntry); err != nil {
		t.Fatalf("create global: %v", err)
	}
	scoped := &domain.BlacklistEntry{
		RestaurantID: &rid, PhoneNormalized: ptr("+77770000002"), IsActive: true, CreatedBy: &uid,
	}
	if err := repo.Create(ctx, scoped); err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	if err := repo.Create(ctx, &domain.BlacklistEntry{
		RestaurantID: &rid, PhoneNormalized: ptr("+77770000002"), IsActive: true,
	}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("duplicate active entry = %v, want ErrAlreadyExists", err)
	}

	// Global entry hits at any venue.
	m, err := repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &other, PhoneNormalized: "+77770000001"})
	if err != nil || m == nil || m.ID != globalEntry.ID {
		t.Fatalf("global match = %+v err=%v", m, err)
	}
	// Venue-scoped entry only at its own venue.
	m, err = repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &other, PhoneNormalized: "+77770000002"})
	if err != nil || m != nil {
		t.Fatalf("scoped entry leaked to another venue: %+v err=%v", m, err)
	}
	m, err = repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &rid, PhoneNormalized: "+77770000002"})
	if err != nil || m == nil || m.ID != scoped.ID {
		t.Fatalf("scoped match = %+v err=%v", m, err)
	}
	// An empty phone/email must not match rows with NULL identifiers.
	m, err = repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &rid})
	if err != nil || m != nil {
		t.Fatalf("empty query matched %+v err=%v", m, err)
	}
	// Clean guest.
	m, _ = repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &rid, PhoneNormalized: "+77779999999"})
	if m != nil {
		t.Errorf("unexpected match: %+v", m)
	}

	list, err := repo.ListByRestaurant(ctx, rid)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d rows err=%v, want venue + global", len(list), err)
	}

	if err := repo.Deactivate(ctx, scoped.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	m, _ = repo.Match(ctx, domain.BlacklistQuery{RestaurantID: &rid, PhoneNormalized: "+77770000002"})
	if m != nil {
		t.Errorf("deactivated entry still matches: %+v", m)
	}
	if err := repo.Deactivate(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("deactivate(missing) = %v, want ErrNotFound", err)
	}
	// The partial unique index frees the slot once the entry is inactive.
	if err := repo.Create(ctx, &domain.BlacklistEntry{
		RestaurantID: &rid, PhoneNormalized: ptr("+77770000002"), IsActive: true,
	}); err != nil {
		t.Errorf("re-blacklist after deactivate: %v", err)
	}

	rates := NewRateLog(pool)
	phone := "+77771234567"
	for i := 0; i < 3; i++ {
		if err := rates.Create(ctx, &domain.BookingRateLogEntry{
			UserID: &uid, PhoneNormalized: &phone, RestaurantID: &rid,
			Action: domain.RateLogCreate, CreatedAt: time.Now().Add(-time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("rate log create: %v", err)
		}
	}
	if err := rates.Create(ctx, &domain.BookingRateLogEntry{
		PhoneNormalized: &phone, Action: domain.RateLogCancel}); err != nil {
		t.Fatalf("rate log cancel: %v", err)
	}

	n, err := rates.CountSince(ctx, phone, domain.RateLogCreate, time.Now().Add(-90*time.Minute))
	if err != nil || n != 2 {
		t.Errorf("count since 90m = %d err=%v, want 2", n, err)
	}
	n, _ = rates.CountSince(ctx, phone, domain.RateLogCancel, time.Now().Add(-90*time.Minute))
	if n != 1 {
		t.Errorf("count cancel = %d, want 1", n)
	}
	n, _ = rates.CountSince(ctx, "+70000000000", domain.RateLogCreate, time.Now().Add(-24*time.Hour))
	if n != 0 {
		t.Errorf("count other phone = %d, want 0", n)
	}
}

func TestRestaurantSurveys(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	uid := seedUser(t, pool)
	repo := NewSurveys(pool)

	b := newBooking(rid, time.Now().Add(-24*time.Hour))
	b.UserID = &uid
	if err := New(pool).Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}

	s := &domain.RestaurantSurvey{
		BookingID: &b.ID, RestaurantID: rid, UserID: uid, RatingOverall: 5, RatingFood: 5,
		RatingService: 4, RatingAmbience: 5, NPS: 9, Comment: ptr("Отлично"),
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, &domain.RestaurantSurvey{
		BookingID: &b.ID, RestaurantID: rid, UserID: uid, RatingOverall: 3, RatingFood: 3,
		RatingService: 3, RatingAmbience: 3, NPS: 5,
	}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second survey for booking = %v, want ErrAlreadyExists", err)
	}

	got, err := repo.GetByBooking(ctx, b.ID)
	if err != nil || got.RatingService != 4 || got.NPS != 9 || got.Comment == nil {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	if _, err := repo.GetByBooking(ctx, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get(missing) = %v, want ErrNotFound", err)
	}

	// A survey without a booking is allowed (guest rates the place directly).
	if err := repo.Create(ctx, &domain.RestaurantSurvey{
		RestaurantID: rid, UserID: uid, RatingOverall: 4, RatingFood: 4,
		RatingService: 4, RatingAmbience: 4, NPS: 8,
	}); err != nil {
		t.Fatalf("bookingless survey: %v", err)
	}

	list, err := repo.ListByRestaurant(ctx, rid, 0, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d rows err=%v", len(list), err)
	}
	page1, _ := repo.ListByRestaurant(ctx, rid, 1, 0)
	page2, _ := repo.ListByRestaurant(ctx, rid, 1, 1)
	if len(page1) != 1 || len(page2) != 1 || page1[0].ID == page2[0].ID {
		t.Errorf("pagination broken: %+v %+v", page1, page2)
	}
}

func TestBookingStatusHistoryAndOutboxShareTx(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bookings := New(pool)
	history := NewHistory(pool)
	outbox := NewOutbox(pool)
	txm := sqltx.NewManager(pool)

	b := newBooking(rid, time.Now().Add(24*time.Hour))
	if err := bookings.Create(ctx, b); err != nil {
		t.Fatalf("create booking: %v", err)
	}

	// The three writes of a status transition roll back together.
	boom := errors.New("boom")
	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := bookings.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, time.Now()); err != nil {
			return err
		}
		if err := history.Create(ctx, &domain.BookingStatusChange{
			BookingID: b.ID, FromStatus: ptr(domain.BookingPending),
			ToStatus: domain.BookingConfirmed, ActorType: domain.ActorSystem,
		}); err != nil {
			return err
		}
		if err := outbox.Create(ctx, &domain.BookingOutboxEvent{
			BookingID: b.ID, EventType: domain.EventBookingConfirmed,
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("tx error = %v, want boom", err)
	}
	got, _ := bookings.GetByID(ctx, b.ID)
	if got.Status != domain.BookingPending {
		t.Errorf("status survived the rollback: %q", got.Status)
	}
	if trail, _ := history.ListByBooking(ctx, b.ID); len(trail) != 0 {
		t.Errorf("history survived the rollback: %+v", trail)
	}

	// Now the committing path.
	payload := json.RawMessage(`{"booking_id":"x","guests":2}`)
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := bookings.UpdateStatus(ctx, b.ID, domain.BookingConfirmed, time.Now()); err != nil {
			return err
		}
		if err := history.Create(ctx, &domain.BookingStatusChange{
			BookingID: b.ID, ToStatus: domain.BookingConfirmed,
			ActorType: domain.ActorManager, Reason: ptr("ok"),
		}); err != nil {
			return err
		}
		return outbox.Create(ctx, &domain.BookingOutboxEvent{
			BookingID: b.ID, EventType: domain.EventBookingConfirmed, Payload: payload,
		})
	}); err != nil {
		t.Fatalf("commit path: %v", err)
	}

	trail, err := history.ListByBooking(ctx, b.ID)
	if err != nil || len(trail) != 1 || trail[0].FromStatus != nil ||
		trail[0].ToStatus != domain.BookingConfirmed || trail[0].ActorType != domain.ActorManager {
		t.Fatalf("history = %+v err=%v", trail, err)
	}

	var claimed []domain.BookingOutboxEvent
	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		claimed, err = outbox.ClaimUnpublished(ctx, 10)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			t.Fatalf("claim = %d events, want 1", len(claimed))
		}
		// jsonb normalizes key order, so compare the decoded value.
		var want, have map[string]any
		_ = json.Unmarshal(payload, &want)
		if err := json.Unmarshal(claimed[0].Payload, &have); err != nil {
			t.Errorf("payload is not valid json: %v", err)
		}
		if len(have) != len(want) || have["booking_id"] != want["booking_id"] || claimed[0].PublishedAt != nil {
			t.Errorf("claimed event mismatch: %+v", claimed[0])
		}
		// A parallel worker must skip the locked row.
		parallel, err := outbox.ClaimUnpublished(context.Background(), 10)
		if err != nil {
			return err
		}
		if len(parallel) != 0 {
			t.Errorf("parallel worker claimed %d locked events, want 0", len(parallel))
		}
		return outbox.MarkPublished(ctx, []uuid.UUID{claimed[0].ID}, time.Now())
	}); err != nil {
		t.Fatalf("claim tx: %v", err)
	}

	if err := txm.WithinTx(ctx, func(ctx context.Context) error {
		left, err := outbox.ClaimUnpublished(ctx, 10)
		if err != nil {
			return err
		}
		if len(left) != 0 {
			t.Errorf("published event re-claimed: %+v", left)
		}
		return nil
	}); err != nil {
		t.Fatalf("recheck tx: %v", err)
	}
	if err := outbox.MarkPublished(ctx, nil, time.Now()); err != nil {
		t.Errorf("mark published(nil) = %v, want nil", err)
	}
}

package notifications

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeFeed is an in-memory domain.NotificationFeedRepository. Insert enforces the
// same (outbox_event_id, user_id) idempotency the Postgres unique key does, so a
// redelivery test proves the no-op without a database.
type fakeFeed struct {
	mu   sync.Mutex
	rows []domain.Notification
}

func newFakeFeed() *fakeFeed { return &fakeFeed{} }

func (f *fakeFeed) Insert(_ context.Context, n *domain.Notification) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.OutboxEventID == n.OutboxEventID && r.UserID == n.UserID {
			return false, nil
		}
	}
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now()
	}
	cp := *n
	f.rows = append(f.rows, cp)
	return true, nil
}

func (f *fakeFeed) ListByUser(_ context.Context, userID uuid.UUID, cursor *domain.NotificationCursor, limit int) ([]domain.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var mine []domain.Notification
	for _, r := range f.rows {
		if r.UserID == userID {
			mine = append(mine, r)
		}
	}
	// newest first, id as tiebreaker
	sort.Slice(mine, func(i, j int) bool {
		if mine[i].CreatedAt.Equal(mine[j].CreatedAt) {
			return mine[i].ID.String() > mine[j].ID.String()
		}
		return mine[i].CreatedAt.After(mine[j].CreatedAt)
	})
	var out []domain.Notification
	for _, r := range mine {
		if cursor != nil {
			after := r.CreatedAt.Before(cursor.CreatedAt) ||
				(r.CreatedAt.Equal(cursor.CreatedAt) && r.ID.String() < cursor.ID.String())
			if !after {
				continue
			}
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeFeed) CountUnread(_ context.Context, userID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.UserID == userID && r.ReadAt == nil {
			n++
		}
	}
	return n, nil
}

func (f *fakeFeed) MarkRead(_ context.Context, id, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id && f.rows[i].UserID == userID {
			if f.rows[i].ReadAt == nil {
				now := time.Now()
				f.rows[i].ReadAt = &now
			}
			return nil
		}
	}
	return domain.ErrNotFound
}

func (f *fakeFeed) MarkAllRead(_ context.Context, userID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for i := range f.rows {
		if f.rows[i].UserID == userID && f.rows[i].ReadAt == nil {
			f.rows[i].ReadAt = &now
		}
	}
	return nil
}

var _ domain.NotificationFeedRepository = (*fakeFeed)(nil)

func newFeedNotifier(feed domain.NotificationFeedRepository) *FeedNotifier {
	return NewFeedNotifier(feed, fakeVenues{name: "Ocean Basket"}, discardLog())
}

func TestFeedNotifierWritesRowPerType(t *testing.T) {
	cases := []struct {
		event     domain.BookingEventType
		wantType  domain.NotificationFeedType
		wantTitle string
	}{
		{domain.EventBookingConfirmed, domain.FeedTypeBooking, "Бронь подтверждена"},
		{domain.EventBookingCancelled, domain.FeedTypeBooking, "Бронь отменена"},
		{domain.EventBookingReminder, domain.FeedTypeReminder, "Напоминание о брони"},
	}
	for _, tc := range cases {
		t.Run(string(tc.event), func(t *testing.T) {
			feed := newFakeFeed()
			n := newFeedNotifier(feed)
			uid := uuid.New()
			if err := n.Notify(context.Background(), guestEvent(uid, tc.event)); err != nil {
				t.Fatalf("notify: %v", err)
			}
			rows, _ := feed.ListByUser(context.Background(), uid, nil, 10)
			if len(rows) != 1 {
				t.Fatalf("want 1 row, got %d", len(rows))
			}
			r := rows[0]
			if r.Type != tc.wantType {
				t.Errorf("type = %q, want %q", r.Type, tc.wantType)
			}
			if r.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", r.Title, tc.wantTitle)
			}
			if r.BookingID == nil || r.RestaurantID == nil {
				t.Errorf("booking/restaurant id must be set: %+v", r)
			}
		})
	}
}

func TestFeedNotifierCancelledNamesWhoCancelled(t *testing.T) {
	feed := newFakeFeed()
	n := newFeedNotifier(feed)
	uid := uuid.New()
	ev := guestEvent(uid, domain.EventBookingCancelled)
	ev.CancelledBy = domain.CancelledByRestaurant
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	rows, _ := feed.ListByUser(context.Background(), uid, nil, 10)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if !strings.Contains(rows[0].Body, "рестораном") {
		t.Errorf("body should name the restaurant as canceller: %q", rows[0].Body)
	}
}

func TestFeedNotifierSkipsWhenNoAccount(t *testing.T) {
	feed := newFakeFeed()
	n := newFeedNotifier(feed)
	ev := guestEvent(uuid.New(), domain.EventBookingConfirmed)
	ev.GuestUserID = nil // phone / admin-entered booking
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if len(feed.rows) != 0 {
		t.Fatalf("no account → no feed row, got %d", len(feed.rows))
	}
}

func TestFeedNotifierIdempotentOnRedelivery(t *testing.T) {
	feed := newFakeFeed()
	n := newFeedNotifier(feed)
	uid := uuid.New()
	ev := guestEvent(uid, domain.EventBookingConfirmed)
	// Same event delivered twice (a sibling channel failed the first tick).
	for i := 0; i < 2; i++ {
		if err := n.Notify(context.Background(), ev); err != nil {
			t.Fatalf("notify %d: %v", i, err)
		}
	}
	rows, _ := feed.ListByUser(context.Background(), uid, nil, 10)
	if len(rows) != 1 {
		t.Fatalf("redelivery must not double-write: got %d rows", len(rows))
	}
}

func TestFeedNotifierInterested(t *testing.T) {
	n := newFeedNotifier(newFakeFeed())
	want := map[domain.BookingEventType]bool{
		domain.EventBookingConfirmed: true,
		domain.EventBookingCancelled: true,
		domain.EventBookingReminder:  true,
		domain.EventBookingCreated:   false,
		domain.EventBookingNoShow:    false,
	}
	for ev, exp := range want {
		if got := n.Interested(ev); got != exp {
			t.Errorf("Interested(%s) = %v, want %v", ev, got, exp)
		}
	}
}

func TestNotificationFeedListUnreadAndPagination(t *testing.T) {
	feed := newFakeFeed()
	uc := NewNotificationFeedUseCase(feed)
	uid := uuid.New()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// 5 rows, ascending created_at so newest is #4.
	for i := 0; i < 5; i++ {
		_, _ = feed.Insert(context.Background(), &domain.Notification{
			UserID:        uid,
			Type:          domain.FeedTypeBooking,
			Title:         "t",
			Body:          "b",
			OutboxEventID: uuid.New(),
			CreatedAt:     base.Add(time.Duration(i) * time.Minute),
		})
	}
	// noise for another user must never leak or count.
	_, _ = feed.Insert(context.Background(), &domain.Notification{
		UserID: uuid.New(), Type: domain.FeedTypeBooking, Title: "x", Body: "x",
		OutboxEventID: uuid.New(), CreatedAt: base,
	})

	page1, err := uc.List(context.Background(), uid, nil, 2)
	if err != nil {
		t.Fatalf("list p1: %v", err)
	}
	if page1.UnreadCount != 5 {
		t.Errorf("unread = %d, want 5", page1.UnreadCount)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page size = %d, want 2", len(page1.Items))
	}
	if page1.Next == nil {
		t.Fatalf("full page must yield a next cursor")
	}
	// newest first
	if !page1.Items[0].CreatedAt.After(page1.Items[1].CreatedAt) {
		t.Errorf("items not newest-first: %v", page1.Items)
	}

	page2, err := uc.List(context.Background(), uid, page1.Next, 2)
	if err != nil {
		t.Fatalf("list p2: %v", err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("page2 size = %d, want 2", len(page2.Items))
	}
	// no overlap between pages
	seen := map[uuid.UUID]bool{page1.Items[0].ID: true, page1.Items[1].ID: true}
	for _, it := range page2.Items {
		if seen[it.ID] {
			t.Errorf("cursor overlap: %s appeared on both pages", it.ID)
		}
	}

	page3, err := uc.List(context.Background(), uid, page2.Next, 2)
	if err != nil {
		t.Fatalf("list p3: %v", err)
	}
	if len(page3.Items) != 1 {
		t.Fatalf("last page size = %d, want 1", len(page3.Items))
	}
	if page3.Next != nil {
		t.Errorf("short page must end pagination, got cursor")
	}
}

func TestNotificationFeedMarkReadScopesToOwner(t *testing.T) {
	feed := newFakeFeed()
	uc := NewNotificationFeedUseCase(feed)
	owner, other := uuid.New(), uuid.New()
	n := &domain.Notification{
		UserID: owner, Type: domain.FeedTypeBooking, Title: "t", Body: "b",
		OutboxEventID: uuid.New(), CreatedAt: time.Now(),
	}
	_, _ = feed.Insert(context.Background(), n)

	// Another guest cannot mark it read — sees a not-found, cannot probe it.
	if err := uc.MarkRead(context.Background(), n.ID, other); err != domain.ErrNotFound {
		t.Fatalf("cross-user mark-read = %v, want ErrNotFound", err)
	}
	if c, _ := feed.CountUnread(context.Background(), owner); c != 1 {
		t.Fatalf("owner's entry must still be unread, count=%d", c)
	}

	// The owner marks it read; a second call is idempotent.
	if err := uc.MarkRead(context.Background(), n.ID, owner); err != nil {
		t.Fatalf("owner mark-read: %v", err)
	}
	if err := uc.MarkRead(context.Background(), n.ID, owner); err != nil {
		t.Fatalf("idempotent mark-read: %v", err)
	}
	if c, _ := feed.CountUnread(context.Background(), owner); c != 0 {
		t.Fatalf("after read, unread = %d, want 0", c)
	}
}

func TestNotificationFeedMarkAllRead(t *testing.T) {
	feed := newFakeFeed()
	uc := NewNotificationFeedUseCase(feed)
	uid := uuid.New()
	for i := 0; i < 3; i++ {
		_, _ = feed.Insert(context.Background(), &domain.Notification{
			UserID: uid, Type: domain.FeedTypeBooking, Title: "t", Body: "b",
			OutboxEventID: uuid.New(), CreatedAt: time.Now(),
		})
	}
	if err := uc.MarkAllRead(context.Background(), uid); err != nil {
		t.Fatalf("mark all: %v", err)
	}
	if c, _ := feed.CountUnread(context.Background(), uid); c != 0 {
		t.Fatalf("unread after mark-all = %d, want 0", c)
	}
}

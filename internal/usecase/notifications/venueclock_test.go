package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func almatyEvent(t domain.BookingEventType) Event {
	uid := uuid.New()
	e := guestEvent(uid, t)
	e.StartsAt = time.Date(2026, 8, 1, 15, 30, 0, 0, time.UTC)
	return e
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func TestGuestMessageRendersInVenueZone(t *testing.T) {
	almaty := mustLoc(t, "Asia/Almaty")
	for _, typ := range []domain.BookingEventType{domain.EventBookingConfirmed, domain.EventBookingCancelled} {
		msg, ok := buildGuestMessage(almatyEvent(typ), "Ocean Basket", "", almaty)
		if !ok {
			t.Fatalf("%s: no template", typ)
		}
		if !strings.Contains(msg.Body, "01.08 в 20:30") {
			t.Errorf("%s: body %q must show 20:30 (Almaty), not 15:30 (UTC)", typ, msg.Body)
		}
	}
}

func TestGuestFooterRendersInVenueZone(t *testing.T) {
	almaty := mustLoc(t, "Asia/Almaty")
	n := &GuestPushNotifier{
		rules:         fakeVenues{freeCancelMinutes: hourly(120)},
		rulesDefaults: testBookingRulesDefaults, // hold 15 min
		log:           discardLog(),
	}
	footer := n.bookingRulesFooter(context.Background(), almatyEvent(domain.EventBookingConfirmed), almaty)
	if !strings.Contains(footer, "до 20:45") { // 15:30 UTC = 20:30 + 15 min hold
		t.Errorf("hold-until in %q must be 20:45 Almaty", footer)
	}
	if !strings.Contains(footer, "до 01.08 в 18:30") { // 20:30 - 2h
		t.Errorf("free-cancel deadline in %q must be 18:30 Almaty", footer)
	}
}

func TestVenueClockResolvesAndFallsBack(t *testing.T) {
	almaty := mustLoc(t, "Asia/Almaty")
	tokyo := mustLoc(t, "Asia/Tokyo")
	rid := uuid.New()
	cases := []struct {
		name string
		tz   string
		want *time.Location
	}{
		{"venue zone wins", "Asia/Tokyo", tokyo},
		{"empty zone uses platform fallback", "", almaty},
		{"garbage zone uses platform fallback", "Mars/Olympus", almaty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newVenueClock(fakeVenues{tz: tc.tz}, almaty, discardLog())
			if got := c.location(context.Background(), rid); got.String() != tc.want.String() {
				t.Errorf("location = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFeedRendersInVenueZone(t *testing.T) {
	mustLoc(t, "Asia/Almaty")
	feed := newFakeFeed()
	n := NewFeedNotifier(feed, fakeVenues{name: "Ocean Basket"}, fakeVenues{tz: "Asia/Almaty"}, nil, discardLog())
	e := almatyEvent(domain.EventBookingConfirmed)
	if err := n.Notify(context.Background(), e); err != nil {
		t.Fatalf("notify: %v", err)
	}
	rows, _ := feed.ListByUser(context.Background(), *e.GuestUserID, nil, 10)
	if len(rows) != 1 || !strings.Contains(rows[0].Body, "20:30") {
		t.Fatalf("feed body must show 20:30 Almaty: %+v", rows)
	}
}

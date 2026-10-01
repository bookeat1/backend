package kwaakaorders

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func ptrT(t time.Time) *time.Time { return &t }

func TestDueAt(t *testing.T) {
	starts := time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)
	lead := 60 * time.Minute
	base := func(status domain.BookingStatus, paid bool) *domain.KitchenClaimSubject {
		return &domain.KitchenClaimSubject{Status: status, Paid: paid, StartsAt: starts, EndsAt: starts.Add(2 * time.Hour),
			ConfirmedAt: ptrT(starts.Add(-24 * time.Hour)), CapturedAt: ptrT(starts.Add(-23 * time.Hour)),
			ArrivedAt: ptrT(starts.Add(5 * time.Minute))}
	}
	enabledLong := ptrT(starts.Add(-48 * time.Hour))
	cases := []struct {
		name    string
		sub     *domain.KitchenClaimSubject
		enabled *time.Time
		now     time.Time
		want    bool
		trigger domain.KitchenOrderTrigger
	}{
		{"paid: 1s before starts-lead", base(domain.BookingConfirmed, true), enabledLong, starts.Add(-lead - time.Second), false, ""},
		{"paid: exactly at starts-lead", base(domain.BookingConfirmed, true), enabledLong, starts.Add(-lead), true, domain.KitchenTriggerLead},
		{"paid: after deadline starts+30m", base(domain.BookingConfirmed, true), enabledLong, starts.Add(31 * time.Minute), false, ""},
		{"paid: inside window late", base(domain.BookingConfirmed, true), enabledLong, starts.Add(29 * time.Minute), true, domain.KitchenTriggerLead},
		{"unpaid confirmed never", base(domain.BookingConfirmed, false), enabledLong, starts, false, ""},
		{"unpaid confirmed never, even early", base(domain.BookingConfirmed, false), enabledLong, starts.Add(-lead), false, ""},
		{"arrived unpaid goes at once", base(domain.BookingArrived, false), enabledLong, starts.Add(6 * time.Minute), true, domain.KitchenTriggerArrived},
		{"arrived paid goes at once", base(domain.BookingArrived, true), enabledLong, starts.Add(6 * time.Minute), true, domain.KitchenTriggerArrived},
		{"arrived after ends_at", base(domain.BookingArrived, false), enabledLong, starts.Add(2*time.Hour + time.Minute), false, ""},
		{"send moment before enabled_at", base(domain.BookingConfirmed, true), ptrT(starts.Add(-30 * time.Minute)), starts.Add(-10 * time.Minute), false, ""},
		{"arrived before enabled_at", base(domain.BookingArrived, false), ptrT(starts.Add(time.Hour)), starts.Add(70 * time.Minute), false, ""},
		{"pending never", base(domain.BookingPending, true), enabledLong, starts, false, ""},
		{"waitlist never", base(domain.BookingWaitlist, true), enabledLong, starts, false, ""},
		{"cancelled never", base(domain.BookingCancelled, true), enabledLong, starts, false, ""},
		{"no_show never", base(domain.BookingNoShow, true), enabledLong, starts, false, ""},
		{"completed never", base(domain.BookingCompleted, true), enabledLong, starts, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dueAt(tc.sub, lead, tc.enabled, tc.now)
			if got.Send != tc.want || (tc.want && got.Trigger != tc.trigger) {
				t.Fatalf("got %+v want send=%v trigger=%s", got, tc.want, tc.trigger)
			}
		})
	}
}

func TestPickTable(t *testing.T) {
	pool := []domain.KwaakaPoolTable{{KwaakaTableID: "A", Position: 1}, {KwaakaTableID: "B", Position: 2}, {KwaakaTableID: "C", Position: 3}}
	none := map[string]int{}
	cases := []struct {
		name       string
		pos        map[string]int
		inflight   map[string]int
		live       map[string]int
		want       string
		wantShared bool
	}{
		{"all free → first by position", none, none, none, "A", false},
		{"POS shows A busy", map[string]int{"A": 1}, none, none, "B", false},
		{"inflight on top of POS", map[string]int{"A": 1}, map[string]int{"B": 1}, none, "C", false},
		{"POS silent → live", nil, none, map[string]int{"A": 1, "B": 1}, "C", false},
		{"POS silent, live ignored inflight", nil, map[string]int{"A": 5}, none, "A", false},
		{"all busy → least loaded, shared", map[string]int{"A": 2, "B": 1, "C": 3}, none, none, "B", true},
		{"all busy tie → smaller position", map[string]int{"A": 1, "B": 1, "C": 1}, none, none, "A", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, shared := pickTable(pool, tc.pos, tc.inflight, tc.live)
			if got != tc.want || shared != tc.wantShared {
				t.Fatalf("got %s shared=%v want %s shared=%v", got, shared, tc.want, tc.wantShared)
			}
		})
	}
}

func TestBuildSnapshot(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Almaty")
	p1 := "prod-1"
	c := "без лука"
	notes := "АЛЛЕРГИЯ-НА-ОРЕХИ секретная заметка"
	sub := &domain.KitchenClaimSubject{BookingID: uuid.MustParse("abcdef12-0000-0000-0000-000000000000"),
		Name: "Айдар", Phone: "+77771234567", Guests: 4, StartsAt: time.Date(2026, 9, 24, 14, 0, 0, 0, time.UTC),
		Paid: true, PaidMinor: 550000,
		Items: []domain.KitchenClaimItem{
			{Name: "Наггетсы", PriceMinor: 289000, Currency: "KZT", Quantity: 2, KwaakaProductID: &p1, Comment: &c},
			{Name: "Ручное блюдо", PriceMinor: 100000, Currency: "KZT", Quantity: 1},
		}}
	_ = notes // bookings.notes is not part of KitchenClaimSubject by construction
	r := buildSnapshot(sub, "oid", "T1", loc)
	if r.NoPosItems || !r.Partial || r.TotalMinor != 289000*2+100000 {
		t.Fatalf("flags: %+v", r)
	}
	if len(r.Snapshot.Items) != 1 || r.Snapshot.Items[0].ProductID != "prod-1" || r.Snapshot.Items[0].PriceMinor != 289000 {
		t.Fatalf("items: %+v", r.Snapshot.Items)
	}
	cm := r.Snapshot.Comment
	for _, want := range []string{"бронь abcdef12", "24.09 19:00", "гостей 4", "Айдар",
		"ОПЛАЧЕНО ОНЛАЙН 5500 ₸, повторно не брать", "К блюдам: Наггетсы: без лука",
		"Нет в кассе, ввести вручную: Ручное блюдо ×1 — 1000 ₸"} {
		if !strings.Contains(cm, want) {
			t.Errorf("comment lacks %q:\n%s", want, cm)
		}
	}
	for _, bad := range []string{"АЛЛЕРГИЯ", "секретная", "@", "email"} {
		if strings.Contains(cm, bad) {
			t.Errorf("comment must not contain %q", bad)
		}
	}
	sub.Paid = false
	if !strings.Contains(buildSnapshot(sub, "oid", "T1", loc).Snapshot.Comment, "Предзаказ НЕ оплачен") {
		t.Error("unpaid line missing")
	}
}

func TestBuildSnapshotNoPosItemsAndOverflow(t *testing.T) {
	loc := time.UTC
	sub := &domain.KitchenClaimSubject{BookingID: uuid.New(), Name: "X", StartsAt: time.Now(), Paid: true, PaidMinor: 100,
		Items: []domain.KitchenClaimItem{{Name: "M", PriceMinor: 100, Quantity: 1}}}
	if r := buildSnapshot(sub, "o", "T", loc); !r.NoPosItems {
		t.Fatal("no product ids → NoPosItems")
	}
	pid := "p"
	long := strings.Repeat("я", 2000)
	sub.Items = []domain.KitchenClaimItem{{Name: "N", PriceMinor: 100, Quantity: 1, KwaakaProductID: &pid, Comment: &long}}
	cm := buildSnapshot(sub, "o", "T", loc).Snapshot.Comment
	if !strings.Contains(cm, "ОПЛАЧЕНО ОНЛАЙН") {
		t.Fatal("paid line must survive overflow")
	}
	if strings.Contains(cm, "К блюдам") {
		t.Fatal("dish comments must be cut first")
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
}

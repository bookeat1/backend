package kwaakaorders

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

var t0 = time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)

func sendingRow(attempts int) *domain.KitchenOrder {
	snap, _ := json.Marshal(domain.KitchenSnapshot{OrderID: "oid", TableID: "T1", Comment: "c"})
	n := t0
	return &domain.KitchenOrder{ID: uuid.New(), BookingID: uuid.New(), RestaurantID: uuid.New(),
		KwaakaRestaurantID: "kw-1", KwaakaTableID: "T1", Status: domain.KitchenOrderSending, Attempts: attempts,
		RequestSnapshot: snap, DeadlineAt: t0.Add(time.Hour), NextAttemptAt: &n}
}

func run(t *testing.T, row *domain.KitchenOrder, pos *fakePOS, mut func(w *Worker)) (*fakeOrders, *fakeOutbox) {
	t.Helper()
	fo := &fakeOrders{row: row}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)
	if mut != nil {
		mut(w)
	}
	cp := *row
	w.sendOne(context.Background(), &cp)
	return fo, ob
}

func TestSendOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		res        domain.PosCreateResult
		unknownIn  bool
		wantStatus domain.KitchenOrderStatus
		wantAlerts int
		wantGets   int
		wantUnk    bool
		getOrder   *domain.PosOrder
		getErr     error
	}{
		{"created", domain.PosCreateResult{Outcome: domain.PosCreated, PosOrderID: "K1"}, false, domain.KitchenOrderSent, 0, 0, false, nil, nil},
		{"rejected definite", domain.PosCreateResult{Outcome: domain.PosRejected, Message: "bad"}, false, domain.KitchenOrderFailed, 1, 0, false, nil, nil},
		{"rejected after unknown, found", domain.PosCreateResult{Outcome: domain.PosRejected}, true, domain.KitchenOrderSent, 0, 1, true, &domain.PosOrder{ID: "K9"}, nil},
		{"rejected after unknown, absent", domain.PosCreateResult{Outcome: domain.PosRejected}, true, domain.KitchenOrderFailed, 1, 1, true, nil, domain.ErrNotFound},
		{"rejected after unknown, GET broken", domain.PosCreateResult{Outcome: domain.PosRejected}, true, domain.KitchenOrderFailedUnknown, 1, 1, true, nil, context.DeadlineExceeded},
		{"auth retries", domain.PosCreateResult{Outcome: domain.PosAuth}, false, domain.KitchenOrderSending, 0, 0, false, nil, nil},
		{"429 retries", domain.PosCreateResult{Outcome: domain.PosRateLimit}, false, domain.KitchenOrderSending, 0, 0, false, nil, nil},
		{"5xx retries, marks unknown", domain.PosCreateResult{Outcome: domain.PosUnknown}, false, domain.KitchenOrderSending, 0, 0, true, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := sendingRow(1)
			row.OutcomeUnknown = tc.unknownIn
			pos := &fakePOS{create: []domain.PosCreateResult{tc.res}, getOrder: tc.getOrder, getErr: tc.getErr}
			fo, ob := run(t, row, pos, nil)
			if fo.row.Status != tc.wantStatus {
				t.Fatalf("status %s want %s", fo.row.Status, tc.wantStatus)
			}
			if len(ob.events) != tc.wantAlerts {
				t.Fatalf("alerts %d want %d", len(ob.events), tc.wantAlerts)
			}
			if pos.gets != tc.wantGets {
				t.Fatalf("gets %d want %d", pos.gets, tc.wantGets)
			}
			if tc.wantStatus == domain.KitchenOrderSending && (fo.row.NextAttemptAt == nil || !fo.row.NextAttemptAt.After(t0)) {
				t.Fatal("retry not scheduled in the future")
			}
			if tc.res.Outcome == domain.PosUnknown && !fo.row.OutcomeUnknown {
				t.Fatal("5xx must set outcome_unknown")
			}
		})
	}
}

func TestSendRetryCarriesSameBody(t *testing.T) {
	row := sendingRow(1)
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosUnknown}, {Outcome: domain.PosCreated, PosOrderID: "K"}}}
	fo := &fakeOrders{row: row}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	cp := *row
	w.sendOne(context.Background(), &cp)
	next := *fo.row
	next.Attempts = 2 // what the lease does
	fo.row.Attempts = 2
	w.sendOne(context.Background(), &next)
	if len(pos.creates) != 2 {
		t.Fatalf("creates %d", len(pos.creates))
	}
	a, _ := json.Marshal(pos.creates[0])
	b, _ := json.Marshal(pos.creates[1])
	if string(a) != string(b) || pos.creates[1].OrderID != "oid" || pos.creates[1].TableID != "T1" {
		t.Fatalf("retry body differs: %s vs %s", a, b)
	}
	if fo.row.Status != domain.KitchenOrderSent {
		t.Fatalf("status %s", fo.row.Status)
	}
}

func TestSendWindowEnded(t *testing.T) {
	// deadline passed, no attempt landed → failed, no HTTP create.
	row := sendingRow(1)
	row.DeadlineAt = t0.Add(-time.Second)
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated}}}
	fo, ob := run(t, row, pos, nil)
	if fo.row.Status != domain.KitchenOrderFailed || len(pos.creates) != 0 || len(ob.events) != 1 {
		t.Fatalf("%s creates=%d alerts=%d", fo.row.Status, len(pos.creates), len(ob.events))
	}
	// deadline passed after an unknown outcome → GET reconcile.
	row = sendingRow(3)
	row.DeadlineAt = t0.Add(-time.Second)
	row.OutcomeUnknown = true
	pos = &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated}}, getErr: domain.ErrNotFound}
	fo, ob = run(t, row, pos, nil)
	if fo.row.Status != domain.KitchenOrderFailed || pos.gets != 1 || len(pos.creates) != 0 {
		t.Fatalf("%s gets=%d", fo.row.Status, pos.gets)
	}
	// attempts exhausted, GET inconclusive → failed_unknown.
	row = sendingRow(6)
	row.OutcomeUnknown = true
	pos = &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated}}, getErr: context.DeadlineExceeded}
	fo, ob = run(t, row, pos, nil)
	if fo.row.Status != domain.KitchenOrderFailedUnknown || len(ob.events) != 1 {
		t.Fatalf("%s alerts=%d", fo.row.Status, len(ob.events))
	}
}

func TestSendCancelledBookingGetsNoFailureAlert(t *testing.T) {
	row := sendingRow(1)
	c := t0
	row.CancelRequestedAt = &c
	row.OutcomeUnknown = true
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRejected}}, getErr: domain.ErrNotFound}
	fo, ob := run(t, row, pos, nil)
	if len(ob.events) != 0 {
		t.Fatalf("alerts for cancelled booking: %d", len(ob.events))
	}
	_ = fo
}

func TestSendCreatedWhileCancelRequestedGoesToCancelling(t *testing.T) {
	row := sendingRow(2)
	row.OutcomeUnknown = true
	c := t0
	row.CancelRequestedAt = &c
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K"}}}
	fo, _ := run(t, row, pos, nil)
	if fo.row.Status != domain.KitchenOrderCancelling || fo.row.KwaakaOrderID == nil {
		t.Fatalf("%s", fo.row.Status)
	}
}

func TestSendLostCASKeepsPosOrderID(t *testing.T) {
	row := sendingRow(1)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K7"}}}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	cp := *row
	// somebody bumps the row (lease of another instance) before our outcome lands
	fo.row.Attempts = 2
	w.sendOne(context.Background(), &cp)
	if fo.row.KwaakaOrderID == nil || *fo.row.KwaakaOrderID != "K7" {
		t.Fatal("POS order id lost after CAS miss")
	}
}

func TestCancelSweepAndSend(t *testing.T) {
	// sent → cancelling
	row := sendingRow(1)
	row.Status = domain.KitchenOrderSent
	fo := &fakeOrders{row: row}
	pos := &fakePOS{cancel: domain.PosCancelResult{Outcome: domain.PosCreated}}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	if err := w.CancelSweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fo.row.Status != domain.KitchenOrderCancelling {
		t.Fatalf("%s", fo.row.Status)
	}
	// second sweep: request flag set → no double
	if rows, _ := fo.ListCancelledPending(context.Background(), 10); len(rows) != 0 {
		t.Fatal("swept twice")
	}
	fo.row.Attempts = 1
	cp := *fo.row
	w.cancelOne(context.Background(), &cp)
	if fo.row.Status != domain.KitchenOrderCancelled || len(pos.cancels) != 1 || fo.row.TableReleasedAt == nil {
		t.Fatalf("%s cancels=%v", fo.row.Status, pos.cancels)
	}
}

func TestCancelSweepSendingNoAttempts(t *testing.T) {
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	_ = w.CancelSweep(context.Background())
	if fo.row.Status != domain.KitchenOrderCancelled || len(pos.cancels)+len(pos.creates) != 0 {
		t.Fatal("expected cancelled without HTTP")
	}
}

func TestCancelUsesStoredKwaakaRestaurantAndReconciles(t *testing.T) {
	row := sendingRow(1)
	row.Status = domain.KitchenOrderCancelling
	c := t0
	row.CancelRequestedAt = &c
	id := "K1"
	row.KwaakaOrderID = &id
	cases := []struct {
		name string
		get  *domain.PosOrder
		err  error
		want domain.KitchenOrderStatus
		al   int
	}{
		{"gone", nil, domain.ErrNotFound, domain.KitchenOrderCancelled, 0},
		{"deleted", &domain.PosOrder{State: domain.PosStateDeleted}, nil, domain.KitchenOrderCancelled, 0},
		{"refused", &domain.PosOrder{State: domain.PosStateOpen}, nil, domain.KitchenOrderCancelFailed, 1},
		{"unclear retries", nil, context.DeadlineExceeded, domain.KitchenOrderCancelling, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := *row
			fo := &fakeOrders{row: &r}
			ob := &fakeOutbox{}
			pos := &fakePOS{cancel: domain.PosCancelResult{Outcome: domain.PosRejected}, getOrder: tc.get, getErr: tc.err}
			w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)
			cp := r
			w.cancelOne(context.Background(), &cp)
			if fo.row.Status != tc.want || len(ob.events) != tc.al {
				t.Fatalf("%s alerts=%d", fo.row.Status, len(ob.events))
			}
			if pos.cancels[0] != "kw-1|K1" {
				t.Fatalf("cancel target %v", pos.cancels)
			}
		})
	}
}

func TestClaimPassNoHTTPWhenOffOrMismatched(t *testing.T) {
	starts := t0.Add(30 * time.Minute)
	sub := &domain.KitchenClaimSubject{BookingID: uuid.New(), RestaurantID: uuid.New(), Status: domain.BookingConfirmed, Paid: true,
		StartsAt: starts, EndsAt: starts.Add(2 * time.Hour), Timezone: "Asia/Almaty", KwaakaID: "kw-1", Name: "A", Guests: 2,
		Items: []domain.KitchenClaimItem{{Name: "X", PriceMinor: 1000, Currency: "KZT", Quantity: 1, KwaakaProductID: str("p")}}}
	pool := []domain.KwaakaPoolTable{{KwaakaTableID: "T1", Position: 1}}
	enabled := t0.Add(-time.Hour)
	mk := func(s domain.KwaakaOrderSettings, global bool) (*fakeOrders, *fakePOS) {
		fo := &fakeOrders{sub: sub}
		pos := &fakePOS{}
		w := newTestWorker(fo, fakeSettings{&s}, pos, &fakeOutbox{}, t0)
		w.cfg.Enabled = global
		_ = w.ClaimPass(context.Background())
		return fo, pos
	}
	ok := domain.KwaakaOrderSettings{OrdersEnabled: true, KwaakaRestaurantID: "kw-1", Pool: pool, EnabledAt: &enabled}
	for name, tc := range map[string]struct {
		s domain.KwaakaOrderSettings
		g bool
	}{
		"global off":   {ok, false},
		"venue off":    {func() domain.KwaakaOrderSettings { s := ok; s.OrdersEnabled = false; return s }(), true},
		"link changed": {func() domain.KwaakaOrderSettings { s := ok; s.KwaakaRestaurantID = "other"; return s }(), true},
		"empty pool":   {func() domain.KwaakaOrderSettings { s := ok; s.Pool = nil; return s }(), true},
	} {
		fo, pos := mk(tc.s, tc.g)
		if len(fo.inserted) != 0 || pos.lists+len(pos.creates)+pos.gets != 0 {
			t.Fatalf("%s: inserted=%d http=%d", name, len(fo.inserted), pos.lists+len(pos.creates))
		}
	}
	// happy: due (now = starts-30m, lead 60m) → one row, one POS load call, no create.
	fo, pos := mk(ok, true)
	if len(fo.inserted) != 1 || pos.lists != 1 || len(pos.creates) != 0 {
		t.Fatalf("inserted=%d lists=%d creates=%d", len(fo.inserted), pos.lists, len(pos.creates))
	}
	if fo.inserted[0].Status != domain.KitchenOrderSending || fo.inserted[0].KwaakaTableID != "T1" {
		t.Fatalf("%+v", fo.inserted[0])
	}
}

func TestAlertPayloadCarriesRestaurantID(t *testing.T) {
	row := sendingRow(1)
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRejected, Message: "bad"}}}
	_, ob := run(t, row, pos, nil)
	if len(ob.events) != 1 {
		t.Fatalf("events %d", len(ob.events))
	}
	var p map[string]any
	_ = json.Unmarshal(ob.events[0].Payload, &p)
	if p["restaurant_id"] != row.RestaurantID.String() || p["reason"] != "failed" {
		t.Fatalf("%v", p)
	}
}

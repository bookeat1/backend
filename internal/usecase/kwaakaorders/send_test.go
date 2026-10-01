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

// leaseAndSend runs one worker cycle the way SendPass does: lease, then drive.
func leaseAndSend(t *testing.T, w *Worker, fo *fakeOrders, now time.Time) {
	t.Helper()
	rows, err := fo.LeaseDue(context.Background(), now, time.Minute, 20)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		o := rows[i]
		w.sendOne(context.Background(), &o)
	}
}

// Review finding 4: the worker dies after the POST reached Kwaaka but before the
// outcome was stored. The restart must NOT treat attempt 2 as a fresh create:
// a 400 "duplicate" must reconcile via GET, not become a false failed + manual entry.
func TestRestartAfterUncertainPostReconcilesInsteadOfFailing(t *testing.T) {
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRejected, Message: "HTTP 400: duplicate order_id"}},
		getOrder: &domain.PosOrder{ID: "K-existing"}}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)

	// attempt 1: leased (write-ahead lands), POST goes out, process dies: nothing else is stored.
	rows, _ := fo.LeaseDue(context.Background(), t0, time.Minute, 20)
	if len(rows) != 1 || rows[0].OutcomeUnknown {
		t.Fatalf("first lease: %+v", rows)
	}
	if !fo.row.OutcomeUnknown || fo.row.Attempts != 1 {
		t.Fatalf("write-ahead missing: unknown=%v attempts=%d", fo.row.OutcomeUnknown, fo.row.Attempts)
	}
	// restart: lease expired, attempt 2.
	leaseAndSend(t, w, fo, t0.Add(2*time.Minute))
	if fo.row.Status != domain.KitchenOrderSent || pos.gets != 1 || len(ob.events) != 0 {
		t.Fatalf("status=%s gets=%d alerts=%d, want sent/1/0", fo.row.Status, pos.gets, len(ob.events))
	}
	if fo.row.KwaakaOrderID == nil || *fo.row.KwaakaOrderID != "K-existing" || fo.row.OutcomeUnknown {
		t.Fatalf("order id / flag not settled: %+v", fo.row)
	}
}

func TestRestartAfterUncertainPostAbsentFailsWithAlert(t *testing.T) {
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRejected, Message: "bad"}}, getErr: domain.ErrNotFound}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)
	_, _ = fo.LeaseDue(context.Background(), t0, time.Minute, 20) // died mid-POST
	leaseAndSend(t, w, fo, t0.Add(2*time.Minute))
	if fo.row.Status != domain.KitchenOrderFailed || pos.gets != 1 || len(ob.events) != 1 {
		t.Fatalf("status=%s gets=%d alerts=%d", fo.row.Status, pos.gets, len(ob.events))
	}
}

// A first-ever 400 is still a definite refusal (no GET, failed + alert).
func TestFirstAttemptRejectionIsDefinite(t *testing.T) {
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRejected, Message: "bad"}}}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)
	leaseAndSend(t, w, fo, t0)
	if fo.row.Status != domain.KitchenOrderFailed || pos.gets != 0 || len(ob.events) != 1 || fo.row.OutcomeUnknown {
		t.Fatalf("status=%s gets=%d alerts=%d unknown=%v", fo.row.Status, pos.gets, len(ob.events), fo.row.OutcomeUnknown)
	}
}

// 401/429 prove nothing was created: the write-ahead mark must be cleared again,
// otherwise every later reject/cancel is judged as "maybe landed".
func TestAuthAndRateLimitClearWriteAheadMark(t *testing.T) {
	for _, out := range []domain.PosCallOutcome{domain.PosAuth, domain.PosRateLimit} {
		row := sendingRow(0)
		fo := &fakeOrders{row: row}
		pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: out}}}
		w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
		leaseAndSend(t, w, fo, t0)
		if fo.row.Status != domain.KitchenOrderSending || fo.row.OutcomeUnknown {
			t.Fatalf("outcome %v: status=%s unknown=%v", out, fo.row.Status, fo.row.OutcomeUnknown)
		}
	}
	// ...but an earlier genuinely uncertain attempt stays uncertain through a 429.
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosUnknown}, {Outcome: domain.PosRateLimit}}}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	leaseAndSend(t, w, fo, t0)
	leaseAndSend(t, w, fo, t0.Add(time.Hour))
	if !fo.row.OutcomeUnknown {
		t.Fatal("uncertain outcome must survive a later 429")
	}
}

// Review finding 5: cancel before send holds on a retry after 429/401, not only on attempt 1.
func TestCancelBeforeSendHoldsAfterRateLimitRetry(t *testing.T) {
	row := sendingRow(0)
	fo := &fakeOrders{row: row}
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosRateLimit}, {Outcome: domain.PosCreated, PosOrderID: "K"}}}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, pos, ob, t0)
	leaseAndSend(t, w, fo, t0) // attempt 1: 429
	if len(pos.creates) != 1 || fo.row.OutcomeUnknown {
		t.Fatalf("setup: creates=%d unknown=%v", len(pos.creates), fo.row.OutcomeUnknown)
	}
	// the booking is cancelled while the row waits for its retry
	if err := w.CancelSweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fo.row.Status != domain.KitchenOrderCancelled {
		t.Fatalf("sweep must cancel directly, status=%s", fo.row.Status)
	}
	leaseAndSend(t, w, fo, t0.Add(time.Hour))
	if len(pos.creates) != 1 || len(pos.cancels) != 0 {
		t.Fatalf("a cancelled booking's order reached the kitchen: creates=%d cancels=%d", len(pos.creates), len(pos.cancels))
	}
	if len(ob.events) != 0 {
		t.Fatalf("alerts: %d", len(ob.events))
	}
}

// Same, when the cancel flag lands without a status change (the row was leased):
// sendOne itself must refuse to POST after a 429.
func TestSendOneCancelRequestedAfterRateLimitDoesNotPost(t *testing.T) {
	row := sendingRow(2)
	c := t0
	row.CancelRequestedAt = &c
	row.OutcomeUnknown = false // entry value: earlier attempts were 429/401
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K"}}}
	fo, ob := run(t, row, pos, nil)
	if fo.row.Status != domain.KitchenOrderCancelled || len(pos.creates) != 0 || len(ob.events) != 0 {
		t.Fatalf("status=%s creates=%d alerts=%d", fo.row.Status, len(pos.creates), len(ob.events))
	}
}

// After an uncertain attempt the sweep must NOT cancel locally: the order may exist.
func TestCancelSweepKeepsUncertainSendingRow(t *testing.T) {
	row := sendingRow(2)
	row.OutcomeUnknown = true
	fo := &fakeOrders{row: row}
	pos := &fakePOS{}
	w := newTestWorker(fo, fakeSettings{}, pos, &fakeOutbox{}, t0)
	_ = w.CancelSweep(context.Background())
	if fo.row.Status != domain.KitchenOrderSending || fo.row.CancelRequestedAt == nil {
		t.Fatalf("status=%s cancelReq=%v", fo.row.Status, fo.row.CancelRequestedAt)
	}
}

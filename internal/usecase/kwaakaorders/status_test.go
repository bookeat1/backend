package kwaakaorders

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func evt(id string, st domain.PosOrderState) domain.KwaakaOrderStatusEvent {
	return domain.KwaakaOrderStatusEvent{OrderID: id, Raw: string(st), State: st}
}

func sentRow() *domain.KitchenOrder {
	r := sendingRow(1)
	r.Status = domain.KitchenOrderSent
	k := "K1"
	r.KwaakaOrderID = &k
	return r
}

func TestApplyMonotonicAndStale(t *testing.T) {
	fo := &fakeOrders{row: sentRow()}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0)
	ctx := context.Background()
	if out, _, err := w.ApplyPOSStatus(ctx, evt("K1", domain.PosStateClosed), "webhook"); err != nil || out != domain.WebhookOutcomeApplied {
		t.Fatal(out, err)
	}
	if fo.row.TableReleasedAt == nil {
		t.Fatal("closed must release the table")
	}
	// late "open" after "closed" changes nothing
	if out, _, _ := w.ApplyPOSStatus(ctx, evt("K1", domain.PosStateOpen), "webhook"); out != domain.WebhookOutcomeStale {
		t.Fatal(out)
	}
	if *fo.row.PosState != domain.PosStateClosed {
		t.Fatal("state went backwards")
	}
	// terminal vs terminal: first wins
	if out, _, _ := w.ApplyPOSStatus(ctx, evt("K1", domain.PosStateDeleted), "webhook"); out != domain.WebhookOutcomeStale {
		t.Fatal(out)
	}
}

func TestApplySendingBecomesSentAndKeepsID(t *testing.T) {
	row := sendingRow(1)
	fo := &fakeOrders{row: row}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0)
	if _, _, err := w.ApplyPOSStatus(context.Background(), evt(row.ID.String(), domain.PosStateOpen), "webhook"); err != nil {
		t.Fatal(err)
	}
	if fo.row.Status != domain.KitchenOrderSent || fo.row.SentAt == nil {
		t.Fatalf("%s", fo.row.Status)
	}
	// the in-flight POST outcome must now lose its CAS and not wipe pos_state
	cp := *row
	pos := &fakePOS{create: []domain.PosCreateResult{{Outcome: domain.PosCreated, PosOrderID: "K5"}}}
	w.pos = pos
	w.sendOne(context.Background(), &cp)
	if fo.row.PosState == nil || *fo.row.PosState != domain.PosStateOpen {
		t.Fatal("pos_state overwritten by the POST outcome")
	}
}

// A webhook that proves creation while the POST outcome was still unknown must
// clear the mark: the row is `sent` and there is nothing left to reconcile.
func TestApplySendingBecomesSentClearsOutcomeUnknown(t *testing.T) {
	row := sendingRow(2)
	row.OutcomeUnknown = true
	fo := &fakeOrders{row: row}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0)
	if _, _, err := w.ApplyPOSStatus(context.Background(), evt(row.ID.String(), domain.PosStateOpen), "webhook"); err != nil {
		t.Fatal(err)
	}
	if fo.row.Status != domain.KitchenOrderSent || fo.row.OutcomeUnknown {
		t.Fatalf("status=%s outcome_unknown=%v, want sent/false", fo.row.Status, fo.row.OutcomeUnknown)
	}
}

func TestApplyDeletedRules(t *testing.T) {
	ctx := context.Background()
	// live booking: exactly one pos_cancelled alert, however many deliveries
	fo := &fakeOrders{row: sentRow()}
	ob := &fakeOutbox{}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, ob, t0)
	for i := 0; i < 3; i++ {
		_, _, _ = w.ApplyPOSStatus(ctx, evt("K1", domain.PosStateDeleted), "webhook")
	}
	if len(ob.events) != 1 || fo.row.Status != domain.KitchenOrderSent {
		t.Fatalf("alerts=%d status=%s", len(ob.events), fo.row.Status)
	}
	// cancelling → cancelled, no alert
	r := sentRow()
	r.Status = domain.KitchenOrderCancelling
	c := t0
	r.CancelRequestedAt = &c
	fo = &fakeOrders{row: r}
	ob = &fakeOutbox{}
	w = newTestWorker(fo, fakeSettings{}, &fakePOS{}, ob, t0)
	_, _, _ = w.ApplyPOSStatus(ctx, evt("K1", domain.PosStateDeleted), "poll")
	if fo.row.Status != domain.KitchenOrderCancelled || len(ob.events) != 0 || fo.row.TableReleasedAt == nil {
		t.Fatalf("%s alerts=%d", fo.row.Status, len(ob.events))
	}
}

func TestApplyUnknownStatusStoredRaw(t *testing.T) {
	fo := &fakeOrders{row: sentRow()}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0)
	ev := domain.KwaakaOrderStatusEvent{OrderID: "K1", Raw: "weird", State: domain.PosStateUnknown}
	out, _, err := w.ApplyPOSStatus(context.Background(), ev, "webhook")
	if err != nil || out != domain.WebhookOutcomeUnknownStatus || fo.row.PosState != nil || *fo.row.PosStatusRaw != "weird" {
		t.Fatal(out, err)
	}
}

func TestInboxUnknownOrderRetriesThenIgnored(t *testing.T) {
	id := uuid.New()
	hooks := newFakeHooks(domain.KwaakaWebhookEvent{ID: id, Kind: domain.KwaakaWebhookOrderStatus, Body: []byte(`{"orderId":"nope","status":"open"}`)})
	fo := &fakeOrders{row: sentRow()}
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0).WithInbox(hooks, 0)
	for attempt := 0; attempt < 3; attempt++ {
		hooks.ev[0].Attempts = attempt
		if err := w.ProcessInbox(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, done := hooks.outcome[id]; done || hooks.deferred[id].IsZero() {
			t.Fatalf("attempt %d: not deferred", attempt)
		}
	}
	hooks.ev[0].Attempts = 3
	_ = w.ProcessInbox(context.Background())
	if hooks.outcome[id] != domain.WebhookOutcomeIgnoredUnknownOrder {
		t.Fatal(hooks.outcome[id])
	}
}

func TestInboxKindsAndGarbage(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	fo := &fakeOrders{row: sentRow()}
	hooks := newFakeHooks(
		domain.KwaakaWebhookEvent{ID: a, Kind: domain.KwaakaWebhookReserveStatus, Body: []byte(`{"reserveId":"R","status":"x"}`)},
		domain.KwaakaWebhookEvent{ID: b, Kind: domain.KwaakaWebhookOrderStatus, Body: []byte(`not json`)},
		domain.KwaakaWebhookEvent{ID: c, Kind: domain.KwaakaWebhookOrderStatus, Body: []byte(`{"orderId":"K1","status":"printed"}`)},
	)
	w := newTestWorker(fo, fakeSettings{}, &fakePOS{}, &fakeOutbox{}, t0).WithInbox(hooks, 0)
	if err := w.ProcessInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hooks.outcome[a] != domain.WebhookOutcomeStored || hooks.outcome[b] != domain.WebhookOutcomeUnparseable || hooks.outcome[c] != domain.WebhookOutcomeApplied {
		t.Fatal(hooks.outcome)
	}
	if *fo.row.PosState != domain.PosStateBillPrinted {
		t.Fatal("reserve-status or garbage changed something / order not applied")
	}
}

func TestPollUsesSameFunctionAndCanBeDisabled(t *testing.T) {
	r := sentRow()
	fo := &fakePollOrders{fakeOrders: fakeOrders{row: r}}
	pos := &fakePOS{getOrder: &domain.PosOrder{ID: "K1", Raw: "closed", State: domain.PosStateClosed}}
	w := newTestWorker(&fo.fakeOrders, fakeSettings{}, pos, &fakeOutbox{}, t0)
	w.orders = fo
	_ = w.PollPass(context.Background())
	if pos.gets != 0 {
		t.Fatal("poll must be off at 0")
	}
	w.pollEvery = 15 * time.Minute
	_ = w.PollPass(context.Background())
	if pos.gets != 1 || fo.row.PosState == nil || *fo.row.PosState != domain.PosStateClosed || *fo.row.PosStatusSource != "poll" {
		t.Fatalf("gets=%d", pos.gets)
	}
}

type fakePollOrders struct{ fakeOrders }

func (f *fakePollOrders) ListPollDue(context.Context, time.Time, time.Time, int) ([]domain.KitchenOrder, error) {
	return []domain.KitchenOrder{*f.row}, nil
}

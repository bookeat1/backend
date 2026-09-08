package notifications

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// receiptHarness wires a ReceiptWorker over in-memory fakes with a frozen clock.
type receiptHarness struct {
	w       *ReceiptWorker
	tickets *fakePushTickets
	tokens  *fakeDeviceTokens
	now     time.Time

	// calls records the id batches the worker asked about, in order.
	calls [][]string
	// answer is the scripted provider reply, keyed by ticket id. An id ABSENT
	// from it is absent from the response — Expo's "no receipt yet".
	answer map[string]MobilePushReceipt
	// fetchErr, when set, makes every provider call fail.
	fetchErr error
}

func newReceiptHarness(t *testing.T, cfg ReceiptWorkerConfig, tokens ...domain.DevicePushToken) *receiptHarness {
	t.Helper()
	h := &receiptHarness{
		tickets: newFakePushTickets(),
		tokens:  newFakeDeviceTokens(tokens...),
		now:     time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		answer:  map[string]MobilePushReceipt{},
	}
	fetch := func(_ context.Context, ids []string) (map[string]MobilePushReceipt, error) {
		batch := append([]string(nil), ids...)
		h.calls = append(h.calls, batch)
		if h.fetchErr != nil {
			return nil, h.fetchErr
		}
		out := map[string]MobilePushReceipt{}
		for _, id := range ids {
			if r, ok := h.answer[id]; ok {
				out[id] = r
			}
		}
		return out, nil
	}
	h.w = NewReceiptWorker(h.tickets, h.tokens, fetch, cfg, discardLog())
	h.w.now = func() time.Time { return h.now }
	return h
}

// seed enqueues one ticket created `age` ago for the given device.
func (h *receiptHarness) seed(t *testing.T, id string, deviceTokenID uuid.UUID, age time.Duration) {
	t.Helper()
	if err := h.tickets.Record(context.Background(), domain.PushTicket{
		ID:            id,
		DeviceTokenID: deviceTokenID,
		CreatedAt:     h.now.Add(-age),
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
}

func receiptDevice() domain.DevicePushToken {
	return domain.DevicePushToken{
		ID: uuid.New(), UserID: uuid.New(),
		Token:    "ExponentPushToken[" + uuid.NewString() + "]",
		Platform: domain.PlatformAndroid, IsActive: true,
	}
}

// THE BUG THIS WHOLE PATH EXISTS FOR. Expo answered the SEND with status "ok",
// so the send path recorded a delivery and moved on; the receipt says the device
// is not registered with FCM. The token must end up inactive.
//
// Reproduced live on 2026-09-01 against production android tokens: three sends,
// three `ok` tickets, two DeviceNotRegistered receipts.
//
// This test fails on the pre-fix behaviour (no receipt polling at all): with the
// worker removed nothing ever reads the ticket and the token stays active.
func TestReceiptWorkerDeactivatesTokenOnDeviceNotRegistered(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.seed(t, "ticket-dead", dev.ID, time.Hour)
	h.answer["ticket-dead"] = MobilePushReceipt{
		Verdict: MobilePushDeviceGone, Reason: "DeviceNotRegistered",
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if h.tokens.isActive(dev.ID) {
		t.Fatal("a token Expo reported as DeviceNotRegistered is still active")
	}
	if res.Gone != 1 || res.Polled != 1 {
		t.Fatalf("tick result = %+v, want 1 polled / 1 gone", res)
	}
	if left := h.tickets.unresolvedIDs(); len(left) != 0 {
		t.Fatalf("an answered ticket is still queued: %v", left)
	}
}

// A receipt that blames the MESSAGE, not the device, must NOT silence the phone.
// MessageRateExceeded is the dangerous one: it is transient by nature, and
// deactivating on it would mute a perfectly live device forever — the token is
// only re-activated by the app registering again.
func TestReceiptWorkerKeepsTokenOnNonDeviceError(t *testing.T) {
	for _, reason := range []string{"MessageRateExceeded", "MessageTooBig", "MismatchSenderId", "InvalidCredentials"} {
		t.Run(reason, func(t *testing.T) {
			dev := receiptDevice()
			h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
			h.seed(t, "ticket-"+reason, dev.ID, time.Hour)
			h.answer["ticket-"+reason] = MobilePushReceipt{
				Verdict: MobilePushRejected, Reason: reason,
			}

			res, err := h.w.Tick(context.Background())
			if err != nil {
				t.Fatalf("tick: %v", err)
			}
			if !h.tokens.isActive(dev.ID) {
				t.Fatalf("%s deactivated a live device token", reason)
			}
			if res.Rejected != 1 || res.Gone != 0 {
				t.Fatalf("tick result = %+v, want 1 rejected / 0 gone", res)
			}
			// Still resolved: the receipt is final, asking again changes nothing.
			if left := h.tickets.unresolvedIDs(); len(left) != 0 {
				t.Fatalf("a finally-answered ticket is still queued: %v", left)
			}
		})
	}
}

// A receipt that does not exist YET is the normal case, observed live. The
// ticket must stay queued: treating "absent" as delivered would swallow exactly
// the DeviceNotRegistered this worker was built to catch.
func TestReceiptWorkerLeavesTicketQueuedWhenReceiptIsNotReadyYet(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.seed(t, "ticket-pending", dev.ID, time.Hour)
	// No entry in h.answer → the provider omits it from the response.

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Pending != 1 || res.Delivered != 0 || res.Gone != 0 {
		t.Fatalf("tick result = %+v, want 1 pending only", res)
	}
	if !h.tokens.isActive(dev.ID) {
		t.Fatal("a device with no receipt yet was deactivated")
	}
	if got := h.tickets.unresolvedIDs(); len(got) != 1 || got[0] != "ticket-pending" {
		t.Fatalf("unresolved = %v, want the ticket to stay queued", got)
	}

	// Next tick, the receipt has arrived and the token dies.
	h.answer["ticket-pending"] = MobilePushReceipt{Verdict: MobilePushDeviceGone, Reason: "DeviceNotRegistered"}
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if h.tokens.isActive(dev.ID) {
		t.Fatal("the token survived a DeviceNotRegistered receipt on the second tick")
	}
}

// A ticket younger than MinAge is not worth a request: the provider has no
// receipt for it yet.
func TestReceiptWorkerIgnoresTicketsYoungerThanMinAge(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{MinAge: 15 * time.Minute}, dev)
	h.seed(t, "ticket-young", dev.ID, time.Minute)

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("asked the provider about a %v-old ticket: %v", time.Minute, h.calls)
	}
	if res != (ReceiptTickResult{}) {
		t.Fatalf("tick result = %+v, want the zero value", res)
	}
}

// PROVIDER RETENTION. Expo deletes receipts after 24 hours, so a ticket older
// than that can never be answered. It must be force-resolved — and never asked
// about — or the table grows forever and every tick re-asks about the same dead
// ids.
func TestReceiptWorkerExpiresTicketsPastProviderRetention(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.seed(t, "ticket-ancient", dev.ID, 25*time.Hour)
	h.seed(t, "ticket-fresh", dev.ID, time.Hour)
	h.answer["ticket-fresh"] = MobilePushReceipt{Verdict: MobilePushDelivered}
	// Scripted, but must never be reached: the ancient ticket is closed before
	// the queue is read.
	h.answer["ticket-ancient"] = MobilePushReceipt{Verdict: MobilePushDeviceGone, Reason: "DeviceNotRegistered"}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Expired != 1 {
		t.Fatalf("tick result = %+v, want 1 expired", res)
	}
	if len(h.calls) != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", len(h.calls))
	}
	for _, id := range h.calls[0] {
		if id == "ticket-ancient" {
			t.Fatal("asked the provider about a ticket whose receipt it has already deleted")
		}
	}
	if left := h.tickets.unresolvedIDs(); len(left) != 0 {
		t.Fatalf("unresolved after the pass = %v, want none", left)
	}
	if !h.tokens.isActive(dev.ID) {
		t.Fatal("an expired ticket must not deactivate anything — its receipt was never read")
	}
}

// BATCHING. The provider rejects more than 1000 ids in one call
// (PUSH_TOO_MANY_RECEIPTS), so a backlog has to be chunked. The worker must also
// stop reading the queue at MaxPerTick instead of pulling an unbounded batch
// into memory.
func TestReceiptWorkerBatchesRequests(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{BatchSize: 300, MaxPerTick: 700}, dev)
	for i := 0; i < 1000; i++ {
		h.seed(t, fmt.Sprintf("ticket-%04d", i), dev.ID, time.Hour)
	}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	var sizes []int
	seen := map[string]int{}
	for _, c := range h.calls {
		sizes = append(sizes, len(c))
		if len(c) > 300 {
			t.Fatalf("a batch of %d exceeds the configured 300", len(c))
		}
		for _, id := range c {
			seen[id]++
		}
	}
	if len(sizes) != 3 || sizes[0] != 300 || sizes[1] != 300 || sizes[2] != 100 {
		t.Fatalf("batch sizes = %v, want [300 300 100] (700 tickets capped by MaxPerTick)", sizes)
	}
	if res.Polled != 700 {
		t.Fatalf("polled = %d, want 700", res.Polled)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("ticket %s was asked about %d times in one pass", id, n)
		}
	}
	// The remaining 300 are still queued for the next tick, not lost.
	if got := len(h.tickets.unresolvedIDs()); got != 1000 {
		t.Fatalf("unresolved = %d, want all 1000 (nothing was answered)", got)
	}
}

// The batch size is clamped to the provider's hard limit, whatever the operator
// puts in the env var — an oversized batch is refused outright, which would make
// the worker a permanent no-op.
func TestReceiptWorkerClampsBatchToProviderLimit(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{BatchSize: 5000, MaxPerTick: 1500}, dev)
	for i := 0; i < 1500; i++ {
		h.seed(t, fmt.Sprintf("ticket-%04d", i), dev.ID, time.Hour)
	}
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, c := range h.calls {
		if len(c) > maxReceiptBatch {
			t.Fatalf("batch of %d exceeds the provider limit of %d", len(c), maxReceiptBatch)
		}
	}
	if len(h.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (1000 + 500)", len(h.calls))
	}
}

// A provider outage decides nothing: every ticket in the failing batch stays
// queued, and the tick reports the error so the loop logs it.
func TestReceiptWorkerFetchFailureLeavesTicketsQueued(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.seed(t, "ticket-1", dev.ID, time.Hour)
	h.fetchErr = errors.New("boom")

	if _, err := h.w.Tick(context.Background()); err == nil {
		t.Fatal("a failing provider call must surface as a tick error")
	}
	if got := h.tickets.unresolvedIDs(); len(got) != 1 {
		t.Fatalf("unresolved = %v, want the ticket kept for a retry", got)
	}
	if !h.tokens.isActive(dev.ID) {
		t.Fatal("a provider outage deactivated a device")
	}
}

// If the deactivation itself fails, the ticket must stay OPEN: closing it would
// throw away the only signal that this device is dead. The 24-hour expiry bounds
// how long that retry can go on.
func TestReceiptWorkerKeepsTicketWhenDeactivationFails(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.tokens.deactivateErr = errors.New("db down")
	h.seed(t, "ticket-dead", dev.ID, time.Hour)
	h.answer["ticket-dead"] = MobilePushReceipt{Verdict: MobilePushDeviceGone, Reason: "DeviceNotRegistered"}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Gone != 0 {
		t.Fatalf("tick result = %+v, want 0 gone (the deactivation failed)", res)
	}
	if got := h.tickets.unresolvedIDs(); len(got) != 1 {
		t.Fatalf("unresolved = %v, want the ticket kept until the token is really silenced", got)
	}

	// The database comes back: the retry closes it.
	h.tokens.deactivateErr = nil
	if _, err := h.w.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if h.tokens.isActive(dev.ID) {
		t.Fatal("the retry did not deactivate the dead token")
	}
	if got := h.tickets.unresolvedIDs(); len(got) != 0 {
		t.Fatalf("unresolved = %v, want none after a successful retry", got)
	}
}

// A delivered receipt closes the ticket and touches nothing else.
func TestReceiptWorkerResolvesDeliveredTickets(t *testing.T) {
	dev := receiptDevice()
	h := newReceiptHarness(t, ReceiptWorkerConfig{}, dev)
	h.seed(t, "ticket-ok", dev.ID, time.Hour)
	h.answer["ticket-ok"] = MobilePushReceipt{Verdict: MobilePushDelivered}

	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Delivered != 1 || res.Gone != 0 || res.Rejected != 0 {
		t.Fatalf("tick result = %+v, want 1 delivered", res)
	}
	if !h.tokens.isActive(dev.ID) {
		t.Fatal("a delivered receipt deactivated the device")
	}
	if left := h.tickets.unresolvedIDs(); len(left) != 0 {
		t.Fatalf("unresolved = %v, want none", left)
	}
}

// An empty queue is a cheap no-op: no provider call, no error.
func TestReceiptWorkerEmptyQueueIsNoop(t *testing.T) {
	h := newReceiptHarness(t, ReceiptWorkerConfig{})
	res, err := h.w.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res != (ReceiptTickResult{}) || len(h.calls) != 0 {
		t.Fatalf("empty queue did work: result=%+v calls=%v", res, h.calls)
	}
}

// Config guards: a max age beyond the provider's retention is pointless (the
// receipt is already deleted), and a window that closes before it opens would
// expire every ticket before it was ever polled.
func TestReceiptWorkerConfigDefaults(t *testing.T) {
	got := ReceiptWorkerConfig{}.withDefaults()
	if got.TickInterval != defaultReceiptTickInterval || got.MinAge != defaultReceiptMinAge ||
		got.MaxAge != defaultReceiptMaxAge || got.BatchSize != defaultReceiptBatchSize ||
		got.MaxPerTick != defaultReceiptMaxPerTick {
		t.Fatalf("zero config = %+v, want the documented defaults", got)
	}
	if got := (ReceiptWorkerConfig{MaxAge: 72 * time.Hour}).withDefaults(); got.MaxAge != defaultReceiptMaxAge {
		t.Fatalf("MaxAge = %v, want it clamped to the provider's 24h retention", got.MaxAge)
	}
	if got := (ReceiptWorkerConfig{MinAge: 2 * time.Hour, MaxAge: time.Hour}).withDefaults(); got.MaxAge <= got.MinAge {
		t.Fatalf("inverted window survived: %+v", got)
	}
	if got := (ReceiptWorkerConfig{BatchSize: 9000}).withDefaults(); got.BatchSize != maxReceiptBatch {
		t.Fatalf("BatchSize = %d, want it clamped to %d", got.BatchSize, maxReceiptBatch)
	}
}

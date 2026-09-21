package kwaakasync

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestTick_UpsertsAndStopLists(t *testing.T) {
	restaurantID := uuid.New()
	restaurants := &fakeRestaurants{linked: []domain.KwaakaLinkedRestaurant{
		{RestaurantID: restaurantID, KwaakaRestaurantID: "kw-1"},
	}}
	fetcher := &fakeFetcher{menus: map[string]domain.KwaakaMenu{
		"kw-1": {Products: []domain.KwaakaMenuProduct{
			{ExternalID: "p1", Name: "Наггетсы", Price: "2890.00", IsAvailable: true, Category: "Основное"},
			{ExternalID: "p2", Name: "Стоп", Price: "1500.00", IsAvailable: false},
		}},
	}}
	items := newFakeItems()

	w := NewWorker(restaurants, fetcher, items, inlineTx{}, Config{}, discardLog())
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if fetcher.calls[0] != "kw-1" {
		t.Fatalf("fetched %v, want [kw-1]", fetcher.calls)
	}
	if len(items.upserted) != 2 {
		t.Fatalf("upserted %d items, want 2", len(items.upserted))
	}
	got := items.upserted[items.key(restaurantID, "p1")]
	if got.Name != "Наггетсы" || got.Price != "2890.00" || !got.IsAvailable || *got.Category != "Основное" {
		t.Errorf("p1 upserted wrong: %+v", got)
	}
	if got2 := items.upserted[items.key(restaurantID, "p2")]; got2.IsAvailable {
		t.Errorf("p2 should be upserted as unavailable, got %+v", got2)
	}

	if len(items.stopCalls) != 1 {
		t.Fatalf("stop calls = %d, want 1", len(items.stopCalls))
	}
	sc := items.stopCalls[0]
	if sc.restaurantID != restaurantID {
		t.Errorf("stop call restaurant = %v, want %v", sc.restaurantID, restaurantID)
	}
	if len(sc.keep) != 2 {
		t.Errorf("stop call keep = %v, want both p1 and p2 (both present in this sync)", sc.keep)
	}
}

// A restaurant no longer part of the fresh menu must fall out — this asserts
// the KEEP set only ever contains the ids from THIS pass' fetch, never a
// previous one.
func TestTick_KeepSetIsOnlyCurrentPass(t *testing.T) {
	restaurantID := uuid.New()
	restaurants := &fakeRestaurants{linked: []domain.KwaakaLinkedRestaurant{
		{RestaurantID: restaurantID, KwaakaRestaurantID: "kw-1"},
	}}
	fetcher := &fakeFetcher{menus: map[string]domain.KwaakaMenu{
		"kw-1": {Products: []domain.KwaakaMenuProduct{
			{ExternalID: "only-this-one", Name: "Only", Price: "100.00", IsAvailable: true},
		}},
	}}
	items := newFakeItems()
	w := NewWorker(restaurants, fetcher, items, inlineTx{}, Config{}, discardLog())
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if keep := items.stopCalls[0].keep; len(keep) != 1 || keep[0] != "only-this-one" {
		t.Fatalf("keep = %v, want [only-this-one]", keep)
	}
}

// One restaurant's fetch failure must not stop the pass from syncing the
// others, and Tick must still report the error to the caller (so Run logs
// it) rather than swallowing it silently.
func TestTick_OneRestaurantFailureDoesNotStopOthers(t *testing.T) {
	r1, r2 := uuid.New(), uuid.New()
	restaurants := &fakeRestaurants{linked: []domain.KwaakaLinkedRestaurant{
		{RestaurantID: r1, KwaakaRestaurantID: "broken"},
		{RestaurantID: r2, KwaakaRestaurantID: "ok"},
	}}
	fetcher := &fakeFetcher{
		menus: map[string]domain.KwaakaMenu{
			"ok": {Products: []domain.KwaakaMenuProduct{{ExternalID: "p1", Name: "A", Price: "1.00", IsAvailable: true}}},
		},
		errs: map[string]error{"broken": domain.ErrUnavailable},
	}
	items := newFakeItems()
	w := NewWorker(restaurants, fetcher, items, inlineTx{}, Config{}, discardLog())

	err := w.Tick(context.Background())
	if err == nil {
		t.Fatal("Tick should report the failed restaurant's error")
	}
	if len(items.upserted) != 1 {
		t.Fatalf("the healthy restaurant should still have synced, got %d items", len(items.upserted))
	}
	if _, ok := items.upserted[items.key(r2, "p1")]; !ok {
		t.Fatal("restaurant r2's dish was not upserted despite r1 failing")
	}
}

// A malformed price must never reach UpsertFromKwaaka — the guard belongs to
// the worker, not just the adapter, since a future menu source could hand the
// worker a bad value directly.
func TestSyncRestaurant_SkipsMalformedPrice(t *testing.T) {
	restaurantID := uuid.New()
	fetcher := &fakeFetcher{menus: map[string]domain.KwaakaMenu{
		"kw-1": {Products: []domain.KwaakaMenuProduct{
			{ExternalID: "bad", Name: "Bad price", Price: "not-a-number", IsAvailable: true},
		}},
	}}
	items := newFakeItems()
	w := NewWorker(&fakeRestaurants{}, fetcher, items, inlineTx{}, Config{}, discardLog())

	res, err := w.syncRestaurant(context.Background(), domain.KwaakaLinkedRestaurant{
		RestaurantID: restaurantID, KwaakaRestaurantID: "kw-1",
	})
	if err != nil {
		t.Fatalf("syncRestaurant: %v", err)
	}
	if res.Skipped != 1 || res.Upserted != 0 {
		t.Fatalf("res = %+v, want Skipped=1 Upserted=0", res)
	}
	if len(items.upserted) != 0 {
		t.Fatalf("malformed-price product must not be upserted, got %v", items.upserted)
	}
}

// A write failure mid-restaurant must abort that restaurant's transaction
// (the fake tx just calls fn, but the returned error must propagate) so a
// half-applied sync is never committed.
func TestSyncRestaurant_WriteFailureAborts(t *testing.T) {
	fetcher := &fakeFetcher{menus: map[string]domain.KwaakaMenu{
		"kw-1": {Products: []domain.KwaakaMenuProduct{
			{ExternalID: "boom", Name: "Boom", Price: "1.00", IsAvailable: true},
		}},
	}}
	items := newFakeItems()
	items.upsertErrOn = "boom"
	w := NewWorker(&fakeRestaurants{}, fetcher, items, inlineTx{}, Config{}, discardLog())

	_, err := w.syncRestaurant(context.Background(), domain.KwaakaLinkedRestaurant{
		RestaurantID: uuid.New(), KwaakaRestaurantID: "kw-1",
	})
	if err == nil {
		t.Fatal("expected the write failure to propagate")
	}
	if len(items.stopCalls) != 0 {
		t.Fatal("MarkUnavailableExceptKwaakaIDs must not run after an upsert failure")
	}
}

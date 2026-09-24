package kwaaka

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"backend-core/internal/domain"
)

func snap() domain.KitchenSnapshot {
	return domain.KitchenSnapshot{OrderID: "11111111-1111-1111-1111-111111111111", TableID: "T1",
		CustomerName: "Гость", CustomerPhone: "+77771234567", Comment: "BookEat · бронь abc",
		Items: []domain.KitchenSnapshotItem{{ProductID: "p1", Name: "Наггетсы", Quantity: 2, PriceMinor: 289000, Currency: "KZT"}}}
}

func TestBuildTableOrderBodyGolden(t *testing.T) {
	got, err := BuildTableOrderBody(snap())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/table_order.golden.json")
	if strings.TrimSpace(string(want)) != string(got) {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
	again, _ := BuildTableOrderBody(snap())
	if string(again) != string(got) {
		t.Fatal("body must be byte-identical across calls (retries)")
	}
}

func TestCreateTableOrderClassification(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    domain.PosCallOutcome
		wantID  string
	}{
		{"200", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`"pos-1"`)) }, domain.PosCreated, "pos-1"},
		{"200 garbage", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<<<`)) }, domain.PosCreated, ""},
		{"400", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }, domain.PosRejected, ""},
		{"404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }, domain.PosRejected, ""},
		{"401", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }, domain.PosAuth, ""},
		{"403", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }, domain.PosAuth, ""},
		{"429", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }, domain.PosRateLimit, ""},
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, domain.PosUnknown, ""},
		{"drop", func(w http.ResponseWriter, r *http.Request) {
			hj := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
		}, domain.PosUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.handler(w, r)
			}))
			defer srv.Close()
			res := NewOrderPOS(testClient(t, srv)).CreateTableOrder(context.Background(), "rest-1", snap())
			if res.Outcome != tc.want || res.PosOrderID != tc.wantID {
				t.Fatalf("got %+v", res)
			}
			if calls.Load() != 1 {
				t.Fatalf("POST must be a single attempt, got %d", calls.Load())
			}
		})
	}
}

func TestCreateTableOrderTimeoutIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // server only notices a dropped client after the body is read
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := testClient(t, srv)
	c.cfg.Timeout = 50 * 1e6
	res := NewOrderPOS(c).CreateTableOrder(context.Background(), "rest-1", snap())
	if res.Outcome != domain.PosUnknown {
		t.Fatalf("timeout must be unknown, got %+v", res)
	}
}

func TestCreateTableOrderRequestShape(t *testing.T) {
	var gotPath, gotAuth, gotService, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotService = r.URL.Path, r.Header.Get("Authorization"), r.URL.Query().Get("service")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`"x"`))
	}))
	defer srv.Close()
	NewOrderPOS(testClient(t, srv)).CreateTableOrder(context.Background(), "rest-1", snap())
	if gotPath != "/v1/table-booking/restaurants/rest-1/order" || gotAuth != "test-token" || gotService != "bookeat" {
		t.Fatalf("path=%q auth=%q service=%q", gotPath, gotAuth, gotService)
	}
	want, _ := os.ReadFile("testdata/table_order.golden.json")
	if gotBody != strings.TrimSpace(string(want)) {
		t.Fatalf("body %s", gotBody)
	}
}

func TestCancelAndOrdersEndpoints(t *testing.T) {
	var cancelBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/order/cancel"):
			b, _ := io.ReadAll(r.Body)
			cancelBody = string(b)
		case strings.HasSuffix(r.URL.Path, "/orders"):
			if ids := r.URL.Query()["tableIds"]; len(ids) == 2 {
				_, _ = w.Write([]byte(`{"orders":[{"id":"o1","table_ids":["A"],"status":"New"}]}`))
				return
			}
			switch r.URL.Query().Get("orderId") {
			case "o-closed":
				_, _ = w.Write([]byte(`{"orders":[{"id":"o-closed","status":"weird","when_closed":"2026-09-24T12:00:00Z"}]}`))
			case "o-none":
				_, _ = w.Write([]byte(`{"orders":[]}`))
			default:
				w.WriteHeader(400)
			}
		case strings.HasSuffix(r.URL.Path, "/tables"):
			_, _ = w.Write([]byte(`{"tables":[{"id":"T1","number":5,"name":"BookEat 1","seating_capacity":4,"section_name":"Зал"}]}`))
		}
	}))
	defer srv.Close()
	pos := NewOrderPOS(testClient(t, srv))
	ctx := context.Background()

	if r := pos.CancelTableOrder(ctx, "rest-1", "o1", "guest_cancelled"); r.Outcome != domain.PosCreated {
		t.Fatalf("cancel: %+v", r)
	}
	if cancelBody != `{"order_id":"o1","cancel_reason":"guest_cancelled"}` {
		t.Fatalf("cancel body %s", cancelBody)
	}
	orders, err := pos.ListOrdersByTables(ctx, "rest-1", []string{"A", "B"})
	if err != nil || len(orders) != 1 || orders[0].State != domain.PosStateOpen {
		t.Fatalf("list: %+v %v", orders, err)
	}
	o, err := pos.GetOrder(ctx, "rest-1", "o-closed")
	if err != nil || o.State != domain.PosStateClosed || o.Raw != "weird" {
		t.Fatalf("closed by when_closed: %+v %v", o, err)
	}
	if _, err := pos.GetOrder(ctx, "rest-1", "o-none"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty orders must be ErrNotFound: %v", err)
	}
	if _, err := pos.GetOrder(ctx, "rest-1", "o-400"); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("400 must be ErrUnavailable: %v", err)
	}
	tables, err := pos.GetTables(ctx, "rest-1")
	if err != nil || len(tables) != 1 || tables[0].SectionName != "Зал" {
		t.Fatalf("tables: %+v %v", tables, err)
	}
}

func TestMapPosStatusUnknownIsUnknown(t *testing.T) {
	if MapPosStatus("some-new-status") != domain.PosStateUnknown {
		t.Fatal("unfamiliar status must map to unknown")
	}
	if MapPosStatus(" Closed ") != domain.PosStateClosed {
		t.Fatal("case/space insensitive")
	}
}

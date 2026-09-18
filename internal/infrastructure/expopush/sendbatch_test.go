package expopush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"backend-core/internal/usecase/notifications"
)

// TestSendBatchChunksAt100 pins criterion 15: 250 tokens must become exactly 3
// Expo requests (100 + 100 + 50), and the results must map back positionally —
// result[i] answers msgs[i], regardless of chunk boundaries.
func TestSendBatchChunksAt100(t *testing.T) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		var in []pushMessage
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if len(in) > defaultMaxBatchSize {
			t.Fatalf("chunk of %d exceeds defaultMaxBatchSize %d", len(in), defaultMaxBatchSize)
		}
		out := make([]map[string]any, len(in))
		for i, m := range in {
			// Ticket id echoes the token so the test can verify positional
			// alignment once results come back flattened across chunks.
			out[i] = map[string]any{"status": "ok", "id": "ticket-for-" + m.To}
		}
		body, _ := json.Marshal(map[string]any{"data": out})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	s := NewSender(Config{Endpoint: srv.URL})
	const total = 250
	msgs := make([]BatchMessage, total)
	for i := range msgs {
		msgs[i] = BatchMessage{Token: fmt.Sprintf("token-%03d", i), Title: "t", Body: "b"}
	}

	results, err := s.SendBatch(context.TODO(), msgs)
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("expo requests = %d, want 3 (100+100+50)", got)
	}
	if len(results) != total {
		t.Fatalf("results = %d, want %d", len(results), total)
	}
	for i, r := range results {
		want := "ticket-for-" + msgs[i].Token
		if r.Verdict != notifications.MobilePushDelivered || r.TicketID != want {
			t.Fatalf("result[%d] = %+v, want delivered with ticket %q", i, r, want)
		}
	}
}

// TestSendBatchStopsAtFailingChunkButKeepsEarlierResults pins the partial-
// progress contract SendBatch documents: a chunk that fails transiently stops
// the call, but every message from a chunk that already succeeded keeps its
// real verdict rather than being reported as if never attempted.
func TestSendBatchStopsAtFailingChunkButKeepsEarlierResults(t *testing.T) {
	var call int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&call, 1)
		if n == 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var in []pushMessage
		_ = json.NewDecoder(r.Body).Decode(&in)
		out := make([]map[string]any, len(in))
		for i := range in {
			out[i] = map[string]any{"status": "ok", "id": "t" + strconv.Itoa(int(n))}
		}
		body, _ := json.Marshal(map[string]any{"data": out})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	s := NewSender(Config{Endpoint: srv.URL})
	msgs := make([]BatchMessage, 250)
	for i := range msgs {
		msgs[i] = BatchMessage{Token: fmt.Sprintf("tok-%d", i)}
	}
	results, err := s.SendBatch(context.TODO(), msgs)
	if err == nil {
		t.Fatal("want an error from the failing second chunk")
	}
	if len(results) != len(msgs) {
		t.Fatalf("results = %d, want %d (always full length)", len(results), len(msgs))
	}
	for i := 0; i < 100; i++ {
		if results[i].Verdict != notifications.MobilePushDelivered {
			t.Fatalf("result[%d] from the SUCCEEDING first chunk = %+v, want delivered", i, results[i])
		}
	}
	for i := 100; i < 250; i++ {
		if results[i].Verdict != notifications.MobilePushRejected {
			t.Fatalf("result[%d] from/after the failing chunk = %+v, want rejected (never attempted)", i, results[i])
		}
	}

	var batchErr *SendBatchError
	if !errors.As(err, &batchErr) {
		t.Fatalf("err = %v, want a *SendBatchError", err)
	}
	if !batchErr.Definite {
		t.Fatalf("batchErr = %+v, want Definite=true — a 502 is Expo itself answering, safe to retry", batchErr)
	}
	if batchErr.FailedAt != 100 || batchErr.FailedThrough != 200 {
		t.Fatalf("batchErr range = [%d:%d], want [100:200] (the second, failing chunk)", batchErr.FailedAt, batchErr.FailedThrough)
	}
}

// TestSendBatchTransportErrorIsUnknownOutcome pins the bug-review fix: a
// client-side transport failure (timeout, connection reset) happens AFTER the
// request body already left this process, so Expo may have already accepted
// it — the outcome is UNKNOWN, not a definite non-delivery, and callers must
// not treat it the same as a 429/5xx.
//
// This deliberately does NOT use httptest.NewServer: an http.Server whose
// handler blocks without ever reading the request body never starts the
// background read that lets Go detect the client going away, so
// Request.Context() never fires and (*httptest.Server).Close's own
// wait-for-active-connections shutdown hangs forever (empirically: the full
// package deadlocked at go test's 10-minute timeout). A raw listener that we
// just close, with no graceful-shutdown handshake, has no such wait.
func TestSendBatchTransportErrorIsUnknownOutcome(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by the deferred Close above
			}
			// Accept the connection but never write a response — the
			// client's own short timeout below fires first, simulating a
			// connection dropped after the request already left this
			// process. Drain whatever the client sends so it isn't blocked
			// on a full write buffer while it waits out its timeout.
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	s := NewSender(Config{Endpoint: "http://" + ln.Addr().String(), Timeout: 10 * time.Millisecond})
	msgs := []BatchMessage{{Token: "tok-1"}, {Token: "tok-2"}}
	results, err := s.SendBatch(context.TODO(), msgs)
	if err == nil {
		t.Fatal("want an error from the timed-out request")
	}
	for i, r := range results {
		if r.Verdict != notifications.MobilePushRejected {
			t.Fatalf("result[%d] = %+v, want the rejected placeholder (no real verdict exists)", i, r)
		}
	}

	var batchErr *SendBatchError
	if !errors.As(err, &batchErr) {
		t.Fatalf("err = %v, want a *SendBatchError", err)
	}
	if batchErr.Definite {
		t.Fatal("batchErr.Definite = true, want false — a client timeout does not tell us Expo rejected anything")
	}
	if batchErr.FailedAt != 0 || batchErr.FailedThrough != len(msgs) {
		t.Fatalf("batchErr range = [%d:%d], want [0:%d] (the whole, only chunk)", batchErr.FailedAt, batchErr.FailedThrough, len(msgs))
	}
}

// TestSendBatchMalformedResponseIsUnknownOutcome covers the other unknown-
// outcome case from the review: Expo answers 2xx (so it did receive the
// request), but the body cannot be parsed — still not a definite answer.
func TestSendBatchMalformedResponseIsUnknownOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	}))
	defer srv.Close()

	s := NewSender(Config{Endpoint: srv.URL})
	_, err := s.SendBatch(context.TODO(), []BatchMessage{{Token: "tok-1"}})

	var batchErr *SendBatchError
	if !errors.As(err, &batchErr) {
		t.Fatalf("err = %v, want a *SendBatchError", err)
	}
	if batchErr.Definite {
		t.Fatal("batchErr.Definite = true, want false — an unparseable 2xx body tells us nothing about delivery")
	}
}

// TestSendBatchIncludesChannelID pins criterion 21's wire shape: the
// channelId travels in the request body exactly as given, and an empty one is
// omitted rather than sent as "".
func TestSendBatchIncludesChannelID(t *testing.T) {
	var got []pushMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, `{"data":[{"status":"ok","id":"a"},{"status":"ok","id":"b"}]}`)
	}))
	defer srv.Close()

	s := NewSender(Config{Endpoint: srv.URL})
	_, err := s.SendBatch(context.TODO(), []BatchMessage{
		{Token: "t1", ChannelID: "offers"},
		{Token: "t2"},
	})
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expo saw %d messages, want 2", len(got))
	}
	if got[0].ChannelID != "offers" {
		t.Fatalf("message 0 channelId = %q, want offers", got[0].ChannelID)
	}
	if got[1].ChannelID != "" {
		t.Fatalf("message 1 channelId = %q, want empty (omitted)", got[1].ChannelID)
	}
}

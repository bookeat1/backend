package expopush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

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

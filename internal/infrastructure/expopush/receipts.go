package expopush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"backend-core/internal/usecase/notifications"
)

// maxReceiptIDs is Expo's documented cap on ids per getReceipts call; more is
// answered with PUSH_TOO_MANY_RECEIPTS. The caller batches, so exceeding it here
// is a programming error and is reported as one rather than silently truncated —
// truncating would drop tickets that then never get polled again.
const maxReceiptIDs = 1000

// receiptsRequest is the getReceipts body: {"ids": [...]}.
type receiptsRequest struct {
	IDs []string `json:"ids"`
}

// receiptsResponse is Expo's receipt envelope. Unlike the send response, `data`
// is an OBJECT keyed by ticket id — and it only contains the tickets Expo
// already has a receipt for. A ticket that is still in flight is simply ABSENT
// from the map (observed live on 2026-09-01), which is why this type must never
// be read as "every id I asked about is in here".
type receiptsResponse struct {
	Data map[string]struct {
		Status  string `json:"status"` // "ok" | "error"
		Message string `json:"message"`
		Details struct {
			Error string `json:"error"` // DeviceNotRegistered, MessageTooBig, …
		} `json:"details"`
	} `json:"data"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Receipts fetches the final per-device outcome for a batch of ticket ids.
//
// Mapping:
//
//	status "ok"                                → Delivered
//	status "error" + DeviceNotRegistered       → DeviceGone (deactivate token)
//	status "error" + anything else             → Rejected (token untouched)
//	id absent from the response                → NOT in the returned map
//	HTTP 429 / 5xx / transport failure         → error (transient, retried)
//
// The returned map contains ONLY the ids Expo answered about. The caller must
// treat a missing id as "no receipt yet" and ask again later.
//
// Reason carries details.error, never Expo's message text: the message can quote
// the push token, and a push token is a device credential that must not reach a
// log (the same discipline as phone masking elsewhere in this repo).
func (s *Sender) Receipts(ctx context.Context, ids []string) (map[string]notifications.MobilePushReceipt, error) {
	if len(ids) == 0 {
		return map[string]notifications.MobilePushReceipt{}, nil
	}
	if len(ids) > maxReceiptIDs {
		return nil, fmt.Errorf("expo receipts: %d ids exceed the limit of %d per request", len(ids), maxReceiptIDs)
	}

	body, err := json.Marshal(receiptsRequest{IDs: ids})
	if err != nil {
		return nil, fmt.Errorf("expo receipts: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.receiptsEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, s.scrub(fmt.Errorf("expo receipts: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, s.scrub(fmt.Errorf("expo receipts: send: %w", err))
	}
	defer resp.Body.Close()
	// Bounded read. A full 1000-ticket answer is a few hundred kilobytes; the
	// cap is generous but finite, because an unbounded ReadAll on a misbehaving
	// endpoint is a memory hazard.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, s.scrub(fmt.Errorf("expo receipts: read response: %w", err))
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// Transient: the worker leaves the whole batch unresolved for a later
		// tick.
		return nil, fmt.Errorf("expo receipts: status %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 4xx other than 429 (bad access token, malformed batch). The body is
		// NOT echoed: it can quote ticket ids and provider text back.
		return nil, fmt.Errorf("expo receipts: status %d", resp.StatusCode)
	}

	var out receiptsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("expo receipts: decode response: %w", err)
	}
	if len(out.Data) == 0 && len(out.Errors) > 0 {
		// Expo refused the whole request. Only the code is logged — the message
		// is provider text.
		return nil, fmt.Errorf("expo receipts: request refused (code %q)", out.Errors[0].Code)
	}

	res := make(map[string]notifications.MobilePushReceipt, len(out.Data))
	for id, r := range out.Data {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		switch {
		case r.Status == "ok":
			res[id] = notifications.MobilePushReceipt{Verdict: notifications.MobilePushDelivered}
		case r.Details.Error == "DeviceNotRegistered":
			res[id] = notifications.MobilePushReceipt{
				Verdict: notifications.MobilePushDeviceGone,
				Reason:  r.Details.Error,
			}
		default:
			// Any other error, including a status Expo may add later: the
			// message is the problem, not the device. Reason falls back to the
			// status itself so an unknown shape is still greppable, and never
			// to r.Message.
			reason := strings.TrimSpace(r.Details.Error)
			if reason == "" {
				reason = strings.TrimSpace(r.Status)
			}
			res[id] = notifications.MobilePushReceipt{
				Verdict: notifications.MobilePushRejected,
				Reason:  reason,
			}
		}
	}
	return res, nil
}

// Package expopush wraps the Expo push API behind the
// notifications.MobilePushSender seam, so the usecase layer stays free of the
// provider and the network. The BookEat app is Expo/React Native, so Expo's
// hosted service is the pragmatic first target: it fans out to both APNs and
// FCM without the backend holding Apple or Google credentials.
//
// It is deliberately a tiny net/http client rather than the Expo server SDK: the
// notifier needs exactly two calls, POST /--/api/v2/push/send and POST
// /--/api/v2/push/getReceipts. Replacing Expo with direct FCM/APNs later means
// writing a sibling package with the same Send/Receipts signatures — nothing in
// usecase/notifications changes.
package expopush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"backend-core/internal/usecase/notifications"
)

// defaultEndpoint is Expo's push endpoint (docs.expo.dev, "Sending
// notifications with Expo's Push API").
const defaultEndpoint = "https://exp.host/--/api/v2/push/send"

// defaultReceiptsEndpoint is Expo's receipt endpoint. A ticket from
// defaultEndpoint only says the message was ACCEPTED; this is where the
// per-device outcome (delivered / DeviceNotRegistered / …) finally appears.
const defaultReceiptsEndpoint = "https://exp.host/--/api/v2/push/getReceipts"

// Config configures the Expo sender.
type Config struct {
	// AccessToken is Expo's optional push-security token. It is a credential:
	// read from env only (EXPO_ACCESS_TOKEN) and NEVER logged. Expo accepts
	// unauthenticated sends unless push security is enabled on the project, so
	// this may legitimately be empty — Configured() therefore keys off Provider,
	// not off this field.
	AccessToken string
	// Endpoint overrides the Expo send URL (tests, or a future self-hosted relay).
	Endpoint string
	// ReceiptsEndpoint overrides the Expo receipts URL (tests, or a relay).
	ReceiptsEndpoint string
	// Timeout caps one send call.
	Timeout time.Duration
	// MaxBatchSize caps how many messages SendBatch puts in ONE Expo request.
	// Zero/negative uses defaultMaxBatchSize (Expo's own documented ceiling);
	// a value ABOVE it is clamped down to it — this can only ever make a
	// deployment send SMALLER batches than Expo allows, never larger ones a
	// real Expo project would reject. env: PUSH_CAMPAIGNS_SEND_BATCH_SIZE
	// (the only caller that ever sets this today — booking pushes always
	// batch of one, well under any cap).
	MaxBatchSize int
}

// Sender posts one message per call to the Expo push service.
type Sender struct {
	endpoint         string
	receiptsEndpoint string
	accessToken      string
	client           *http.Client
	maxBatchSize     int
}

// NewSender builds an Expo push sender.
func NewSender(cfg Config) *Sender {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	receipts := strings.TrimSpace(cfg.ReceiptsEndpoint)
	if receipts == "" {
		receipts = defaultReceiptsEndpoint
	}
	batchSize := cfg.MaxBatchSize
	if batchSize <= 0 || batchSize > defaultMaxBatchSize {
		batchSize = defaultMaxBatchSize
	}
	return &Sender{
		endpoint:         endpoint,
		receiptsEndpoint: receipts,
		accessToken:      strings.TrimSpace(cfg.AccessToken),
		client:           &http.Client{Timeout: timeout},
		maxBatchSize:     batchSize,
	}
}

// pushMessage is one Expo push message. Only the fields the guest notifier
// actually uses are modelled; Expo ignores what it is not sent.
type pushMessage struct {
	To    string            `json:"to"`
	Title string            `json:"title"`
	Body  string            `json:"body"`
	Data  map[string]string `json:"data,omitempty"`
	Sound string            `json:"sound,omitempty"`
	// ChannelID selects the Android notification channel ("bookings",
	// "offers"). Omitted (empty) leaves Android to its default channel — the
	// behaviour every message had before this field existed.
	ChannelID string `json:"channelId,omitempty"`
}

// pushResponse is Expo's ticket envelope. `data` is an ARRAY because the request
// body is always an array of one — Expo mirrors the request's shape, and posting
// a bare object would make `data` an object instead, so the array form keeps the
// decoding unconditional.
type pushResponse struct {
	Data []struct {
		// ID is the RECEIPT HANDLE. Expo only returns it for an accepted
		// ticket, and it is the only way to ever learn what happened on the
		// device — see the receipt worker.
		ID      string `json:"id"`
		Status  string `json:"status"` // "ok" | "error"
		Message string `json:"message"`
		Details struct {
			Error string `json:"error"` // e.g. "DeviceNotRegistered"
		} `json:"details"`
	} `json:"data"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// defaultMaxBatchSize is Expo's documented ceiling on messages per send
// request (docs.expo.dev, "Batching push notifications") — the default and
// hard upper bound for Sender.maxBatchSize (see Config.MaxBatchSize).
const defaultMaxBatchSize = 100

// Send delivers one message to one Expo push token. It is a thin wrapper over
// SendBatch with a single-element slice (criterion 15 of the push-campaigns
// spec): booking pushes go through the exact same wire format and verdict
// mapping they always have, byte for byte, because they are now simply
// SendBatch's n=1 case rather than a separate code path.
//
// Any error — definite or unknown-outcome alike — is forwarded as-is: this
// preserves booking push's pre-existing "any error is retryable" behaviour
// (usecase/notifications.guestpush) untouched. The definite/unknown
// distinction only matters to a caller fanning out MANY messages in one call
// (push campaigns), because only there does "retry" mean "resend to
// thousands of the same recipients" — see SendBatch and SendBatchError.
func (s *Sender) Send(ctx context.Context, token string, msg notifications.MobilePushMessage) (notifications.MobilePushResult, error) {
	results, err := s.SendBatch(ctx, []BatchMessage{{
		Token: token, Title: msg.Title, Body: msg.Body, Data: msg.Data, ChannelID: msg.ChannelID,
	}})
	if len(results) != 1 {
		// Unreachable given SendBatch's own contract (always len(msgs) results),
		// kept so a future refactor cannot silently panic on results[0] below.
		return rejected(), err
	}
	return results[0], err
}

// BatchMessage is one recipient's push within a SendBatch call.
type BatchMessage struct {
	Token     string
	Title     string
	Body      string
	Data      map[string]string
	ChannelID string
}

// SendBatchError is returned by SendBatch when it could not get an answer for
// every message. Everything in the returned results slice before FailedAt is
// a REAL per-message verdict, from a chunk Expo answered in full; SendBatch
// never attempts a chunk after one that failed, so nothing from FailedAt
// onward was resolved.
//
// Definite says whether msgs[FailedAt:FailedThrough] — the chunk that was
// actually attempted and failed — got a DEFINITE non-delivery answer: Expo
// itself responded (429/5xx, or a 2xx envelope with no tickets at all), so
// nothing in that chunk reached a device and it is safe to retry. When false,
// that same range's outcome is UNKNOWN: a transport error, client timeout or
// connection reset, an unreadable 2xx body, a malformed JSON response, or a
// ticket count that does not match the sent message count — all cases where
// the request may have already reached Expo before this process lost track
// of it. Retrying an unknown-outcome range risks a duplicate push, so a
// caller MUST NOT auto-retry it (spec push-campaigns-manual-spec §7 п.7 /
// §3.11: "лучше недослать, чем прислать дважды").
//
// msgs[FailedThrough:] (any chunk after the failing one) was never even sent
// — SendBatch stops at the first failure — so it is ALWAYS safe to retry,
// independent of Definite.
type SendBatchError struct {
	FailedAt      int
	FailedThrough int
	Definite      bool
	Err           error
}

func (e *SendBatchError) Error() string { return e.Err.Error() }
func (e *SendBatchError) Unwrap() error { return e.Err }

// SendBatch delivers up to len(msgs) messages, chunked internally to Expo's
// own per-request ceiling (s.maxBatchSize) so a push-campaign fan-out of
// thousands of tokens is simply several sequential requests, never one
// oversized one. The returned slice is ALWAYS len(msgs) long and positionally
// aligned with msgs: result[i] answers msgs[i], for every i before the
// failure (see SendBatchError) — every message from the failure onward keeps
// its Rejected placeholder in the slice and MUST be ignored in favour of the
// returned *SendBatchError, which is the only thing that can tell "Expo
// definitely said no" apart from "we don't actually know".
func (s *Sender) SendBatch(ctx context.Context, msgs []BatchMessage) ([]notifications.MobilePushResult, error) {
	out := make([]notifications.MobilePushResult, len(msgs))
	for i := range out {
		out[i] = rejected()
	}
	for start := 0; start < len(msgs); start += s.maxBatchSize {
		end := start + s.maxBatchSize
		if end > len(msgs) {
			end = len(msgs)
		}
		results, definite, err := s.sendChunk(ctx, msgs[start:end])
		if err != nil {
			return out, &SendBatchError{FailedAt: start, FailedThrough: end, Definite: definite, Err: err}
		}
		copy(out[start:start+len(results)], results)
	}
	return out, nil
}

// sendChunk sends AT MOST s.maxBatchSize messages in one Expo request and
// returns one result per input message, positionally aligned — exactly the
// verdict mapping the pre-batch Send used for its single element:
//
//	HTTP 2xx + status "ok"                     → Delivered + the ticket id
//	status "error", details.error DeviceNotRegistered → DeviceGone (deactivate)
//	any other ticket error (e.g. MessageTooBig) → Rejected (not retryable)
//	HTTP 429 / 5xx                              → err, definite=true  (Expo
//	  itself said no — safe to retry the whole chunk)
//	HTTP 2xx with no tickets at all (errors[] populated) → err, definite=true
//	  (Expo answered synchronously that it accepted nothing)
//	transport failure, timeout, unreadable body, malformed JSON, or a ticket
//	  count that does not match the chunk → err, definite=false (the request
//	  may have already reached Expo — outcome UNKNOWN, must not be retried
//	  blindly)
//
// No per-message results are returned on any error path, since a whole-chunk
// failure — definite or not — answers nothing about any individual message
// in it.
//
// TODO(verify): проверить на реальном Expo-проекте, что при
// push-security-токене неверный токен приходит как HTTP 400 с errors[].code
// UNAUTHORIZED, а не как ticket-ошибка.
func (s *Sender) sendChunk(ctx context.Context, chunk []BatchMessage) (results []notifications.MobilePushResult, definite bool, err error) {
	payload := make([]pushMessage, len(chunk))
	for i, m := range chunk {
		payload[i] = pushMessage{To: m.Token, Title: m.Title, Body: m.Body, Data: m.Data, Sound: "default", ChannelID: m.ChannelID}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("expo push: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, s.scrub(fmt.Errorf("expo push: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// The request may have left this process and reached Expo before the
		// timeout/connection error fired — outcome unknown, never definite.
		return nil, false, s.scrub(fmt.Errorf("expo push: send: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// Expo itself answered "try later" — definitely not accepted. The
		// body is drained (not parsed: this class of response is not the
		// JSON ticket envelope) so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, true, fmt.Errorf("expo push: status %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 4xx other than 429: a bad request or a bad access token. Retrying
		// cannot fix it. The response body is NOT echoed into the error — it
		// may quote a push token back. Every message in the chunk is reported
		// Rejected rather than erroring the caller into a pointless retry.
		out := make([]notifications.MobilePushResult, len(chunk))
		for i := range out {
			out[i] = rejected()
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return out, false, nil
	}

	// Bounded read: scales with the chunk (≤100 tiny ticket entries), and an
	// unbounded ReadAll on a misbehaving endpoint is still a memory hazard.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Expo's headers arrived (2xx), but the body never fully did — we
		// cannot know what it would have said, so this is NOT a definite
		// non-delivery.
		return nil, false, s.scrub(fmt.Errorf("expo push: read response: %w", err))
	}

	var decoded pushResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// A 2xx body that fails to parse tells us nothing about delivery —
		// unknown, not definite.
		return nil, false, fmt.Errorf("expo push: decode response: %w", err)
	}
	if len(decoded.Data) == 0 {
		// A 2xx with no tickets is Expo refusing the whole request (errors[]
		// is populated) — Expo DID answer, definitively, so this chunk is
		// safe to retry. The code is safe to log, the message may quote a
		// token.
		code := ""
		if len(decoded.Errors) > 0 {
			code = decoded.Errors[0].Code
		}
		return nil, true, fmt.Errorf("expo push: no tickets returned (code %q)", code)
	}
	if len(decoded.Data) != len(chunk) {
		// Cannot map tickets to messages positionally — we do not know which
		// message got which verdict, so nothing here can be called definite.
		return nil, false, fmt.Errorf("expo push: got %d tickets for %d messages, cannot map positionally",
			len(decoded.Data), len(chunk))
	}
	out := make([]notifications.MobilePushResult, len(chunk))
	for i, ticket := range decoded.Data {
		switch {
		case ticket.Status == "ok":
			out[i] = notifications.MobilePushResult{
				Verdict:  notifications.MobilePushDelivered,
				TicketID: strings.TrimSpace(ticket.ID),
			}
		case ticket.Details.Error == "DeviceNotRegistered":
			out[i] = notifications.MobilePushResult{Verdict: notifications.MobilePushDeviceGone}
		default:
			out[i] = rejected()
		}
	}
	return out, false, nil
}

// rejected is the "no ticket to poll" result. Spelled out once so no branch
// accidentally returns a ticket id it does not have.
func rejected() notifications.MobilePushResult {
	return notifications.MobilePushResult{Verdict: notifications.MobilePushRejected}
}

// scrub removes the access token from an error message so the credential can
// never reach a log, even via a wrapped *url.Error.
func (s *Sender) scrub(err error) error {
	if err == nil || s.accessToken == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), s.accessToken, "***"))
}

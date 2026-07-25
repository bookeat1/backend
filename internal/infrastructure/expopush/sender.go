// Package expopush wraps the Expo push API behind the
// notifications.MobilePushSender seam, so the usecase layer stays free of the
// provider and the network. The BookEat app is Expo/React Native, so Expo's
// hosted service is the pragmatic first target: it fans out to both APNs and
// FCM without the backend holding Apple or Google credentials.
//
// It is deliberately a tiny net/http client rather than the Expo server SDK: the
// notifier needs exactly one call, POST /--/api/v2/push/send. Replacing Expo
// with direct FCM/APNs later means writing a sibling package with the same Send
// signature — nothing in usecase/notifications changes.
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

// Config configures the Expo sender.
type Config struct {
	// AccessToken is Expo's optional push-security token. It is a credential:
	// read from env only (EXPO_ACCESS_TOKEN) and NEVER logged. Expo accepts
	// unauthenticated sends unless push security is enabled on the project, so
	// this may legitimately be empty — Configured() therefore keys off Provider,
	// not off this field.
	AccessToken string
	// Endpoint overrides the Expo URL (tests, or a future self-hosted relay).
	Endpoint string
	// Timeout caps one send call.
	Timeout time.Duration
}

// Sender posts one message per call to the Expo push service.
type Sender struct {
	endpoint    string
	accessToken string
	client      *http.Client
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
	return &Sender{
		endpoint:    endpoint,
		accessToken: strings.TrimSpace(cfg.AccessToken),
		client:      &http.Client{Timeout: timeout},
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
}

// pushResponse is Expo's ticket envelope. `data` is an ARRAY because the request
// body is always an array of one — Expo mirrors the request's shape, and posting
// a bare object would make `data` an object instead, so the array form keeps the
// decoding unconditional.
type pushResponse struct {
	Data []struct {
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

// Send delivers one message to one Expo push token.
//
// Verdict mapping (per Expo's documented ticket format):
//
//	HTTP 2xx + status "ok"                     → Delivered
//	status "error", details.error DeviceNotRegistered → DeviceGone (deactivate)
//	any other ticket error (e.g. MessageTooBig) → Rejected (not retryable)
//	HTTP 429 / 5xx / transport failure          → error (transient, retried)
//
// A ticket only means Expo ACCEPTED the message; the final per-device outcome
// lives behind the getReceipts endpoint, which is deliberately not implemented
// here — see the package TODO below.
//
// TODO(verify): проверить на реальном Expo-проекте — (1) что при
// push-security-токене неверный токен приходит как HTTP 400 с errors[].code
// UNAUTHORIZED, а не как ticket-ошибка; (2) нужен ли нам pull получателей
// (getReceipts), чтобы ловить отложенный DeviceNotRegistered — сейчас мёртвый
// токен гасится только если Expo сообщает о нём уже в тикете.
func (s *Sender) Send(ctx context.Context, token string, msg notifications.MobilePushMessage) (notifications.MobilePushVerdict, error) {
	body, err := json.Marshal([]pushMessage{{
		To:    token,
		Title: msg.Title,
		Body:  msg.Body,
		Data:  msg.Data,
		Sound: "default",
	}})
	if err != nil {
		return notifications.MobilePushRejected, fmt.Errorf("expo push: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return notifications.MobilePushRejected, s.scrub(fmt.Errorf("expo push: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return notifications.MobilePushRejected, s.scrub(fmt.Errorf("expo push: send: %w", err))
	}
	defer resp.Body.Close()
	// Bounded read: the ticket envelope for one message is tiny, and an
	// unbounded ReadAll on a misbehaving endpoint is a memory hazard.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return notifications.MobilePushRejected, s.scrub(fmt.Errorf("expo push: read response: %w", err))
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// Transient — the notifier leaves the outbox event for the next tick.
		return notifications.MobilePushRejected, fmt.Errorf("expo push: status %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 4xx other than 429: a bad request or a bad access token. Retrying
		// cannot fix it; the notifier logs and drains. The response body is NOT
		// echoed into the error — it may quote the push token back.
		return notifications.MobilePushRejected, nil
	}

	var out pushResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return notifications.MobilePushRejected, fmt.Errorf("expo push: decode response: %w", err)
	}
	if len(out.Data) == 0 {
		// A 2xx with no ticket is Expo refusing the whole request (errors[] is
		// populated). Treat as non-retryable; the code is safe to log, the
		// message may quote the token.
		code := ""
		if len(out.Errors) > 0 {
			code = out.Errors[0].Code
		}
		return notifications.MobilePushRejected, fmt.Errorf("expo push: no ticket returned (code %q)", code)
	}
	ticket := out.Data[0]
	if ticket.Status == "ok" {
		return notifications.MobilePushDelivered, nil
	}
	if ticket.Details.Error == "DeviceNotRegistered" {
		return notifications.MobilePushDeviceGone, nil
	}
	return notifications.MobilePushRejected, nil
}

// scrub removes the access token from an error message so the credential can
// never reach a log, even via a wrapped *url.Error.
func (s *Sender) scrub(err error) error {
	if err == nil || s.accessToken == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), s.accessToken, "***"))
}

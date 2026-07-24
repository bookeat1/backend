// Package amplitude is a tiny net/http client for Amplitude's HTTP V2 API
// (POST /2/httpapi), wrapped behind the analytics.Sender seam so the usecase
// layer stays free of the transport. It is deliberately not an SDK: the worker
// needs exactly one call, a batch upload.
package amplitude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"backend-core/internal/usecase/analytics"
)

// DefaultEndpoint is Amplitude's US HTTP V2 batch-ingest endpoint. The EU
// endpoint (api.eu.amplitude.com) is set via AMPLITUDE_ENDPOINT if the project
// lives in the EU data centre.
const DefaultEndpoint = "https://api2.amplitude.com/2/httpapi"

// Config holds the Amplitude project credentials and transport tuning. The API
// key is read from env only and is NEVER logged (same discipline as acquirer
// keys / VAPID / bot tokens). Configured reports whether a real client can be
// built.
type Config struct {
	APIKey   string        // env: AMPLITUDE_API_KEY
	Endpoint string        // env: AMPLITUDE_ENDPOINT (defaults to DefaultEndpoint)
	Timeout  time.Duration // env: ANALYTICS_HTTP_TIMEOUT
}

// Configured reports whether an API key is present. When false the caller runs
// analytics with a no-op sender instead of building this client.
func (c Config) Configured() bool { return strings.TrimSpace(c.APIKey) != "" }

// Client posts event batches to the Amplitude HTTP V2 API.
type Client struct {
	apiKey   string
	endpoint string
	client   *http.Client
	log      *slog.Logger
}

var _ analytics.Sender = (*Client)(nil)

// NewClient builds an Amplitude client. Callers should only build one when
// cfg.Configured() is true.
func NewClient(cfg Config, log *slog.Logger) *Client {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		apiKey:   strings.TrimSpace(cfg.APIKey),
		endpoint: endpoint,
		client:   &http.Client{Timeout: timeout},
		log:      log,
	}
}

// apiEvent is one Amplitude event object. Field names follow the HTTP V2 spec.
// time is milliseconds since epoch; insert_id is the dedupe key.
type apiEvent struct {
	UserID          string         `json:"user_id,omitempty"`
	DeviceID        string         `json:"device_id,omitempty"`
	EventType       string         `json:"event_type"`
	Time            int64          `json:"time"`
	InsertID        string         `json:"insert_id"`
	EventProperties map[string]any `json:"event_properties,omitempty"`
}

type apiRequest struct {
	APIKey string     `json:"api_key"`
	Events []apiEvent `json:"events"`
}

// Send uploads a batch. Per the analytics.Sender contract it returns a non-nil
// error ONLY for a transient failure worth retrying (network error, HTTP
// 429/5xx). A permanent rejection (HTTP 400/413) is logged and swallowed with a
// nil error so one poison batch never blocks the analytics stream forever.
func (c *Client) Send(ctx context.Context, batch []analytics.Event) error {
	if len(batch) == 0 {
		return nil
	}
	events := make([]apiEvent, 0, len(batch))
	for _, e := range batch {
		events = append(events, apiEvent{
			UserID:          e.UserID,
			DeviceID:        e.DeviceID,
			EventType:       string(e.Type),
			Time:            e.Time.UnixMilli(),
			InsertID:        e.InsertID,
			EventProperties: e.Properties,
		})
	}
	body, err := json.Marshal(apiRequest{APIKey: c.apiKey, Events: events})
	if err != nil {
		return fmt.Errorf("amplitude: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("amplitude: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		// Network/timeout — transient, retry.
		return fmt.Errorf("amplitude: send: %w", err)
	}
	defer resp.Body.Close()
	// Read a bounded amount of the body for diagnostics, then discard the rest
	// so the connection can be reused.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500:
		// 429 (throttled) and 5xx are transient — retry next tick.
		return fmt.Errorf("amplitude: transient status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	default:
		// 400 (bad payload), 413 (too big) and any other 4xx are permanent for
		// this batch: retrying identical bytes cannot succeed. Drop with a loud
		// log rather than block every later event behind it.
		c.log.Error("amplitude: batch permanently rejected, dropping",
			slog.Int("status", resp.StatusCode),
			slog.Int("events", len(batch)),
			slog.String("response", strings.TrimSpace(string(snippet))))
		return nil
	}
}

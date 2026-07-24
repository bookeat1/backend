// Package telegramnotify wraps the Telegram Bot API sendMessage call behind the
// notifications.TelegramSender seam, so the usecase layer stays free of the
// transport and the network. It is a deliberately tiny client (net/http + one
// JSON body) rather than a heavyweight Telegram library — the notifier needs
// exactly one method, POST sendMessage.
package telegramnotify

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
)

// Config holds the notifications bot token. It is read from env only
// (TELEGRAM_NOTIFY_BOT_TOKEN) and is NEVER logged — the token is a bot
// credential (same discipline as acquirer keys / VAPID). Configured reports
// whether a real sender can be built.
type Config struct {
	BotToken string
	// Timeout caps one sendMessage call.
	Timeout time.Duration
}

// Configured reports whether a bot token is present. When false the caller runs
// the telegram channel as a clean no-op.
func (c Config) Configured() bool { return strings.TrimSpace(c.BotToken) != "" }

// Sender posts messages via the Telegram Bot API.
type Sender struct {
	token   string
	baseURL string // override for tests; defaults to https://api.telegram.org
	client  *http.Client
}

// NewSender builds a Telegram sender. Callers should only build one when
// cfg.Configured() is true.
func NewSender(cfg Config) *Sender {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Sender{
		token:   strings.TrimSpace(cfg.BotToken),
		baseURL: "https://api.telegram.org",
		client:  &http.Client{Timeout: timeout},
	}
}

type sendMessageRequest struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

// Send delivers text to chatID and returns the Bot API HTTP status code. A 2xx
// means accepted; 400/403 mean a bad/blocked chat (not retryable); other codes
// (429/5xx) are transient the caller retries.
//
// The bot token lives in the request URL. Every error returned here is scrubbed
// of the token before it can reach a log line: a raw *url.Error would otherwise
// embed the full URL (token included).
func (s *Sender) Send(ctx context.Context, chatID, text string) (int, error) {
	body, err := json.Marshal(sendMessageRequest{ChatID: chatID, Text: text})
	if err != nil {
		return 0, fmt.Errorf("telegram: marshal request: %w", err)
	}
	url := s.baseURL + "/bot" + s.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, s.scrub(fmt.Errorf("telegram: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, s.scrub(fmt.Errorf("telegram: sendMessage: %w", err))
	}
	defer resp.Body.Close()
	// Drain and discard: we key off the status code only, but leaving the body
	// unread would leak the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// scrub replaces the bot token anywhere in an error message with "***" so the
// credential can never reach a log, even via a wrapped *url.Error.
func (s *Sender) scrub(err error) error {
	if err == nil {
		return nil
	}
	if s.token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), s.token, "***")
	return errors.New(msg)
}

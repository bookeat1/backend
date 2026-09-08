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
	ChatID      string       `json:"chat_id"`
	Text        string       `json:"text"`
	ReplyMarkup *replyMarkup `json:"reply_markup,omitempty"`
}

// replyMarkup is the inline keyboard under the alert. Only what the venue
// buttons need: rows of buttons that carry callback data, no URLs, no web apps.
type replyMarkup struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Send delivers text to chatID and returns the Bot API HTTP status code. A 2xx
// means accepted; 400/403 mean a bad/blocked chat (not retryable); other codes
// (429/5xx) are transient the caller retries.
//
// The bot token lives in the request URL. Every error returned here is scrubbed
// of the token before it can reach a log line: a raw *url.Error would otherwise
// embed the full URL (token included).
func (s *Sender) Send(ctx context.Context, chatID, text string) (int, error) {
	return s.send(ctx, sendMessageRequest{ChatID: chatID, Text: text})
}

// SendWithActions delivers text under a one-row inline keyboard. Each action is
// a label and the callback data Telegram sends back when it is pressed; the
// data is opaque here on purpose — this package knows how to talk to Telegram,
// not what a booking is.
//
// Telegram caps callback_data at 64 bytes. An action over the limit is dropped
// rather than sent, because Telegram answers the whole sendMessage with a 400
// and the venue would get NO alert at all instead of an alert without one
// button.
func (s *Sender) SendWithActions(ctx context.Context, chatID, text string, actions [][2]string) (int, error) {
	row := make([]inlineButton, 0, len(actions))
	for _, a := range actions {
		if len(a[1]) > 64 {
			continue
		}
		row = append(row, inlineButton{Text: a[0], CallbackData: a[1]})
	}
	req := sendMessageRequest{ChatID: chatID, Text: text}
	if len(row) > 0 {
		req.ReplyMarkup = &replyMarkup{InlineKeyboard: [][]inlineButton{row}}
	}
	return s.send(ctx, req)
}

func (s *Sender) send(ctx context.Context, payload sendMessageRequest) (int, error) {
	body, err := json.Marshal(payload)
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

// answerCallbackRequest acknowledges a button press. Telegram shows text as a
// toast over the chat; without this call the button spins until it times out.
type answerCallbackRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

// AnswerCallback acknowledges a press. Errors are returned but are not worth
// failing a webhook over: the decision behind the press is already committed.
func (s *Sender) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return s.post(ctx, "answerCallbackQuery", answerCallbackRequest{CallbackQueryID: callbackID, Text: text})
}

// editMessageTextRequest rewrites a message we sent. Passing no reply_markup
// drops the inline keyboard, which is the point: once a booking is answered the
// buttons must stop inviting a second answer.
type editMessageTextRequest struct {
	ChatID    string `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
}

// EditMessageText rewrites the alert in place and removes its buttons.
func (s *Sender) EditMessageText(ctx context.Context, chatID string, messageID int64, text string) error {
	return s.post(ctx, "editMessageText", editMessageTextRequest{ChatID: chatID, MessageID: messageID, Text: text})
}

// post is the shared plumbing for the fire-and-check calls above: marshal, POST,
// drain, and turn a non-2xx into an error with the token scrubbed out.
func (s *Sender) post(ctx context.Context, method string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/bot"+s.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return s.scrub(fmt.Errorf("telegram: build %s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return s.scrub(fmt.Errorf("telegram: %s: %w", method, err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram: %s got status %d", method, resp.StatusCode)
	}
	return nil
}

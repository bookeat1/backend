package otpsender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// TelegramGatewayConfig configures the PAID Telegram Gateway channel.
//
// This is NOT the notifications bot (telegramnotify) and not a chat message:
// Telegram Gateway delivers a verification code to a PHONE NUMBER inside the
// Telegram app, for any number that has Telegram, with no prior linking and no
// bot /start. The account and its token live at https://gateway.telegram.org
// (Account → API token) and the balance is topped up there.
type TelegramGatewayConfig struct {
	// Token is the Gateway API access token. A credential: env only, never
	// logged, never in an error (httpClient scrubs it).
	Token string
	// CodeTTL is how long Telegram keeps the code valid on its side. Clamped to
	// the API's supported 30…3600 s window; we pass our own OTP TTL so the code
	// does not outlive our otp_codes row (or die before it).
	CodeTTL time.Duration
	// SenderUsername is an OPTIONAL verified channel username the code is sent
	// from. Empty (the default) uses Telegram's own verification sender, which
	// is what an account without a verified channel must use.
	SenderUsername string
	// Timeout caps one HTTP call (both the pre-check and the send).
	Timeout time.Duration
	// BaseURL overrides the API host. Tests only — production leaves it empty.
	BaseURL string
}

// Configured reports whether the channel can be built. No token → the channel
// simply does not exist; that is a configuration state, not an error.
func (c TelegramGatewayConfig) Configured() bool { return trimmed(c.Token) != "" }

// telegramGatewayBaseURL is the documented API host.
const telegramGatewayBaseURL = "https://gatewayapi.telegram.org"

// Telegram Gateway's ttl bounds, from the API docs ("supported values are from
// 30 to 3600"). A value outside them is rejected, so we clamp rather than let a
// configuration choice break every login.
const (
	telegramMinTTL = 30 * time.Second
	telegramMaxTTL = 3600 * time.Second
)

// errPhoneNotOnTelegram is the API's word for "this number has no Telegram".
// TODO(verify): проверить на песочнице — строка не описана в документации.
const errPhoneNotOnTelegram = "PHONE_NUMBER_NOT_TELEGRAM"

// TelegramGateway is the Telegram Gateway channel.
//
// # Why this channel is first
//
// It is the only one of the three that can say "this will fail" BEFORE anything
// is spent: checkSendAbility is free and answers whether the number can receive
// at all. So the waterfall's default order starts here — a miss costs nothing
// and a hit is the cheapest delivery we have.
type TelegramGateway struct {
	cfg  TelegramGatewayConfig
	base string
	http *httpClient
}

// NewTelegramGateway builds the channel. Build it only when cfg.Configured().
func NewTelegramGateway(cfg TelegramGatewayConfig) *TelegramGateway {
	base := trimmed(cfg.BaseURL)
	if base == "" {
		base = telegramGatewayBaseURL
	}
	return &TelegramGateway{
		cfg:  cfg,
		base: base,
		http: newHTTPClient(cfg.Timeout, cfg.Token),
	}
}

var _ Channel = (*TelegramGateway)(nil)

func (t *TelegramGateway) Name() string { return domain.OTPChannelTelegram }

// gatewayResponse is the API's envelope: {"ok":true,"result":{…}} or
// {"ok":false,"error":"ERROR_CODE"}.
type gatewayResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Result struct {
		RequestID string `json:"request_id"`
	} `json:"result"`
}

// Send runs the pre-check, then the send.
//
// The pre-check is not an optimization we could skip: it is what makes this
// channel safe to put first for every guest, including the majority who are not
// on Telegram. Its request_id is passed to the send so Telegram charges the pair
// once rather than twice.
//
// We pass OUR code (the API accepts `code`, 4–8 digits) instead of letting
// Telegram generate one, because verification happens on our side against the
// hash in otp_codes — a code we never saw would be unverifiable.
func (t *TelegramGateway) Send(ctx context.Context, phone, code string) (string, error) {
	requestID, err := t.checkSendAbility(ctx, phone)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"phone_number": phone,
		"code":         code,
		"ttl":          int(t.ttl().Seconds()),
	}
	if requestID != "" {
		// Reusing the pre-check's request_id is what makes the pair cost one fee.
		body["request_id"] = requestID
	}
	if u := trimmed(t.cfg.SenderUsername); u != "" {
		body["sender_username"] = u
	}

	resp, err := t.call(ctx, "sendVerificationMessage", body)
	if err != nil {
		return "", err
	}
	if !resp.OK {
		// Everything the API refuses is a fallback, never a failed login: an
		// empty balance, a revoked token, a sender that is not verified. The
		// distinction below only picks the log level.
		if unavailableGatewayError(resp.Error) {
			return "", fmt.Errorf("%w: %s", ErrChannelUnavailable, resp.Error)
		}
		return "", fmt.Errorf("telegram gateway: send rejected: %s", resp.Error)
	}
	// The send answers with the same request_id; keep the pre-check's value when
	// the send omits it, so the log always carries the handle Telegram knows.
	if id := trimmed(resp.Result.RequestID); id != "" {
		return id, nil
	}
	return requestID, nil
}

// checkSendAbility asks whether the number can receive a Gateway message. A
// clean "not on Telegram" is reported as ErrChannelUnavailable so the waterfall
// records it as expected traffic rather than as a provider fault.
func (t *TelegramGateway) checkSendAbility(ctx context.Context, phone string) (string, error) {
	resp, err := t.call(ctx, "checkSendAbility", map[string]any{"phone_number": phone})
	if err != nil {
		return "", err
	}
	if !resp.OK {
		if unavailableGatewayError(resp.Error) {
			return "", fmt.Errorf("%w: %s", ErrChannelUnavailable, resp.Error)
		}
		return "", fmt.Errorf("telegram gateway: pre-check rejected: %s", resp.Error)
	}
	return resp.Result.RequestID, nil
}

// unavailableGatewayError decides whether an `error` string is a "this channel
// cannot deliver right now" (INFO in the log, next channel) rather than a fault
// worth a WARN. It changes NOTHING about the flow: both cases fall through to
// WhatsApp, so a wrong guess here costs a log level and nothing else.
//
// Out of balance is deliberately in this bucket and was observed live on
// 2026-07-25: the account reported remaining_balance 0 while the request cost 0.
// A guest must never see a 500 because our prepaid balance ran out.
//
// TODO(verify): проверить на песочнице — Telegram Gateway не публикует список
// значений поля `error`; строки ниже собраны из наблюдений и по аналогии с
// ACCESS_TOKEN_INVALID из документации. Уточнить точные коды для «нет
// баланса» и «номер не в Telegram» и оставить только их.
func unavailableGatewayError(code string) bool {
	switch code {
	case errPhoneNotOnTelegram, "PHONE_NUMBER_NOT_FOUND":
		return true
	}
	// Any balance-shaped error, whatever Telegram calls it exactly.
	return strings.Contains(code, "BALANCE")
}

func (t *TelegramGateway) call(ctx context.Context, method string, payload map[string]any) (*gatewayResponse, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("telegram gateway: marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("telegram gateway: build %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+trimmed(t.cfg.Token))

	status, body, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram gateway: %s: %w", method, err)
	}
	var parsed gatewayResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		// A non-JSON answer is a proxy/outage page. Report the status only —
		// echoing the body would put an arbitrary third party's bytes into our
		// logs, and those bytes may include our own request.
		return nil, fmt.Errorf("telegram gateway: %s: unreadable response (http %d)", method, status)
	}
	// The API answers 200 with ok:false for business errors; a non-2xx with a
	// parsed envelope still carries the error string, which is the useful part.
	if status < 200 || status > 299 {
		if parsed.Error != "" {
			return &parsed, nil
		}
		return nil, fmt.Errorf("telegram gateway: %s: http %d", method, status)
	}
	return &parsed, nil
}

func (t *TelegramGateway) ttl() time.Duration {
	ttl := t.cfg.CodeTTL
	if ttl < telegramMinTTL {
		return telegramMinTTL
	}
	if ttl > telegramMaxTTL {
		return telegramMaxTTL
	}
	return ttl
}

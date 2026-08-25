// Package whatsapp is the ONE adapter for Meta's WhatsApp Cloud API.
//
// Two features send WhatsApp template messages through the same business
// number: the login code (internal/infrastructure/otpsender.WhatsApp) and the
// venue's "new booking" alert (Sender, below). They differ only in WHICH
// approved template they name and what they put in its parameters, so the
// request building, the response decoding and the secret scrubbing live here,
// once, and each caller keeps only its own semantics.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"backend-core/internal/infrastructure/scrubhttp"
)

const (
	// BaseURL is Meta's Graph host.
	BaseURL = "https://graph.facebook.com"
	// DefaultAPIVersion is the Graph version the live sends were verified on
	// (2026-07-25). Meta deprecates versions on a schedule, so it is config:
	// bumping it must not need a deploy of new code.
	DefaultAPIVersion = "v22.0"
	// ErrCodeNotOnWhatsApp is Meta's code for "this recipient cannot receive the
	// message" — in practice, no WhatsApp account on that number.
	ErrCodeNotOnWhatsApp = 131026
)

// Config is the credential pair every send needs, plus the knobs tests and
// staging override. The template name/language is NOT here: it belongs to
// each caller, because one number sends several different approved templates.
type Config struct {
	// AccessToken is a System User token with whatsapp_business_messaging.
	AccessToken string
	// PhoneNumberID is the sending number's id (WhatsApp → API Setup), not the
	// number itself.
	PhoneNumberID string
	// APIVersion pins the Graph version. Empty → DefaultAPIVersion.
	APIVersion string
	// Timeout caps one send. Empty → scrubhttp.DefaultTimeout.
	Timeout time.Duration
	// BaseURL overrides the Graph host. Tests / staging only.
	BaseURL string
}

// Configured reports whether a send is even possible: a token AND a phone
// number id. One of the two is a misconfiguration that would fail on every
// send, so it counts as absent.
func (c Config) Configured() bool {
	return scrubhttp.Trimmed(c.AccessToken) != "" && scrubhttp.Trimmed(c.PhoneNumberID) != ""
}

// Template is one send: an APPROVED template, its language exactly as approved,
// and the parameters that fill its {{n}} placeholders in order.
//
// A mismatch between the number of BodyParams and the approved template is
// rejected by Meta on EVERY send ("number of parameters does not match"), so
// neither the count nor the language is a value to guess at.
type Template struct {
	// To is the recipient in international form; the leading "+" is stripped
	// here because Meta wants digits only.
	To string
	// Name is the approved template's name.
	Name string
	// Lang is its approved language code ("ru", "en", …).
	Lang string
	// BodyParams fill {{1}}…{{n}} of the BODY component, in order.
	BodyParams []string
	// ButtonURLParam fills the URL button's own parameter. Meta's authentication
	// templates carry a "Copy code" button by default and that button needs its
	// OWN parameter; a template approved WITHOUT the button is rejected the
	// other way round. Empty = no button component.
	ButtonURLParam string
}

// Result is the outcome of one send as Meta reported it. It is returned even
// alongside an error, so a caller can classify the failure by CODE (retry vs
// give up) instead of by parsing an error string.
type Result struct {
	// MessageID is the wamid — the handle Meta's dashboard and the delivery
	// webhook key on. Present only on acceptance.
	MessageID string
	// Status is the HTTP status Meta answered with, 0 on a transport failure.
	Status int
	// ErrorCode / ErrorSubcode are Meta's numeric codes, 0 when absent.
	ErrorCode    int
	ErrorSubcode int
}

// Client posts templates to one business number.
type Client struct {
	cfg  Config
	base string
	http *scrubhttp.Client
}

// NewClient builds the client. Build it only when cfg.Configured().
func NewClient(cfg Config) *Client {
	base := scrubhttp.Trimmed(cfg.BaseURL)
	if base == "" {
		base = BaseURL
	}
	if scrubhttp.Trimmed(cfg.APIVersion) == "" {
		cfg.APIVersion = DefaultAPIVersion
	}
	return &Client{cfg: cfg, base: base, http: scrubhttp.NewClient(cfg.Timeout, cfg.AccessToken)}
}

// response decodes only the numeric parts of Meta's answer. Meta's prose
// "message" is deliberately NOT parsed: it is free text from a third party that
// would land in our logs, and everything we act on is expressible as a code.
type response struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error struct {
		Code    int `json:"code"`
		Subcode int `json:"error_subcode"`
	} `json:"error"`
}

// Send posts one template.
//
// A nil error means Meta ACCEPTED the message — not that it was delivered.
// Acceptance is all the Cloud API offers synchronously: the real outcome
// arrives later on the delivery webhook (or never), which is why every caller
// is built so that a silent non-delivery is survivable.
func (c *Client) Send(ctx context.Context, t Template) (Result, error) {
	components := []map[string]any{{
		"type":       "body",
		"parameters": textParams(t.BodyParams),
	}}
	if scrubhttp.Trimmed(t.ButtonURLParam) != "" {
		components = append(components, map[string]any{
			"type":       "button",
			"sub_type":   "url",
			"index":      0,
			"parameters": textParams([]string{t.ButtonURLParam}),
		})
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		// Meta wants the number WITHOUT the leading "+", digits only.
		"to":   strings.TrimPrefix(scrubhttp.Trimmed(t.To), "+"),
		"type": "template",
		"template": map[string]any{
			"name":       scrubhttp.Trimmed(t.Name),
			"language":   map[string]any{"code": scrubhttp.Trimmed(t.Lang)},
			"components": components,
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return Result{}, fmt.Errorf("whatsapp: marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/%s/%s/messages", c.base, scrubhttp.Trimmed(c.cfg.APIVersion), scrubhttp.Trimmed(c.cfg.PhoneNumberID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+scrubhttp.Trimmed(c.cfg.AccessToken))

	status, body, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("whatsapp: send: %w", err)
	}

	var parsed response
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		// An HTML error page from a proxy, typically. The status is still the
		// caller's best classifier, so it is carried out.
		return Result{Status: status}, fmt.Errorf("whatsapp: unreadable response (http %d)", status)
	}
	res := Result{Status: status, ErrorCode: parsed.Error.Code, ErrorSubcode: parsed.Error.Subcode}
	if status >= 200 && status <= 299 && len(parsed.Messages) > 0 && parsed.Messages[0].ID != "" {
		res.MessageID = parsed.Messages[0].ID
		return res, nil
	}
	// Reported by CODE, never by Meta's prose: the prose is attacker-influenced
	// free text that would end up in our logs, and it is not a stable thing to
	// alert on.
	return res, fmt.Errorf("whatsapp: rejected (http %d, code %d, subcode %d)", status, res.ErrorCode, res.ErrorSubcode)
}

func textParams(values []string) []map[string]any {
	out := make([]map[string]any, 0, len(values))
	for _, v := range values {
		out = append(out, map[string]any{"type": "text", "text": v})
	}
	return out
}

package domain

import (
	"fmt"
	"net/url"
	"strings"
)

// EventActionTarget names where an event's call-to-action button leads. It is
// DERIVED from EventAction.URL, never stored: two representations of the same
// fact drift apart, and this one is a pure function of the other.
type EventActionTarget string

const (
	// EventActionTargetEvent — the button opens the event's own page inside the
	// app (GET /events/{id}). This is what a platform «афиша» без внешнего
	// билетного партнёра uses.
	EventActionTargetEvent EventActionTarget = "event"
	// EventActionTargetExternal — the button opens EventAction.URL in a browser.
	EventActionTargetExternal EventActionTarget = "external"
)

// EventAction is the optional call-to-action button on an event card («Купить
// билет», «Зарегистрироваться»). It is deliberately NOT the ticketing flow:
// this is a label plus a destination, and the destination is either the event's
// own page or an external partner link.
//
// Label is required whenever the button exists at all — a button without a
// caption cannot be drawn, and the client must never have to invent one.
type EventAction struct {
	Label string
	// URL nil = the event's own page. Non-nil = an EXTERNAL link, already
	// validated by ValidateExternalActionURL: absolute, http/https, with a host
	// and without embedded credentials.
	URL *string
}

// Target reports where the button leads.
func (a EventAction) Target() EventActionTarget {
	if a.URL == nil {
		return EventActionTargetEvent
	}
	return EventActionTargetExternal
}

// maxEventActionLabel bounds the caption. A button caption is two or three
// words; anything longer is a paste accident, not a caption, and it would be
// truncated by every client anyway.
const maxEventActionLabel = 64

// maxEventActionURL bounds the link. 2048 is the length every mainstream
// browser and proxy is known to accept; beyond it the link is not portable, so
// storing it would only move the failure to the guest's phone.
const maxEventActionURL = 2048

// allowedActionURLSchemes is an ALLOWLIST, not a denylist of the bad ones. A
// denylist is a losing game here: `javascript:`, `data:`, `vbscript:`,
// `file:`, `intent:` and a browser's own custom schemes all end up executing or
// reading something on the guest's device, and the list grows with every client
// platform. Two schemes are what a ticket partner ever needs.
//
// `http` is allowed next to `https` on purpose and with open eyes: some KZ
// ticket partners still publish plain-http links, and refusing them would mean
// a marketer cannot enter a link that works — a rule people route around is
// worse than one that is merely imperfect. It is a scheme allowlist, so
// tightening it to https only later is a one-line policy change.
var allowedActionURLSchemes = map[string]bool{"http": true, "https": true}

// ValidateExternalActionURL checks a link an EDITOR typed before it is stored,
// and returns the trimmed value to store. It is strict: everything that is not
// obviously a safe, openable web link is refused with ErrValidation rather than
// stored and discovered later on a guest's phone.
//
// What it refuses, and why:
//   - a scheme outside the allowlist — `javascript:`/`data:`/`vbscript:` are
//     code execution in whatever webview opens them, and every other scheme is
//     a device-local action nobody buys a ticket through;
//   - a relative or scheme-less value ("book-eat.com/x") — the client would
//     have to guess a scheme, and guessing `http` is how a link silently
//     downgrades;
//   - an empty host ("https:///x") — nothing to open;
//   - embedded credentials ("https://user:pass@host") — a phishing shape, and
//     a secret in a column that is served to every guest;
//   - whitespace or ASCII control characters anywhere in the value — the
//     classic way to smuggle "java\nscript:" past a naive check;
//   - anything longer than maxEventActionURL.
func ValidateExternalActionURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("%w: action url must not be empty", ErrValidation)
	}
	if len(v) > maxEventActionURL {
		return "", fmt.Errorf("%w: action url must be at most %d characters", ErrValidation, maxEventActionURL)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return "", fmt.Errorf("%w: action url must not contain whitespace or control characters", ErrValidation)
		}
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%w: action url is not a valid url", ErrValidation)
	}
	// url.Parse lower-cases nothing for us on a malformed input, so compare a
	// folded copy: "HTTPS://..." is the same scheme and must not be refused.
	if !allowedActionURLSchemes[strings.ToLower(u.Scheme)] {
		return "", fmt.Errorf("%w: action url must start with http:// or https://", ErrValidation)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: action url must contain a host", ErrValidation)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: action url must not contain credentials", ErrValidation)
	}
	return v, nil
}

// ValidateEventAction validates a whole button. A nil action ("no button") is
// always valid — the button is optional content, not a required field.
func ValidateEventAction(a *EventAction) error {
	if a == nil {
		return nil
	}
	label := strings.TrimSpace(a.Label)
	if label == "" {
		return fmt.Errorf("%w: action label is required when the event has a button", ErrValidation)
	}
	if len([]rune(label)) > maxEventActionLabel {
		return fmt.Errorf("%w: action label must be at most %d characters", ErrValidation, maxEventActionLabel)
	}
	a.Label = label
	if a.URL == nil {
		return nil
	}
	v, err := ValidateExternalActionURL(*a.URL)
	if err != nil {
		return err
	}
	a.URL = &v
	return nil
}

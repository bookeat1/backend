package domain

import (
	"errors"
	"strings"
	"testing"
)

// The action button's URL is the one field of an event that a guest's device
// EXECUTES rather than displays, so its validation is tested as an allowlist:
// the cases below are the shapes an attacker (or a careless paste) actually
// produces, and each one must be refused with ErrValidation, never stored.
func TestValidateExternalActionURL_Rejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		why  string
	}{
		{"javascript", "javascript:alert(1)", "code execution in whatever webview opens it"},
		{"javascript uppercase", "JavaScript:alert(1)", "the scheme is case-insensitive to a browser"},
		{"javascript with a newline", "java\nscript:alert(1)", "control characters smuggle a scheme past a naive check"},
		{"javascript with a tab", "java\tscript:alert(1)", "same trick, another whitespace character"},
		{"data url", "data:text/html;base64,PHNjcmlwdD4=", "an inline document is not a partner link"},
		{"vbscript", "vbscript:msgbox(1)", "code execution on the platforms that still honour it"},
		{"file", "file:///etc/passwd", "reads the device, not the web"},
		{"custom app scheme", "intent://scan/#Intent;scheme=zxing;end", "a device-local action nobody buys a ticket through"},
		{"relative", "book-eat.com/tickets", "the client would have to guess a scheme"},
		{"scheme-less protocol-relative", "//book-eat.com/tickets", "same guess, and it inherits whatever the webview was on"},
		{"empty host", "https:///tickets", "nothing to open"},
		{"credentials", "https://user:secret@tickets.kz/e/1", "a phishing shape, and a secret in a column served to every guest"},
		{"inner space", "https://tickets.kz/e 1", "an unencoded space is not a portable link"},
		{"empty", "", "no link at all"},
		{"only whitespace", "   ", "no link at all"},
		{"too long", "https://tickets.kz/" + strings.Repeat("a", 2100), "not portable through browsers and proxies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateExternalActionURL(tt.in)
			if err == nil {
				t.Fatalf("accepted %q (%s), stored as %q", tt.in, tt.why, got)
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("error must be ErrValidation (422), got %v", err)
			}
		})
	}
}

// The allowlist is a scheme allowlist, not a denylist: these must pass, or a
// marketer cannot enter a link that works and will route around the field.
func TestValidateExternalActionURL_Accepts(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"https", "https://tickets.kz/e/42", "https://tickets.kz/e/42"},
		{"http", "http://tickets.kz/e/42", "http://tickets.kz/e/42"},
		{"uppercase scheme", "HTTPS://tickets.kz/e/42", "HTTPS://tickets.kz/e/42"},
		{"query and fragment", "https://tickets.kz/e?utm=tg#buy", "https://tickets.kz/e?utm=tg#buy"},
		{"punycode host", "https://xn--80ak6aa92e.com/e", "https://xn--80ak6aa92e.com/e"},
		{"surrounding whitespace is trimmed", "  https://tickets.kz/e  ", "https://tickets.kz/e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateExternalActionURL(tt.in)
			if err != nil {
				t.Fatalf("rejected a legitimate link %q: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("stored %q, want %q", got, tt.want)
			}
		})
	}
}

// A button is a caption plus a destination. No caption means a button nobody
// can draw, whichever destination it has.
func TestValidateEventAction(t *testing.T) {
	external := "https://tickets.kz/e/42"

	if err := ValidateEventAction(nil); err != nil {
		t.Fatalf("no button at all is a valid state: %v", err)
	}

	ownPage := &EventAction{Label: "  Подробнее  "}
	if err := ValidateEventAction(ownPage); err != nil {
		t.Fatalf("a button onto the event's own page must be valid: %v", err)
	}
	if ownPage.Label != "Подробнее" {
		t.Fatalf("label = %q, want it trimmed", ownPage.Label)
	}
	if ownPage.Target() != EventActionTargetEvent {
		t.Fatalf("target = %q, want %q", ownPage.Target(), EventActionTargetEvent)
	}

	ext := &EventAction{Label: "Купить билет", URL: &external}
	if err := ValidateEventAction(ext); err != nil {
		t.Fatalf("an external button must be valid: %v", err)
	}
	if ext.Target() != EventActionTargetExternal {
		t.Fatalf("target = %q, want %q", ext.Target(), EventActionTargetExternal)
	}

	for _, bad := range []*EventAction{
		{Label: "   "},
		{Label: "", URL: &external},
		{Label: strings.Repeat("я", 65)},
		{Label: "Купить", URL: strPtr("javascript:alert(1)")},
	} {
		if err := ValidateEventAction(bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("action %+v must be refused with ErrValidation, got %v", bad, err)
		}
	}
}

func strPtr(s string) *string { return &s }

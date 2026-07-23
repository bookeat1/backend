package tiptoppay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	g, err := New(Config{BaseURL: DefaultBaseURL, PublicID: testPublicID, APISecret: testAPISecret}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// payNotification is a realistic two-stage `pay` callback.
func payNotification() []byte {
	v := url.Values{}
	v.Set("TransactionId", "897749645")
	v.Set("Amount", "103.50")
	v.Set("Currency", "KZT")
	v.Set("PaymentAmount", "103.50")
	v.Set("PaymentCurrency", "KZT")
	v.Set("DateTime", "2026-07-21 09:15:00")
	v.Set("InvoiceId", "11111111-1111-4111-8111-111111111111")
	v.Set("Status", "Authorized")
	v.Set("OperationType", "Payment")
	v.Set("CardFirstSix", "424242")
	v.Set("CardLastFour", "4242")
	v.Set("CardExpDate", "12/25")
	v.Set("TestMode", "1")
	return []byte(v.Encode())
}

func TestVerifyWebhookAcceptsAValidSignature(t *testing.T) {
	g := testGateway(t)
	body := payNotification()

	for _, headerName := range []string{headerContentHMAC, headerXContentHMAC, "content-hmac"} {
		t.Run(headerName, func(t *testing.T) {
			ev, err := g.VerifyWebhook(body, map[string]string{
				headerName:             signBody(testAPISecret, body),
				NotificationTypeHeader: "pay",
			})
			if err != nil {
				t.Fatalf("VerifyWebhook: %v", err)
			}
			if !ev.SignatureValid {
				t.Error("SignatureValid must be true")
			}
			if ev.Type != domain.WebhookPaymentAuthorized {
				t.Errorf("type = %s, want payment.authorized", ev.Type)
			}
			if ev.Status != domain.PaymentAuthorized {
				t.Errorf("status = %s", ev.Status)
			}
			if ev.ProviderPaymentID != "897749645" {
				t.Errorf("provider payment id = %q", ev.ProviderPaymentID)
			}
			if ev.MerchantPaymentID != "11111111-1111-4111-8111-111111111111" {
				t.Errorf("merchant payment id = %q", ev.MerchantPaymentID)
			}
			if ev.Amount != domain.KZT(10350) {
				t.Errorf("amount = %v, want 103.50 KZT", ev.Amount)
			}
			if !ev.OccurredAt.Equal(time.Date(2026, 7, 21, 9, 15, 0, 0, time.UTC)) {
				t.Errorf("occurred at = %v", ev.OccurredAt)
			}
		})
	}
}

func TestVerifyWebhookRejectsForgedMissingAndTamperedSignatures(t *testing.T) {
	g := testGateway(t)
	body := payNotification()
	valid := signBody(testAPISecret, body)

	tests := []struct {
		name    string
		body    []byte
		headers map[string]string
	}{
		{
			name:    "forged signature",
			body:    body,
			headers: map[string]string{headerContentHMAC: signBody("attacker-guess", body)},
		},
		{
			name:    "missing signature header",
			body:    body,
			headers: map[string]string{NotificationTypeHeader: "pay"},
		},
		{
			name:    "nil headers",
			body:    body,
			headers: nil,
		},
		{
			name:    "empty signature header",
			body:    body,
			headers: map[string]string{headerContentHMAC: "   "},
		},
		{
			name:    "empty body",
			body:    nil,
			headers: map[string]string{headerContentHMAC: valid},
		},
		{
			name:    "body tampered after signing (amount raised)",
			body:    []byte(strings.Replace(string(body), "Amount=103.50", "Amount=999.99", 1)),
			headers: map[string]string{headerContentHMAC: valid},
		},
		{
			name:    "extra field appended after signing",
			body:    append(append([]byte{}, body...), []byte("&Evil=1")...),
			headers: map[string]string{headerContentHMAC: valid},
		},
		{
			name:    "signature of a different body",
			body:    body,
			headers: map[string]string{headerContentHMAC: signBody(testAPISecret, []byte("something else"))},
		},
		{
			name:    "not base64",
			body:    body,
			headers: map[string]string{headerContentHMAC: "!!!not-base64!!!"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := g.VerifyWebhook(tc.body, tc.headers)
			if err == nil {
				t.Fatalf("VerifyWebhook accepted %s: %+v", tc.name, ev)
			}
			if ev != nil {
				t.Error("no event may be returned when verification fails — the payload must not be interpreted")
			}
			if !errors.Is(err, ErrSignature) {
				t.Errorf("err = %v, want ErrSignature", err)
			}
			// Transport maps this to 401 (spec §7).
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Errorf("err = %v, want it to wrap domain.ErrUnauthorized", err)
			}
		})
	}
}

// A signed message with fields we do not know about is still authentic: the
// HMAC covers the whole body, so extra fields cannot be injected — but they
// must not break parsing either.
func TestVerifyWebhookAcceptsUnknownFieldsThatWereSigned(t *testing.T) {
	g := testGateway(t)
	v, _ := url.ParseQuery(string(payNotification()))
	v.Set("SomeFutureField", "whatever")
	v.Set("CustomFields", `{"a":"b"}`)
	body := []byte(v.Encode())

	ev, err := g.VerifyWebhook(body, map[string]string{
		headerContentHMAC:      signBody(testAPISecret, body),
		NotificationTypeHeader: "pay",
	})
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["SomeFutureField"] != "whatever" {
		t.Error("unknown fields must survive into the audit payload")
	}
}

// payment_events is read by humans in support tickets. Card identifiers have no
// business being there (spec §8).
func TestVerifyWebhookRedactsCardDataFromTheStoredPayload(t *testing.T) {
	g := testGateway(t)
	body := payNotification()

	ev, err := g.VerifyWebhook(body, map[string]string{
		headerContentHMAC:      signBody(testAPISecret, body),
		NotificationTypeHeader: "pay",
	})
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	for _, forbidden := range []string{"424242", "4242", "12/25"} {
		if strings.Contains(string(ev.Payload), forbidden) {
			t.Errorf("stored payload leaks card data %q: %s", forbidden, ev.Payload)
		}
	}
}

func TestVerifyWebhookMapsNotificationTypes(t *testing.T) {
	g := testGateway(t)

	tests := []struct {
		name       string
		kind       string
		fields     map[string]string
		wantType   domain.WebhookEventType
		wantStatus domain.PaymentStatus
		wantEvent  string
	}{
		{
			name:       "pay, two-stage hold",
			kind:       "pay",
			fields:     map[string]string{"TransactionId": "1", "Status": "Authorized"},
			wantType:   domain.WebhookPaymentAuthorized,
			wantStatus: domain.PaymentAuthorized,
			wantEvent:  "pay:1",
		},
		{
			name:       "pay, one-stage charge",
			kind:       "pay",
			fields:     map[string]string{"TransactionId": "2", "Status": "Completed"},
			wantType:   domain.WebhookPaymentCaptured,
			wantStatus: domain.PaymentCaptured,
			wantEvent:  "pay:2",
		},
		{
			name:       "confirm",
			kind:       "confirm",
			fields:     map[string]string{"TransactionId": "3", "Status": "Completed"},
			wantType:   domain.WebhookPaymentCaptured,
			wantStatus: domain.PaymentCaptured,
			wantEvent:  "confirm:3",
		},
		{
			name:       "fail",
			kind:       "fail",
			fields:     map[string]string{"TransactionId": "4", "Status": "Declined", "Reason": "InsufficientFunds", "ReasonCode": "5051"},
			wantType:   domain.WebhookPaymentFailed,
			wantStatus: domain.PaymentFailed,
			wantEvent:  "fail:4",
		},
		{
			name:       "cancel carries no Status field",
			kind:       "cancel",
			fields:     map[string]string{"TransactionId": "5"},
			wantType:   domain.WebhookPaymentVoided,
			wantStatus: domain.PaymentVoided,
			wantEvent:  "cancel:5",
		},
		{
			name:       "check is authentic but not acted upon",
			kind:       "check",
			fields:     map[string]string{"TransactionId": "6", "Status": "Completed"},
			wantType:   domain.WebhookUnknown,
			wantStatus: domain.PaymentCaptured,
			wantEvent:  "check:6",
		},
		{
			name:       "unrouted notification is not guessed",
			kind:       "",
			fields:     map[string]string{"TransactionId": "7", "Status": "Completed"},
			wantType:   domain.WebhookUnknown,
			wantStatus: domain.PaymentCaptured,
			wantEvent:  "unknown:7",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := url.Values{}
			for k, val := range tc.fields {
				v.Set(k, val)
			}
			body := []byte(v.Encode())
			headers := map[string]string{headerContentHMAC: signBody(testAPISecret, body)}
			if tc.kind != "" {
				headers[NotificationTypeHeader] = tc.kind
			}

			ev, err := g.VerifyWebhook(body, headers)
			if err != nil {
				t.Fatalf("VerifyWebhook: %v", err)
			}
			if ev.Type != tc.wantType {
				t.Errorf("type = %s, want %s", ev.Type, tc.wantType)
			}
			if ev.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", ev.Status, tc.wantStatus)
			}
			if ev.ProviderEventID != tc.wantEvent {
				t.Errorf("event id = %q, want %q", ev.ProviderEventID, tc.wantEvent)
			}
		})
	}
}

// A refund notification is about the refund's own transaction; the payment it
// belongs to is PaymentTransactionId. Getting this backwards would credit the
// wrong record.
func TestVerifyWebhookRefundUsesTheOriginalTransaction(t *testing.T) {
	g := testGateway(t)
	v := url.Values{}
	v.Set("TransactionId", "568")
	v.Set("PaymentTransactionId", "455")
	v.Set("Amount", "50.00")
	v.Set("OperationType", "Refund")
	v.Set("InvoiceId", "order-9")
	v.Set("DateTime", "2026-07-21 10:00:00")
	body := []byte(v.Encode())

	ev, err := g.VerifyWebhook(body, map[string]string{
		headerContentHMAC:      signBody(testAPISecret, body),
		NotificationTypeHeader: "refund",
	})
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Type != domain.WebhookRefundSucceeded {
		t.Errorf("type = %s", ev.Type)
	}
	if ev.ProviderRefundID != "568" {
		t.Errorf("refund id = %q, want 568", ev.ProviderRefundID)
	}
	if ev.ProviderPaymentID != "455" {
		t.Errorf("payment id = %q, want 455", ev.ProviderPaymentID)
	}
	if ev.Amount != domain.KZT(5000) {
		t.Errorf("amount = %v", ev.Amount)
	}
	if ev.ProviderEventID != "refund:568" {
		t.Errorf("event id = %q", ev.ProviderEventID)
	}
}

// The webhook is retried by TipTopPay; the derived event id must be identical
// across those retries so payment_events deduplicates them (spec §7).
func TestWebhookEventIDIsStableAcrossRetries(t *testing.T) {
	g := testGateway(t)
	body := payNotification()
	headers := map[string]string{
		headerContentHMAC:      signBody(testAPISecret, body),
		NotificationTypeHeader: "pay",
	}

	first, err := g.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	second, err := g.VerifyWebhook(body, headers)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if first.ProviderEventID != second.ProviderEventID {
		t.Errorf("event id changed between deliveries: %q vs %q", first.ProviderEventID, second.ProviderEventID)
	}
}

// ---------------------------------------------------------------------------
// anti-corruption layer, table-driven
// ---------------------------------------------------------------------------

func TestMapStatus(t *testing.T) {
	tests := []struct {
		provider  string
		want      domain.PaymentStatus
		wantKnown bool
	}{
		{"AwaitingAuthentication", domain.PaymentCreated, true},
		{"Authorized", domain.PaymentAuthorized, true},
		{"Completed", domain.PaymentCaptured, true},
		{"Cancelled", domain.PaymentVoided, true},
		{"Declined", domain.PaymentFailed, true},
		{" Authorized ", domain.PaymentAuthorized, true},
		{"", domain.PaymentCreated, false},
		{"SomethingNew", domain.PaymentCreated, false},
		// Casing is NOT normalised on purpose: an acquirer changing the case of
		// its status words is a change we want to notice, not absorb.
		{"authorized", domain.PaymentCreated, false},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			got, known := mapStatus(tc.provider)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("mapStatus(%q) = (%s, %v), want (%s, %v)", tc.provider, got, known, tc.want, tc.wantKnown)
			}
		})
	}
	// The safety property: an unknown word must never be read as money taken.
	for _, s := range []string{"", "Paid", "Success", "OK"} {
		if got, _ := mapStatus(s); got == domain.PaymentCaptured {
			t.Errorf("mapStatus(%q) optimistically reported captured", s)
		}
	}
}

func TestMapNotification(t *testing.T) {
	tests := []struct {
		kind   string
		status domain.PaymentStatus
		want   domain.WebhookEventType
	}{
		{"pay", domain.PaymentAuthorized, domain.WebhookPaymentAuthorized},
		{"pay", domain.PaymentCaptured, domain.WebhookPaymentCaptured},
		{"PAY", domain.PaymentCaptured, domain.WebhookPaymentCaptured},
		{"confirm", domain.PaymentCaptured, domain.WebhookPaymentCaptured},
		{"fail", domain.PaymentFailed, domain.WebhookPaymentFailed},
		{"cancel", domain.PaymentVoided, domain.WebhookPaymentVoided},
		{"refund", domain.PaymentCaptured, domain.WebhookRefundSucceeded},
		{"check", domain.PaymentCreated, domain.WebhookUnknown},
		{"recurrent", domain.PaymentCreated, domain.WebhookUnknown},
		{"", domain.PaymentCreated, domain.WebhookUnknown},
	}
	for _, tc := range tests {
		if got := mapNotification(tc.kind, tc.status); got != tc.want {
			t.Errorf("mapNotification(%q, %s) = %s, want %s", tc.kind, tc.status, got, tc.want)
		}
	}
}

func TestParseNotificationTime(t *testing.T) {
	got := parseNotificationTime("2026-07-21 09:15:00")
	if !got.Equal(time.Date(2026, 7, 21, 9, 15, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	if !parseNotificationTime("nonsense").IsZero() {
		t.Error("an unparseable time must be zero, not a guess")
	}
}

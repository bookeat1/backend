package freedompay

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
)

func webhookGateway(t *testing.T) *Gateway {
	t.Helper()
	g, err := New(Config{
		BaseURL:          DefaultBaseURL,
		MerchantID:       testMerchantID,
		SecretKey:        testSecretKey,
		ResultScriptName: DefaultResultScriptName,
	}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// callback builds a realistic, correctly signed result_url callback.
func callback(fields map[string]string) []byte {
	v := url.Values{}
	v.Set("pg_order_id", "11111111-1111-4111-8111-111111111111")
	v.Set("pg_payment_id", "1427057029")
	v.Set("pg_amount", "103.50")
	v.Set("pg_currency", "KZT")
	v.Set("pg_net_amount", "100.00")
	v.Set("pg_result", "1")
	v.Set("pg_captured", "0")
	v.Set("pg_can_reject", "1")
	v.Set("pg_payment_date", "2026-07-21 09:15:00")
	v.Set("pg_description", "Депозит BookEat")
	v.Set("pg_card_pan", "4400-44XX-XXXX-4444")
	v.Set("pg_card_exp", "12/24")
	for k, val := range fields {
		if val == "" {
			v.Del(k)
			continue
		}
		v.Set(k, val)
	}
	v.Set(saltParam, "saltsalt")
	v.Set(sigParam, sign(DefaultResultScriptName, v, testSecretKey))
	return []byte(v.Encode())
}

func TestVerifyWebhookAcceptsAValidCallback(t *testing.T) {
	g := webhookGateway(t)

	ev, err := g.VerifyWebhook(callback(nil), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if !ev.SignatureValid {
		t.Error("SignatureValid must be true")
	}
	if ev.Provider != domain.ProviderFreedomPay {
		t.Errorf("provider = %s", ev.Provider)
	}
	if ev.Type != domain.WebhookPaymentAuthorized {
		t.Errorf("type = %s, want payment.authorized (pg_captured=0)", ev.Type)
	}
	if ev.Status != domain.PaymentAuthorized {
		t.Errorf("status = %s", ev.Status)
	}
	if ev.ProviderPaymentID != "1427057029" {
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
	if !CanReject(ev) {
		t.Error("pg_can_reject=1 must be visible to the saga compensation path")
	}
}

func TestVerifyWebhookAcceptsTheScriptNameFromTheRoute(t *testing.T) {
	g := webhookGateway(t)

	// Same body, signed for a different route.
	v, _ := url.ParseQuery(string(callback(nil)))
	v.Del(sigParam)
	v.Set(sigParam, sign("fp-callback", v, testSecretKey))
	body := []byte(v.Encode())

	if _, err := g.VerifyWebhook(body, nil); err == nil {
		t.Fatal("a callback signed for another script must not verify against the configured one")
	}
	ev, err := g.VerifyWebhook(body, map[string]string{
		ScriptNameHeader: "https://bookeat.kz/webhooks/payments/fp-callback",
	})
	if err != nil {
		t.Fatalf("VerifyWebhook with the route's script name: %v", err)
	}
	if !ev.SignatureValid {
		t.Error("SignatureValid must be true")
	}
}

func TestVerifyWebhookRejectsForgedMissingAndTamperedSignatures(t *testing.T) {
	g := webhookGateway(t)
	valid := callback(nil)

	tamper := func(fn func(url.Values)) []byte {
		v, _ := url.ParseQuery(string(valid))
		fn(v)
		return []byte(v.Encode())
	}

	tests := []struct {
		name string
		body []byte
	}{
		{"empty body", nil},
		{"no pg_sig", tamper(func(v url.Values) { v.Del(sigParam) })},
		{"empty pg_sig", tamper(func(v url.Values) { v.Set(sigParam, "") })},
		{"forged pg_sig", tamper(func(v url.Values) { v.Set(sigParam, "ffffffffffffffffffffffffffffffff") })},
		{"amount raised after signing", tamper(func(v url.Values) { v.Set("pg_amount", "1.00") })},
		{"result flipped after signing", tamper(func(v url.Values) { v.Set("pg_result", "0") })},
		{"extra field appended after signing", tamper(func(v url.Values) { v.Set("pg_evil", "1") })},
		{"field removed after signing", tamper(func(v url.Values) { v.Del("pg_net_amount") })},
		{"salt replaced after signing", tamper(func(v url.Values) { v.Set(saltParam, "other") })},
		{"signature of another merchant's key", func() []byte {
			v, _ := url.ParseQuery(string(valid))
			v.Del(sigParam)
			v.Set(sigParam, sign(DefaultResultScriptName, v, "attacker-key"))
			return []byte(v.Encode())
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := g.VerifyWebhook(tc.body, nil)
			if err == nil {
				t.Fatalf("accepted %s: %+v", tc.name, ev)
			}
			if ev != nil {
				t.Error("no event may be returned when verification fails")
			}
			if !errors.Is(err, ErrSignature) {
				t.Errorf("err = %v, want ErrSignature", err)
			}
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Errorf("err = %v, want it to wrap domain.ErrUnauthorized", err)
			}
		})
	}
}

// A callback carrying fields we do not know about is still authentic — the
// signature covers all of them — and must parse.
func TestVerifyWebhookAcceptsUnknownSignedFields(t *testing.T) {
	g := webhookGateway(t)
	body := callback(map[string]string{"pg_some_future_field": "value"})

	ev, err := g.VerifyWebhook(body, nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["pg_some_future_field"] != "value" {
		t.Error("unknown fields must survive into the audit payload")
	}
}

func TestVerifyWebhookRedactsCardDataAndSignature(t *testing.T) {
	g := webhookGateway(t)

	ev, err := g.VerifyWebhook(callback(nil), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	for _, forbidden := range []string{"4400-44XX-XXXX-4444", "12/24"} {
		if strings.Contains(string(ev.Payload), forbidden) {
			t.Errorf("stored payload leaks card data %q: %s", forbidden, ev.Payload)
		}
	}
	if !strings.Contains(string(ev.Payload), `"pg_sig":"[redacted]"`) {
		t.Errorf("pg_sig must be redacted: %s", ev.Payload)
	}
}

func TestVerifyWebhookStatusMapping(t *testing.T) {
	g := webhookGateway(t)

	tests := []struct {
		name       string
		fields     map[string]string
		wantStatus domain.PaymentStatus
		wantType   domain.WebhookEventType
	}{
		{
			name:       "hold placed",
			fields:     map[string]string{"pg_result": "1", "pg_captured": "0"},
			wantStatus: domain.PaymentAuthorized,
			wantType:   domain.WebhookPaymentAuthorized,
		},
		{
			name:       "charged",
			fields:     map[string]string{"pg_result": "1", "pg_captured": "1"},
			wantStatus: domain.PaymentCaptured,
			wantType:   domain.WebhookPaymentCaptured,
		},
		{
			name:       "declined",
			fields:     map[string]string{"pg_result": "0", "pg_captured": "0", "pg_failure_code": "100", "pg_failure_description": "Insufficient funds"},
			wantStatus: domain.PaymentFailed,
			wantType:   domain.WebhookPaymentFailed,
		},
		{
			name:       "no pg_result falls back to pg_payment_status",
			fields:     map[string]string{"pg_result": "", "pg_payment_status": "revoked"},
			wantStatus: domain.PaymentVoided,
			wantType:   domain.WebhookPaymentVoided,
		},
		{
			name:       "neither pg_result nor a known status is never read as paid",
			fields:     map[string]string{"pg_result": "", "pg_payment_status": "brand_new_word"},
			wantStatus: domain.PaymentCreated,
			wantType:   domain.WebhookUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := g.VerifyWebhook(callback(tc.fields), nil)
			if err != nil {
				t.Fatalf("VerifyWebhook: %v", err)
			}
			if ev.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", ev.Status, tc.wantStatus)
			}
			if ev.Type != tc.wantType {
				t.Errorf("type = %s, want %s", ev.Type, tc.wantType)
			}
			if tc.wantStatus == domain.PaymentFailed && ev.FailureCode == "" {
				t.Error("a failure must carry its code")
			}
		})
	}
}

// FreedomPay retries a callback every 30 minutes for 2 hours; pg_salt and
// pg_sig change per delivery, so the derived event id must not.
func TestWebhookEventIDIsStableAcrossRetries(t *testing.T) {
	g := webhookGateway(t)

	first, err := g.VerifyWebhook(callback(nil), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}

	// Re-sign the same facts with a fresh salt, as a retry would.
	v, _ := url.ParseQuery(string(callback(nil)))
	v.Del(sigParam)
	v.Set(saltParam, newSalt())
	v.Set(sigParam, sign(DefaultResultScriptName, v, testSecretKey))

	second, err := g.VerifyWebhook([]byte(v.Encode()), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook retry: %v", err)
	}
	if first.ProviderEventID != second.ProviderEventID {
		t.Errorf("event id changed between deliveries: %q vs %q", first.ProviderEventID, second.ProviderEventID)
	}
	if first.ProviderEventID != "result:1427057029:1:0" {
		t.Errorf("event id = %q", first.ProviderEventID)
	}
}

func TestAckIsSignedXML(t *testing.T) {
	g := webhookGateway(t)

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"ok", g.AckOK("Order paid"), "ok"},
		{"rejected", g.AckRejected("Table is gone"), "rejected"},
		{"error", g.AckError("bad payload"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := decodeXML(tc.body)
			if err != nil {
				t.Fatalf("decodeXML: %v", err)
			}
			if values.Get("pg_status") != tc.want {
				t.Errorf("pg_status = %q, want %q", values.Get("pg_status"), tc.want)
			}
			if !verify(DefaultResultScriptName, values, testSecretKey) {
				t.Error("our answer must be correctly signed — FreedomPay verifies it")
			}
		})
	}
}

func TestCanReject(t *testing.T) {
	g := webhookGateway(t)

	yes, err := g.VerifyWebhook(callback(map[string]string{"pg_can_reject": "1"}), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if !CanReject(yes) {
		t.Error("pg_can_reject=1 must be reported")
	}

	no, err := g.VerifyWebhook(callback(map[string]string{"pg_can_reject": "0"}), nil)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if CanReject(no) {
		t.Error("pg_can_reject=0 must not be reported as rejectable")
	}
	if CanReject(nil) {
		t.Error("a nil event is not rejectable")
	}
}

// ---------------------------------------------------------------------------
// anti-corruption layer, table-driven
// ---------------------------------------------------------------------------

func TestMapPaymentStatus(t *testing.T) {
	tests := []struct {
		status    string
		captured  bool
		want      domain.PaymentStatus
		wantKnown bool
	}{
		{"new", false, domain.PaymentCreated, true},
		{"pending", false, domain.PaymentCreated, true},
		{"process", false, domain.PaymentCreated, true},
		{"success", false, domain.PaymentAuthorized, true},
		{"success", true, domain.PaymentCaptured, true},
		{"SUCCESS", true, domain.PaymentCaptured, true},
		{" ok ", true, domain.PaymentCaptured, true},
		{"partial", true, domain.PaymentPartiallyRefunded, true},
		{"refunded", true, domain.PaymentRefunded, true},
		{"revoked", false, domain.PaymentVoided, true},
		{"canceled", false, domain.PaymentVoided, true},
		{"failed", false, domain.PaymentFailed, true},
		{"error", false, domain.PaymentFailed, true},
		{"rejected", false, domain.PaymentFailed, true},
		{"expired", false, domain.PaymentExpired, true},
		{"", false, domain.PaymentCreated, false},
		{"whatever", true, domain.PaymentCreated, false},
	}
	for _, tc := range tests {
		t.Run(tc.status+"/"+boolDigit(tc.captured), func(t *testing.T) {
			got, known := mapPaymentStatus(tc.status, tc.captured)
			if got != tc.want || known != tc.wantKnown {
				t.Errorf("mapPaymentStatus(%q, %v) = (%s, %v), want (%s, %v)",
					tc.status, tc.captured, got, known, tc.want, tc.wantKnown)
			}
		})
	}
	// The safety property.
	for _, s := range []string{"", "whatever", "paid", "done"} {
		if got, _ := mapPaymentStatus(s, true); got == domain.PaymentCaptured {
			t.Errorf("mapPaymentStatus(%q) optimistically reported captured", s)
		}
	}
}

func TestMapOperationStatus(t *testing.T) {
	tests := []struct {
		in        string
		want      domain.RefundStatus
		wantKnown bool
	}{
		{"success", domain.RefundSucceeded, true},
		{"OK", domain.RefundSucceeded, true},
		{"1", domain.RefundSucceeded, true},
		{"pending", domain.RefundCreated, true},
		{"failed", domain.RefundFailed, true},
		{"", domain.RefundFailed, true},
		{"unheard-of", domain.RefundFailed, false},
	}
	for _, tc := range tests {
		got, known := mapOperationStatus(tc.in)
		if got != tc.want || known != tc.wantKnown {
			t.Errorf("mapOperationStatus(%q) = (%s, %v), want (%s, %v)", tc.in, got, known, tc.want, tc.wantKnown)
		}
	}
}

func TestDecodeXML(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<response>
  <pg_payment_id>1427057029</pg_payment_id>
  <pg_status>ok</pg_status>
  <pg_amount>1030</pg_amount>
  <pg_user_email></pg_user_email>
  <pg_salt>XeFNeRYiwcqlWPg9TZUj6pc9gj6KYBSb</pg_salt>
  <pg_sig>d333fb3333b3f6e861c61682ba173c46</pg_sig>
</response>`)

	values, err := decodeXML(body)
	if err != nil {
		t.Fatalf("decodeXML: %v", err)
	}
	if values.Get("pg_payment_id") != "1427057029" {
		t.Errorf("pg_payment_id = %q", values.Get("pg_payment_id"))
	}
	if values.Get("pg_status") != "ok" {
		t.Errorf("pg_status = %q", values.Get("pg_status"))
	}
	if _, ok := values["pg_user_email"]; !ok {
		t.Error("an empty element must still be present — it participates in the signature")
	}
	if _, err := decodeXML([]byte(`<response><pg_status>ok`)); err == nil {
		t.Error("truncated XML must be an error")
	}
}

func TestParsePaymentDate(t *testing.T) {
	got := parsePaymentDate("2024-11-28 15:44:53")
	if got == nil || !got.Equal(time.Date(2024, 11, 28, 15, 44, 53, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
	got = parsePaymentDate("2024-11-28T10:44:55+00:00")
	if got == nil || !got.Equal(time.Date(2024, 11, 28, 10, 44, 55, 0, time.UTC)) {
		t.Errorf("RFC3339: got %v", got)
	}
	if parsePaymentDate("") != nil || parsePaymentDate("nonsense") != nil {
		t.Error("an unparseable date must be nil, not a guess")
	}
}

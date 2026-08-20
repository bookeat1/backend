package tiptoppay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

const (
	venueSubMerchant    = "test_api_00000000000000000000003"
	platformSubMerchant = "test_api_00000000000000000000004"
)

// splitGateway is fakeAcquirer.gateway with the split switches on. The two
// flags are separate on purpose (see Config): one says the terminal supports
// splits at all, the other permits the undocumented order-flow route.
func (f *fakeAcquirer) splitGateway(t *testing.T, viaOrders bool) *Gateway {
	t.Helper()
	client := payment.NewClient(f.srv.Client(), payment.Config{MaxAttempts: 3, Timeout: 100 * time.Millisecond}, nil,
		payment.WithSleep(func(ctx context.Context, d time.Duration) error { return ctx.Err() }),
	)
	g, err := New(Config{
		BaseURL:        f.srv.URL,
		PublicID:       testPublicID,
		APISecret:      testAPISecret,
		SplitsEnabled:  true,
		SplitViaOrders: viaOrders,
	}, client, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.now = func() time.Time { return time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC) }
	return g
}

// splitPlan is the division of the standard test payment: 10 000 ₸ to the venue
// and a 350,00 ₸ fee to the platform, adding up to the 10 350,00 ₸ that
// authorizeRequest() charges.
func splitPlan() domain.PaymentSplitPlan {
	return domain.PaymentSplitPlan{
		{Payee: domain.SplitPayeeVenue, AccountRef: venueSubMerchant, Amount: domain.KZT(1_000_000)},
		{Payee: domain.SplitPayeePlatform, AccountRef: platformSubMerchant, Amount: domain.KZT(35_000)},
	}
}

// splitsOf reads the Splits array out of a recorded request as
// (PublicId → the amount EXACTLY as it was written on the wire).
//
// It decodes with UseNumber on purpose: the whole point of the assertions below
// is that the acquirer receives "10000.00" — a dot and two decimals — and a
// plain json.Unmarshal would turn that into a float64 and hand back "10000",
// hiding the very thing being checked (and the very thing a float would break).
func splitsOf(t *testing.T, req recorded) map[string]string {
	t.Helper()
	var body struct {
		Splits []struct {
			PublicID string      `json:"PublicId"`
			Amount   json.Number `json:"Amount"`
		} `json:"Splits"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode %s body: %v", req.Path, err)
	}
	if body.Splits == nil {
		return nil
	}
	out := make(map[string]string, len(body.Splits))
	for _, e := range body.Splits {
		out[e.PublicID] = e.Amount.String()
	}
	return out
}

// amountOf is splitsOf for the request's own top-level Amount.
func amountOf(t *testing.T, req recorded) string {
	t.Helper()
	var body struct {
		Amount json.Number `json:"Amount"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode %s body: %v", req.Path, err)
	}
	return body.Amount.String()
}

// rejectedMsg is fakeAcquirer's rejection helper for a message that contains
// characters JSON has to escape (the acquirer's split errors quote a field
// name). Building the envelope by string concatenation, as `rejected` does,
// would emit invalid JSON for those.
func rejectedMsg(w http.ResponseWriter, message string) {
	body, err := json.Marshal(map[string]any{"Model": nil, "Success": false, "Message": message})
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// Authorize
// ---------------------------------------------------------------------------

func TestAuthorizeSendsTheSplitArray(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/orders/create" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, `{"Id":"gASGZVgUN21hcpPF","Currency":"KZT","Url":"https://orders.tiptoppay.kz/d/x","Status":"Created"}`)
	})
	g := f.splitGateway(t, true)

	req := authorizeRequest()
	req.Splits = splitPlan()
	if _, err := g.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	got := splitsOf(t, f.requests[0])
	want := map[string]string{
		venueSubMerchant:    "10000.00",
		platformSubMerchant: "350.00",
	}
	for ref, amount := range want {
		if got[ref] != amount {
			t.Fatalf("Splits = %v, want %v", got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("Splits = %v, want exactly %d shares", got, len(want))
	}
	// The dot separator and two decimals are the documented format, and the
	// amounts must add up to the request's own Amount.
	if amount := amountOf(t, f.requests[0]); amount != "10350.00" {
		t.Fatalf("Amount = %s, want 10350.00", amount)
	}
}

func TestAuthorizeWithoutSplitsSendsNoSplitField(t *testing.T) {
	// The regression that matters for every existing payment: turning the
	// feature on must not start decorating ordinary payments with an empty
	// array (TipTopPay would read that as "split into nothing").
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		ok(w, `{"Id":"order-1","Currency":"KZT","Url":"https://orders.tiptoppay.kz/d/x","Status":"Created"}`)
	})
	g := f.splitGateway(t, true)

	if _, err := g.Authorize(context.Background(), authorizeRequest()); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if _, present := f.requests[0].Body["Splits"]; present {
		t.Fatalf("body carries a Splits field for a non-split payment: %v", f.requests[0].Body)
	}
}

func TestAuthorizeRefusesSplitsWhenTheyAreNotConfigured(t *testing.T) {
	tests := []struct {
		name          string
		splitsEnabled bool
		viaOrders     bool
		wantCode      domain.ErrorCode
	}{
		{
			name:          "terminal is not set up for splits",
			splitsEnabled: false,
			viaOrders:     true,
			wantCode:      domain.CodeSplitNotSupported,
		},
		{
			// The documented methods for Splits are the cryptogram/token ones;
			// /orders/create is not among them. Until that is confirmed on the
			// sandbox we refuse rather than send a field that may be ignored —
			// an ignored Splits array pays the venue's money to the platform.
			name:          "the order flow is not confirmed to support splits",
			splitsEnabled: true,
			viaOrders:     false,
			wantCode:      domain.CodeSplitFlowUnsupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
				t.Errorf("the acquirer must not be called at all")
				ok(w, `{"Id":"x"}`)
			})
			g := f.splitGateway(t, tc.viaOrders)
			g.cfg.SplitsEnabled = tc.splitsEnabled

			req := authorizeRequest()
			req.Splits = splitPlan()
			_, err := g.Authorize(context.Background(), req)
			if err == nil {
				t.Fatalf("Authorize() = nil, want a refusal")
			}
			if code, _ := domain.CodeOf(err); code != tc.wantCode {
				t.Fatalf("code = %q, want %q — err: %v", code, tc.wantCode, err)
			}
			if len(f.requests) != 0 {
				t.Fatalf("%d request(s) reached the acquirer", len(f.requests))
			}
		})
	}
}

func TestAuthorizeRefusesSplitsThatDoNotAddUp(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		t.Errorf("the acquirer must not be called at all")
		ok(w, `{"Id":"x"}`)
	})
	g := f.splitGateway(t, true)

	req := authorizeRequest()
	plan := splitPlan()
	plan[1].Amount = domain.KZT(34_999) // one tiyn short of the charge
	req.Splits = plan

	_, err := g.Authorize(context.Background(), req)
	if err == nil {
		t.Fatalf("Authorize() = nil, want a refusal")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeSplitSumMismatch {
		t.Fatalf("code = %q, want %q — err: %v", code, domain.CodeSplitSumMismatch, err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("%d request(s) reached the acquirer", len(f.requests))
	}
}

func TestAuthorizeRefusesASplitWithoutAPublicID(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		t.Errorf("the acquirer must not be called at all")
		ok(w, `{"Id":"x"}`)
	})
	g := f.splitGateway(t, true)

	req := authorizeRequest()
	plan := splitPlan()
	plan[0].AccountRef = ""
	req.Splits = plan

	_, err := g.Authorize(context.Background(), req)
	if err == nil {
		t.Fatalf("Authorize() = nil, want a refusal")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeSplitAccountMissing {
		t.Fatalf("code = %q, want %q — err: %v", code, domain.CodeSplitAccountMissing, err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("%d request(s) reached the acquirer", len(f.requests))
	}
}

// ---------------------------------------------------------------------------
// Capture / Refund — the shares have to come back too
// ---------------------------------------------------------------------------

// splitTransaction is a /payments/get answer for a split payment of 10 350,00 ₸.
const splitTransaction = `{"TransactionId":10649404,"Amount":10350.00,"Currency":"KZT","Status":"Authorized",` +
	`"Splits":[{"PublicId":"test_api_00000000000000000000003","Amount":10000.00},` +
	`{"PublicId":"test_api_00000000000000000000004","Amount":350.00}]}`

func TestCaptureRepeatsTheOriginalSplits(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		switch path {
		case "/payments/get":
			ok(w, splitTransaction)
		case "/payments/confirm":
			ok(w, "")
		default:
			t.Errorf("unexpected path %s", path)
		}
	})
	g := f.splitGateway(t, true)

	if _, err := g.Capture(context.Background(), "10649404", domain.KZT(1_035_000)); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	confirm := f.requests[len(f.requests)-1]
	if confirm.Path != "/payments/confirm" {
		t.Fatalf("last request was %s", confirm.Path)
	}
	got := splitsOf(t, confirm)
	if got[venueSubMerchant] != "10000.00" || got[platformSubMerchant] != "350.00" {
		t.Fatalf("confirm Splits = %v, want the original shares", got)
	}
}

func TestCapturePartialScalesTheSplitsWithoutLosingATiyn(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		switch path {
		case "/payments/get":
			ok(w, splitTransaction)
		case "/payments/confirm":
			ok(w, "")
		default:
			t.Errorf("unexpected path %s", path)
		}
	})
	g := f.splitGateway(t, true)

	// A third of the hold, which divides into neither share evenly:
	// 3450,01 ₸ → venue 10000/10350 × 345001 = 333334,29… , platform 11666,70…
	// Floors are 333334 + 11666 = 345000, one tiyn short; it goes to the larger
	// discarded fraction — the platform's.
	const partial = 345_001
	if _, err := g.Capture(context.Background(), "10649404", domain.KZT(partial)); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	confirm := f.requests[len(f.requests)-1]
	got := splitsOf(t, confirm)
	if len(got) != 2 {
		t.Fatalf("confirm Splits = %v, want both shares", got)
	}
	sum, err := sumSplitAmounts(got)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != partial {
		t.Fatalf("Splits sum to %d, the confirmed amount is %d — a tiyn was %s",
			sum, partial, map[bool]string{true: "invented", false: "lost"}[sum > partial])
	}
	if got[venueSubMerchant] != "3333.34" || got[platformSubMerchant] != "116.67" {
		t.Fatalf("Splits = %v, want venue 3333.34 / platform 116.67", got)
	}
}

func TestRefundCarriesTheSplits(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		switch path {
		case "/payments/get":
			ok(w, splitTransaction)
		case "/payments/refund":
			ok(w, `{"TransactionId":568}`)
		default:
			t.Errorf("unexpected path %s", path)
		}
	})
	g := f.splitGateway(t, true)

	if _, err := g.Refund(context.Background(), "10649404", domain.KZT(1_035_000)); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	refund := f.requests[len(f.requests)-1]
	if refund.Path != "/payments/refund" {
		t.Fatalf("last request was %s", refund.Path)
	}
	got := splitsOf(t, refund)
	if got[venueSubMerchant] != "10000.00" || got[platformSubMerchant] != "350.00" {
		t.Fatalf("refund Splits = %v, want the full original shares", got)
	}
}

func TestCaptureOfANonSplitPaymentIsUnchanged(t *testing.T) {
	// A payment created before splits existed must confirm exactly as it always
	// did: one read, then a confirm with no Splits field at all.
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		switch path {
		case "/payments/get":
			ok(w, `{"TransactionId":10649404,"Amount":10350.00,"Currency":"KZT","Status":"Authorized"}`)
		case "/payments/confirm":
			ok(w, "")
		default:
			t.Errorf("unexpected path %s", path)
		}
	})
	g := f.splitGateway(t, true)

	if _, err := g.Capture(context.Background(), "10649404", domain.KZT(1_035_000)); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	confirm := f.requests[len(f.requests)-1]
	if _, present := confirm.Body["Splits"]; present {
		t.Fatalf("confirm carries Splits for a payment that has none: %v", confirm.Body)
	}
}

func TestCaptureDoesNotReadSplitsWhenTheTerminalHasNone(t *testing.T) {
	// With splits switched off the extra /payments/get must not happen at all —
	// one request per money movement, exactly as before this feature.
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/payments/confirm" {
			t.Errorf("unexpected path %s", path)
		}
		ok(w, "")
	})
	g := f.gateway(t, nil)

	if _, err := g.Capture(context.Background(), "10649404", domain.KZT(1_035_000)); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(f.requests) != 1 {
		t.Fatalf("%d requests, want exactly 1 (/payments/confirm)", len(f.requests))
	}
}

func TestCaptureRefusesMoreThanTheSplitsAreWorth(t *testing.T) {
	f := newFakeAcquirer(t, func(path string, _ int, _ map[string]any, w http.ResponseWriter) {
		if path != "/payments/get" {
			t.Errorf("the confirm must never be sent, got %s", path)
		}
		ok(w, splitTransaction)
	})
	g := f.splitGateway(t, true)

	_, err := g.Capture(context.Background(), "10649404", domain.KZT(1_035_001))
	if err == nil {
		t.Fatalf("Capture() = nil, want a refusal")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodeSplitAmountTooBig {
		t.Fatalf("code = %q, want %q — err: %v", code, domain.CodeSplitAmountTooBig, err)
	}
}

// ---------------------------------------------------------------------------
// The acquirer's own split errors
// ---------------------------------------------------------------------------

func TestProviderSplitErrorsCarryOurCodes(t *testing.T) {
	// Every message here is copied verbatim from the «Ошибки» table of the
	// split-payments section of https://developers.tiptoppay.kz.
	tests := []struct {
		message  string
		wantCode domain.ErrorCode
	}{
		{"Split transaction is not supported", domain.CodeSplitNotSupported},
		{"Split transaction is not supported on the Acquirer side", domain.CodeSplitNotSupported},
		{"Subscription is not supported for Splits", domain.CodeSplitNotSupported},
		{"SubMerchant not found", domain.CodeSplitSubMerchantUnknown},
		{"SubMerchants is disabled: test_api_00000000000000000000003, test_api_00000000000000000000004", domain.CodeSplitSubMerchantUnknown},
		{"Split transaction error. Terminals modes not equal", domain.CodeSplitTerminalMismatch},
		{`Pay on terminal with property "IsSplitSubMerchant" not supported`, domain.CodeSplitTerminalMismatch},
		{"Amount is not equal to request amount", domain.CodeSplitSumMismatch},
		{"Duplicated PublicIds for splits", domain.CodeSplitAccountDuplicate},
		{`Field "Splits" is required`, domain.CodeSplitRequired},
		{"Amount is too big", domain.CodeSplitAmountTooBig},
	}

	for _, tc := range tests {
		t.Run(tc.message, func(t *testing.T) {
			f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
				rejectedMsg(w, tc.message)
			})
			g := f.splitGateway(t, true)

			req := authorizeRequest()
			req.Splits = splitPlan()
			_, err := g.Authorize(context.Background(), req)
			if err == nil {
				t.Fatalf("Authorize() = nil, want the acquirer's refusal")
			}
			if code, _ := domain.CodeOf(err); code != tc.wantCode {
				t.Fatalf("code = %q, want %q — err: %v", code, tc.wantCode, err)
			}
			// It is still a definite acquirer NO, so the payment machinery
			// treats it as a terminal failure rather than something to
			// reconcile.
			if !errorsIsProviderRejected(err) {
				t.Fatalf("err = %v, want it to wrap ErrProviderRejected", err)
			}
		})
	}
}

func TestAnUnrecognisedRejectionKeepsTheGenericBehaviour(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		rejected(w, "Insufficient funds")
	})
	g := f.splitGateway(t, true)

	_, err := g.Authorize(context.Background(), authorizeRequest())
	if err == nil {
		t.Fatalf("Authorize() = nil, want a refusal")
	}
	if code, present := domain.CodeOf(err); present {
		t.Fatalf("code = %q, want none for a message that is not about splits", code)
	}
}

// TestSplitErrorsAndLogsCarryNoSubMerchantIDs guards the same rule the adapter
// already holds for credentials: an identifier that addresses somebody's money
// never appears in an error string.
func TestSplitErrorsAndLogsCarryNoSubMerchantIDs(t *testing.T) {
	f := newFakeAcquirer(t, func(_ string, _ int, _ map[string]any, w http.ResponseWriter) {
		t.Errorf("the acquirer must not be called")
		ok(w, `{"Id":"x"}`)
	})
	g := f.splitGateway(t, true)

	req := authorizeRequest()
	plan := splitPlan()
	plan[1].AccountRef = plan[0].AccountRef // duplicate → rejected before the call
	req.Splits = plan

	_, err := g.Authorize(context.Background(), req)
	if err == nil {
		t.Fatalf("Authorize() = nil, want a refusal")
	}
	if containsAny(err.Error(), venueSubMerchant, platformSubMerchant) {
		t.Fatalf("error text leaks a sub-merchant id: %v", err)
	}
}

// --- small helpers -------------------------------------------------------

func sumSplitAmounts(splits map[string]string) (int64, error) {
	var sum int64
	for _, amount := range splits {
		minor, err := payment.ParseMinor(amount)
		if err != nil {
			return 0, err
		}
		sum += minor
	}
	return sum, nil
}

func errorsIsProviderRejected(err error) bool {
	return errors.Is(err, payment.ErrProviderRejected)
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

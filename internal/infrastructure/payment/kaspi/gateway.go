package kaspi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// Gateway is the Kaspi Pay implementation of domain.PaymentGateway. See the
// package doc for the one-stage flow and what each method really does.
type Gateway struct {
	cfg  Config
	http *payment.Client
	log  *slog.Logger
	now  func() time.Time
}

var _ domain.PaymentGateway = (*Gateway)(nil)

// New builds the adapter. client may be nil, in which case a default one is
// created.
func New(cfg Config, client *payment.Client, log *slog.Logger) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = payment.NewClient(nil, payment.DefaultConfig(), log)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Gateway{cfg: cfg, http: client, log: log, now: time.Now}, nil
}

// Name reports the provider code this adapter serves.
func (g *Gateway) Name() domain.PaymentProvider { return domain.ProviderKaspi }

// qrData is the payload the Kaspi service passes through from Kaspi. Only the
// fields we act on are declared; everything else is kept in Raw.
type qrData struct {
	QrOperationId json.Number `json:"QrOperationId"`
	QrToken       string      `json:"QrToken"`
	Amount        json.Number `json:"Amount"`
	ExpireDate    string      `json:"ExpireDate"`
	Status        string      `json:"Status"`
	StatusDesc    string      `json:"StatusDesc"`
	ReceiptUrl    string      `json:"ReceiptUrl"`
}

type qrEnvelope struct {
	Data       *qrData     `json:"Data"`
	StatusCode json.Number `json:"StatusCode"`
	Message    string      `json:"Message"`
	// Error is the Kaspi SERVICE's own error shape (its 4xx/5xx bodies), as
	// opposed to Kaspi's StatusCode envelope above.
	Error string `json:"error"`
}

// Authorize creates the payment LINK. It moves no money: the returned payment
// is `created` and stays that way until the guest actually pays and the
// service's webhook says so.
//
// The call is NOT marked idempotent for the shared client's retry logic, and
// that is deliberate: the Kaspi service's /api/qr/create accepts no
// idempotency key, so a retry after a timeout would create a SECOND payable
// link for the same booking. One unpaid link that expires by itself is a
// harmless outcome; two live links for one booking are how a guest pays twice.
// A transport failure here therefore surfaces as ErrProviderOutcomeUnknown and
// no payment row is written (the checkout only writes after a successful
// answer).
func (g *Gateway) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}
	tenge, err := toTenge(req.Amount)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"amount": tenge})
	if err != nil {
		return nil, fmt.Errorf("kaspi: encode qr/create request: %w", err)
	}

	var env qrEnvelope
	raw, err := g.call(ctx, callSpec{
		op:        "qr/create",
		method:    http.MethodPost,
		path:      "/api/qr/create",
		companyID: req.MerchantAccountRef,
		body:      body,
		// See the doc comment: never retried.
		idempotent: false,
	}, &env)
	if err != nil {
		return nil, err
	}
	if env.Data == nil || strings.TrimSpace(env.Data.QrOperationId.String()) == "" {
		return nil, fmt.Errorf("kaspi qr/create: no operation id in the answer: %w", payment.ErrProviderMalformed)
	}

	out := &domain.GatewayPayment{
		ProviderPaymentID: env.Data.QrOperationId.String(),
		Status:            domain.PaymentCreated,
		Amount:            req.Amount,
		PaymentURL:        payLink(env.Data.QrToken),
		Raw:               raw,
	}
	// The link's lifetime is Kaspi's answer, never our own guess: a Kaspi QR
	// token lives minutes, and telling the app a longer deadline than the one
	// Kaspi enforces means showing a guest a countdown on a dead link.
	if exp := parseKaspiTime(env.Data.ExpireDate); !exp.IsZero() {
		out.ExpiresAt = &exp
	}
	g.log.Info("kaspi payment link created",
		slog.String("payment_id", req.PaymentID.String()),
		slog.String("provider_payment_id", out.ProviderPaymentID),
		// The company id addresses a venue's money; it identifies, it does not
		// authorise, and it is already stored in restaurant_split_accounts.
		slog.String("company", req.MerchantAccountRef),
		slog.Bool("has_expiry", out.ExpiresAt != nil),
	)
	return out, nil
}

// Capture CONFIRMS a one-stage payment that already happened; it moves no
// money.
//
// Kaspi has no hold to clear, so the honest implementation of "capture" for
// this acquirer is to ask it whether the money really moved and answer:
//
//	Processed              → captured (the usecase may book the ledger);
//	a definite refusal     → domain.ErrProviderDeclined, so the caller
//	                         releases its capture claim back to `authorized`
//	                         instead of leaving the payment stuck;
//	still pending, unknown
//	status, or unreachable → domain.ErrProviderOutcomeUnknown, so the caller
//	                         leaves the payment in `capturing` for the
//	                         reconciler rather than guessing.
//
// amount is checked against what Kaspi reports, not trusted: a capture for a
// different amount than the payment's own total must not be booked.
func (g *Gateway) Capture(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("kaspi: capture amount must be positive: %w", domain.ErrValidation)
	}
	got, err := g.Get(ctx, providerPaymentID)
	if err != nil {
		return nil, err
	}
	switch got.Status {
	case domain.PaymentCaptured:
		if got.Amount.AmountMinor != 0 && got.Amount.AmountMinor != amount.AmountMinor {
			return nil, fmt.Errorf(
				"kaspi capture: payment %s was paid for %d minor, not the expected %d minor: %w",
				providerPaymentID, got.Amount.AmountMinor, amount.AmountMinor, domain.ErrProviderOutcomeUnknown)
		}
		return got, nil
	case domain.PaymentFailed, domain.PaymentExpired:
		return nil, fmt.Errorf("kaspi capture: payment %s is %s at Kaspi: %w",
			providerPaymentID, got.Status, domain.ErrProviderDeclined)
	default:
		return nil, fmt.Errorf("kaspi capture: payment %s is not paid yet (%s): %w",
			providerPaymentID, got.Status, domain.ErrProviderOutcomeUnknown)
	}
}

// Void releases a link that was never paid — which, at Kaspi, means doing
// nothing: an unpaid QR token dies on its own at ExpireDate and there is no
// call that cancels one.
//
// A PAID payment is refused outright. Turning a void into a silent refund
// would move real money on a code path whose callers (a lost race between two
// checkouts, a rejected seating) believe nothing is moving; if the money must
// come back it goes through Refund, deliberately.
func (g *Gateway) Void(ctx context.Context, providerPaymentID string) error {
	if err := requireID(providerPaymentID); err != nil {
		return err
	}
	got, err := g.Get(ctx, providerPaymentID)
	if err != nil {
		return err
	}
	if got.Status == domain.PaymentCaptured {
		return fmt.Errorf(
			"kaspi void: payment %s is already paid — Kaspi has no hold to release, this needs a refund decision: %w",
			providerPaymentID, domain.ErrInvalidStatus)
	}
	g.log.Info("kaspi void is a no-op on an unpaid link",
		slog.String("provider_payment_id", providerPaymentID),
		slog.String("kaspi_status", string(got.Status)))
	return nil
}

// Refund sends money back through the Kaspi service. This is the only call in
// this adapter that moves money, and it moves it for real — Kaspi has no
// sandbox.
//
// It is not marked idempotent for the retry logic: /api/refund/create carries
// no idempotency key, so a retried refund is a second refund. The service's
// own guard (a refund may not exceed the original payment) limits the damage
// but does not prevent two partial refunds adding up.
func (g *Gateway) Refund(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayRefund, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	tenge, err := toTenge(amount)
	if err != nil {
		return nil, err
	}
	companyID, err := g.companyForPayment(providerPaymentID)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"qrOperationId": providerPaymentID, "returnAmount": tenge})
	if err != nil {
		return nil, fmt.Errorf("kaspi: encode refund request: %w", err)
	}

	var env qrEnvelope
	raw, err := g.call(ctx, callSpec{
		op:         "refund/create",
		method:     http.MethodPost,
		path:       "/api/refund/create",
		companyID:  companyID,
		body:       body,
		idempotent: false,
	}, &env)
	if err != nil {
		return nil, err
	}
	return &domain.GatewayRefund{
		// The Kaspi service has no separate refund id of its own on the wire;
		// its refund rows are keyed by the original payment. Echoing the
		// payment id keeps the field non-empty and truthful about what it
		// identifies.
		ProviderRefundID: providerPaymentID,
		Status:           domain.RefundSucceeded,
		Amount:           amount,
		Raw:              raw,
	}, nil
}

// Get reads the Kaspi service's view of a payment — the reconciliation path.
func (g *Gateway) Get(ctx context.Context, providerPaymentID string) (*domain.GatewayPayment, error) {
	if err := requireID(providerPaymentID); err != nil {
		return nil, err
	}
	companyID, err := g.companyForPayment(providerPaymentID)
	if err != nil {
		return nil, err
	}

	var env qrEnvelope
	raw, err := g.call(ctx, callSpec{
		op:         "qr/status",
		method:     http.MethodGet,
		path:       "/api/qr/status?qrOperationId=" + url.QueryEscape(providerPaymentID),
		companyID:  companyID,
		idempotent: true,
	}, &env)
	if err != nil {
		return nil, err
	}
	if env.Data == nil {
		return nil, fmt.Errorf("kaspi qr/status: empty answer: %w", payment.ErrProviderMalformed)
	}
	status, known := mapQrStatus(env.Data.Status)
	if !known {
		// Never read an unrecognised status as paid.
		return nil, fmt.Errorf("kaspi qr/status: unknown status %q for payment %s: %w",
			env.Data.Status, providerPaymentID, domain.ErrProviderOutcomeUnknown)
	}

	out := &domain.GatewayPayment{
		ProviderPaymentID: providerPaymentID,
		Status:            status,
		PaymentURL:        payLink(env.Data.QrToken),
		Raw:               raw,
	}
	if minor, err := fromTenge(env.Data.Amount); err == nil && minor > 0 {
		out.Amount = domain.Money{AmountMinor: minor, Currency: domain.CurrencyKZT}
	}
	if exp := parseKaspiTime(env.Data.ExpireDate); !exp.IsZero() {
		out.ExpiresAt = &exp
	}
	if status == domain.PaymentCaptured {
		now := g.now()
		out.AuthorizedAt, out.CapturedAt = &now, &now
	}
	if status == domain.PaymentFailed || status == domain.PaymentExpired {
		out.FailureCode = env.Data.Status
		out.FailureMessage = env.Data.StatusDesc
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// transport
// ---------------------------------------------------------------------------

type callSpec struct {
	op         string
	method     string
	path       string
	companyID  string
	body       []byte
	idempotent bool
}

// call performs one request against the Kaspi service and unwraps its answer.
// Nothing it returns ever contains the API key, the basic-auth password or the
// URL.
func (g *Gateway) call(ctx context.Context, spec callSpec, out *qrEnvelope) (json.RawMessage, error) {
	apiKey, err := g.cfg.apiKeyFor(spec.companyID)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("X-Api-Key", apiKey)
	if len(spec.body) > 0 {
		header.Set("Content-Type", "application/json")
	}
	if g.cfg.BasicAuthUser != "" {
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(g.cfg.BasicAuthUser+":"+g.cfg.BasicAuthPassword)))
	}

	resp, err := g.http.Do(ctx, payment.Request{
		Provider:   domain.ProviderKaspi,
		Op:         spec.op,
		Method:     spec.method,
		URL:        g.cfg.BaseURL + spec.path,
		Header:     header,
		Body:       spec.body,
		Idempotent: spec.idempotent,
	})
	if err != nil {
		return nil, fmt.Errorf("kaspi %s: %w", spec.op, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Do NOT echo the body: a 401 from Caddy or from the service may
		// repeat what we sent.
		return nil, fmt.Errorf("kaspi %s: credentials rejected: %w", spec.op, payment.ErrProviderRejected)
	}

	if err := json.Unmarshal(resp.Body, out); err != nil {
		return resp.Body, fmt.Errorf("kaspi %s: %w", spec.op, payment.ErrProviderMalformed)
	}
	if resp.StatusCode != http.StatusOK {
		// 409 is the service saying "the cashier session died" — an operator
		// problem, not a decline: the payment was never created, but neither
		// was it refused, so it must not be read as a definite "no" that a
		// caller could turn into a failed payment.
		if resp.StatusCode == http.StatusConflict || resp.StatusCode >= http.StatusInternalServerError {
			return resp.Body, fmt.Errorf("kaspi %s: %s: %w",
				spec.op, sanitise(out.Error), domain.ErrProviderOutcomeUnknown)
		}
		return resp.Body, fmt.Errorf("kaspi %s: %s: %w", spec.op, sanitise(out.Error), payment.ErrProviderRejected)
	}
	// Kaspi answers HTTP 200 with a non-zero StatusCode when it refuses.
	if code := strings.TrimSpace(out.StatusCode.String()); code != "" && code != "0" {
		return resp.Body, fmt.Errorf("kaspi %s: Kaspi status %s: %s: %w",
			spec.op, code, sanitise(out.Message), payment.ErrProviderRejected)
	}
	return resp.Body, nil
}

// companyForPayment answers which company's key to use for a call about an
// EXISTING payment.
//
// The acquirer-side id alone does not say which company owns it, and we do not
// carry that mapping here (it lives on the payment's restaurant, in
// restaurant_split_accounts). With a single configured company — the beta
// shape — the answer is unambiguous. With several it is not, and guessing
// would mean asking company A about company B's payment, which the Kaspi
// service correctly answers with 404.
//
// TODO(multi-company): when a second company goes live, Get/Void/Capture/Refund
// need the company id passed in, which means widening domain.PaymentGateway (or
// resolving it from the payment's restaurant before the call). Refusing loudly
// beats silently probing every key.
func (g *Gateway) companyForPayment(providerPaymentID string) (string, error) {
	if len(g.cfg.CompanyAPIKeys) == 1 {
		for id := range g.cfg.CompanyAPIKeys {
			return id, nil
		}
	}
	return "", fmt.Errorf(
		"kaspi: cannot tell which company owns payment %s — %d companies are configured and the acquirer id does not name one: %w",
		providerPaymentID, len(g.cfg.CompanyAPIKeys), domain.ErrProviderOutcomeUnknown)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func validateAuthorize(req domain.AuthorizeRequest) error {
	switch {
	case req.PaymentID == uuid.Nil:
		return fmt.Errorf("kaspi: authorize without a payment id: %w", domain.ErrValidation)
	case req.IdempotencyKey == "":
		return fmt.Errorf("kaspi: authorize without an idempotency key: %w", domain.ErrValidation)
	case strings.TrimSpace(req.MerchantAccountRef) == "":
		// Which company's money this is cannot be guessed. A venue with no
		// mapping is not "the default company" — it is a venue nobody has
		// finished onboarding, and charging its guests would credit somebody
		// else's account.
		return domain.WithCode(domain.CodeSplitAccountMissing, fmt.Errorf(
			"kaspi: this venue has no Kaspi company configured, its payments cannot be routed: %w",
			domain.ErrUnavailable))
	case !req.Splits.IsZero():
		// An adapter for an acquirer without split support must REFUSE a
		// non-empty plan rather than drop it (domain.AuthorizeRequest.Splits):
		// the Kaspi service divides money by COMPANY, not per charge.
		return fmt.Errorf("kaspi: split payments are not supported by this acquirer: %w", domain.ErrValidation)
	}
	return nil
}

func requireID(providerPaymentID string) error {
	if strings.TrimSpace(providerPaymentID) == "" {
		return fmt.Errorf("kaspi: empty provider payment id: %w", domain.ErrValidation)
	}
	return nil
}

// sanitise trims a provider message to something safe and short for an error.
func sanitise(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "no details"
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

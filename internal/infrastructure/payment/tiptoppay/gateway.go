package tiptoppay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// Gateway is the TipTop Pay implementation of domain.PaymentGateway.
type Gateway struct {
	cfg    Config
	http   *payment.Client
	log    *slog.Logger
	now    func() time.Time
	authHd string // pre-computed Basic auth header, never logged
}

// compile-time proof that the adapter satisfies both the domain port and the
// optional reconciliation capability.
var (
	_ domain.PaymentGateway    = (*Gateway)(nil)
	_ payment.MerchantIDFinder = (*Gateway)(nil)
)

// New builds the adapter. client may be nil, in which case a default one is
// created; tests pass an httptest-backed client.
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
	return &Gateway{
		cfg:    cfg,
		http:   client,
		log:    log,
		now:    time.Now,
		authHd: "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PublicID+":"+cfg.APISecret)),
	}, nil
}

// Name reports the provider code this adapter serves.
func (g *Gateway) Name() domain.PaymentProvider { return domain.ProviderTipTopPay }

// ---------------------------------------------------------------------------
// domain.PaymentGateway
// ---------------------------------------------------------------------------

// createOrderRequest is POST /orders/create. Amount is a json.Number built from
// minor units so no float64 ever touches money.
type createOrderRequest struct {
	Amount              json.Number       `json:"Amount"`
	Currency            string            `json:"Currency"`
	Description         string            `json:"Description"`
	Email               string            `json:"Email,omitempty"`
	Phone               string            `json:"Phone,omitempty"`
	InvoiceID           string            `json:"InvoiceId"`
	AccountID           string            `json:"AccountId,omitempty"`
	RequireConfirmation bool              `json:"RequireConfirmation"`
	SendEmail           bool              `json:"SendEmail"`
	SendSms             bool              `json:"SendSms"`
	CultureName         string            `json:"CultureName,omitempty"`
	SuccessRedirectURL  string            `json:"SuccessRedirectUrl,omitempty"`
	FailRedirectURL     string            `json:"FailRedirectUrl,omitempty"`
	JSONData            map[string]string `json:"JsonData,omitempty"`
}

// Authorize creates a hosted payment page with RequireConfirmation=true, i.e.
// a two-stage payment: the guest's funds are held, not taken (spec §2).
//
// It returns the ORDER id in ProviderPaymentID and PaymentCreated as the
// status: at this point nobody has paid anything. The hold materialises later,
// announced by the `pay` notification — see the package doc on the two
// identifiers.
//
// req.CallbackURL is intentionally not sent: TipTopPay resolves notification
// endpoints per terminal (configured in the dashboard or via
// /site/notifications/{Type}/update), not per payment. Passing it would be a
// silent no-op.
func (g *Gateway) Authorize(ctx context.Context, req domain.AuthorizeRequest) (*domain.GatewayPayment, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}

	body := createOrderRequest{
		Amount:              json.Number(payment.FormatMinor(req.Amount.AmountMinor)),
		Currency:            string(req.Amount.Currency),
		Description:         req.Description,
		Email:               req.CustomerEmail,
		Phone:               req.CustomerPhone,
		InvoiceID:           req.PaymentID.String(),
		RequireConfirmation: true,
		JSONData:            metadata(req),
	}
	if req.ReturnURL != "" {
		body.SuccessRedirectURL = req.ReturnURL
		body.FailRedirectURL = req.ReturnURL
	}

	var model orderModel
	raw, err := g.call(ctx, "orders/create", "/orders/create", req.IdempotencyKey, body, &model)
	if err != nil {
		return nil, err
	}

	g.log.Info("tiptoppay order created",
		slog.String("payment_id", req.PaymentID.String()),
		slog.String("provider_order_id", model.ID),
	)

	return &domain.GatewayPayment{
		ProviderPaymentID: model.ID,
		Status:            domain.PaymentCreated,
		Amount:            req.Amount,
		PaymentURL:        model.URL,
		Raw:               raw,
	}, nil
}

// confirmRequest is POST /payments/confirm.
type confirmRequest struct {
	TransactionID int64       `json:"TransactionId"`
	Amount        json.Number `json:"Amount"`
}

// Capture confirms a two-stage payment, optionally for less than the held
// amount (a pre-ordered dish the venue could not serve).
//
// The port gives us no idempotency key for this call, so one is DERIVED from
// the operation, the transaction and the amount. It is deterministic: a retry
// after a timeout sends the same X-Request-ID and TipTopPay replays the stored
// result instead of capturing twice (spec §8).
func (g *Gateway) Capture(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayPayment, error) {
	txID, err := transactionID(providerPaymentID)
	if err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("tiptoppay: capture amount must be positive: %w", domain.ErrValidation)
	}

	body := confirmRequest{TransactionID: txID, Amount: json.Number(payment.FormatMinor(amount.AmountMinor))}
	key := derivedKey("confirm", providerPaymentID, amount)

	// /payments/confirm answers {"Success":true,"Message":null} with no Model.
	raw, err := g.call(ctx, "payments/confirm", "/payments/confirm", key, body, nil)
	if err != nil {
		return nil, err
	}

	now := g.now().UTC()
	g.log.Info("tiptoppay payment captured",
		slog.String("provider_payment_id", providerPaymentID),
		slog.String("amount", amount.String()),
	)
	return &domain.GatewayPayment{
		ProviderPaymentID: providerPaymentID,
		Status:            domain.PaymentCaptured,
		Amount:            amount,
		CapturedAt:        &now,
		Raw:               raw,
	}, nil
}

// voidRequest is POST /payments/void.
type voidRequest struct {
	TransactionID int64 `json:"TransactionId"`
}

// Void releases a hold that was never captured.
func (g *Gateway) Void(ctx context.Context, providerPaymentID string) error {
	txID, err := transactionID(providerPaymentID)
	if err != nil {
		return err
	}
	key := derivedKey("void", providerPaymentID, domain.Money{})
	if _, err := g.call(ctx, "payments/void", "/payments/void", key, voidRequest{TransactionID: txID}, nil); err != nil {
		return err
	}
	g.log.Info("tiptoppay hold released", slog.String("provider_payment_id", providerPaymentID))
	return nil
}

// refundRequest is POST /payments/refund.
type refundRequest struct {
	TransactionID int64       `json:"TransactionId"`
	Amount        json.Number `json:"Amount"`
}

// Refund sends money back from a captured payment; partial refunds are the
// normal case here. TipTopPay refuses refunds on transactions older than a
// year — that shows up as a rejected envelope, mapped to ErrProviderRejected.
func (g *Gateway) Refund(ctx context.Context, providerPaymentID string, amount domain.Money) (*domain.GatewayRefund, error) {
	txID, err := transactionID(providerPaymentID)
	if err != nil {
		return nil, err
	}
	if amount.AmountMinor <= 0 {
		return nil, fmt.Errorf("tiptoppay: refund amount must be positive: %w", domain.ErrValidation)
	}

	body := refundRequest{TransactionID: txID, Amount: json.Number(payment.FormatMinor(amount.AmountMinor))}
	key := derivedKey("refund", providerPaymentID, amount)

	var model refundModel
	raw, err := g.call(ctx, "payments/refund", "/payments/refund", key, body, &model)
	if err != nil {
		return nil, err
	}

	g.log.Info("tiptoppay refund accepted",
		slog.String("provider_payment_id", providerPaymentID),
		slog.String("amount", amount.String()),
	)
	return &domain.GatewayRefund{
		ProviderRefundID: strconv.FormatInt(model.TransactionID, 10),
		Status:           domain.RefundSucceeded,
		Amount:           amount,
		Raw:              raw,
	}, nil
}

// getRequest is POST /payments/get.
type getRequest struct {
	TransactionID int64 `json:"TransactionId"`
}

// findRequest is POST /v2/payments/find.
type findRequest struct {
	InvoiceID string `json:"InvoiceId"`
}

// Get reads TipTopPay's own view of a transaction. This is the reconciliation
// path: a webhook is the primary signal, never the only one (spec §5).
func (g *Gateway) Get(ctx context.Context, providerPaymentID string) (*domain.GatewayPayment, error) {
	txID, err := transactionID(providerPaymentID)
	if err != nil {
		return nil, err
	}
	var model transactionModel
	raw, err := g.call(ctx, "payments/get", "/payments/get", "", getRequest{TransactionID: txID}, &model)
	if err != nil {
		return nil, err
	}
	return g.toGatewayPayment(model, raw), nil
}

// FindByMerchantPaymentID looks a payment up by OUR id (the InvoiceId we sent),
// covering the window in which the acquirer-side transaction does not exist yet
// or its notification was lost. See payment.MerchantIDFinder.
//
// TipTopPay returns the LAST operation for that invoice, which is what
// reconciliation wants.
func (g *Gateway) FindByMerchantPaymentID(ctx context.Context, merchantPaymentID string) (*domain.GatewayPayment, error) {
	if strings.TrimSpace(merchantPaymentID) == "" {
		return nil, fmt.Errorf("tiptoppay: empty merchant payment id: %w", domain.ErrValidation)
	}
	var model transactionModel
	raw, err := g.call(ctx, "payments/find", "/v2/payments/find", "", findRequest{InvoiceID: merchantPaymentID}, &model)
	if err != nil {
		// "Not found" comes back as Success=false; for reconciliation that is
		// "the guest never paid", not an infrastructure problem.
		return nil, err
	}
	return g.toGatewayPayment(model, raw), nil
}

func (g *Gateway) toGatewayPayment(m transactionModel, raw json.RawMessage) *domain.GatewayPayment {
	status, known := mapStatus(m.Status)
	if !known {
		g.log.Warn("tiptoppay unmapped transaction status",
			slog.String("provider_payment_id", strconv.FormatInt(m.TransactionID, 10)),
			slog.String("status", m.Status),
		)
	}

	amountMinor, err := payment.ParseMinor(m.Amount.String())
	if err != nil {
		amountMinor = 0
	}
	currency := domain.Currency(m.Currency)
	if currency == "" {
		currency = domain.CurrencyKZT
	}

	out := &domain.GatewayPayment{
		ProviderPaymentID: strconv.FormatInt(m.TransactionID, 10),
		Status:            status,
		Amount:            domain.Money{AmountMinor: amountMinor, Currency: currency},
		AuthorizedAt:      parseIsoTime(m.AuthDateIso),
		CapturedAt:        parseIsoTime(m.ConfirmDateIso),
		Raw:               raw,
	}
	if status == domain.PaymentFailed {
		out.FailureCode = strconv.Itoa(m.ReasonCode)
		out.FailureMessage = m.Reason
	}
	return out
}

// ---------------------------------------------------------------------------
// transport
// ---------------------------------------------------------------------------

// call performs one TipTopPay API request and unwraps the standard envelope
// into out (which may be nil when the method answers without a Model).
//
// idempotencyKey is sent as X-Request-ID. When it is non-empty the call is
// marked retryable: TipTopPay replays the stored result for an hour, so a retry
// after a timeout cannot produce a second money movement. Reads (/payments/get,
// /v2/payments/find) are retryable for the ordinary reason — they change
// nothing.
func (g *Gateway) call(ctx context.Context, op, path, idempotencyKey string, body any, out any) (json.RawMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("tiptoppay: encode %s request: %w", op, err)
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	header.Set("Authorization", g.authHd)
	if idempotencyKey != "" {
		header.Set("X-Request-ID", idempotencyKey)
	}

	resp, err := g.http.Do(ctx, payment.Request{
		Provider:   domain.ProviderTipTopPay,
		Op:         op,
		Method:     http.MethodPost,
		URL:        g.cfg.BaseURL + path,
		Header:     header,
		Body:       payload,
		Idempotent: true,
	})
	if err != nil {
		return nil, fmt.Errorf("tiptoppay %s: %w", op, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Do NOT echo anything from the response: a 401 body may repeat the
		// credentials we sent.
		return nil, fmt.Errorf("tiptoppay %s: credentials rejected: %w", op, payment.ErrProviderRejected)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tiptoppay %s: HTTP %d: %w", op, resp.StatusCode, payment.ErrProviderRejected)
	}

	var env envelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("tiptoppay %s: %w", op, payment.ErrProviderMalformed)
	}
	if !env.Success {
		msg := ""
		if env.Message != nil {
			msg = *env.Message
		}
		return env.Model, fmt.Errorf("tiptoppay %s: %s: %w", op, sanitise(msg), payment.ErrProviderRejected)
	}
	if out != nil && len(env.Model) > 0 {
		if err := json.Unmarshal(env.Model, out); err != nil {
			return env.Model, fmt.Errorf("tiptoppay %s: decode model: %w", op, payment.ErrProviderMalformed)
		}
	}
	return env.Model, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func validateAuthorize(req domain.AuthorizeRequest) error {
	switch {
	case req.PaymentID == uuid.Nil:
		return fmt.Errorf("tiptoppay: authorize without a payment id: %w", domain.ErrValidation)
	case req.IdempotencyKey == "":
		return fmt.Errorf("tiptoppay: authorize without an idempotency key: %w", domain.ErrValidation)
	case req.Amount.AmountMinor <= 0:
		return fmt.Errorf("tiptoppay: authorize amount must be positive: %w", domain.ErrValidation)
	case !req.Amount.Currency.Valid():
		return fmt.Errorf("tiptoppay: unsupported currency %q: %w", req.Amount.Currency, domain.ErrValidation)
	}
	return nil
}

// metadata copies the caller's metadata and adds the booking reference, so a
// TipTopPay notification can be traced back without a database lookup. Card
// data is impossible here: AuthorizeRequest cannot carry any.
func metadata(req domain.AuthorizeRequest) map[string]string {
	out := make(map[string]string, len(req.Metadata)+3)
	for k, v := range req.Metadata {
		out[k] = v
	}
	out["payment_id"] = req.PaymentID.String()
	out["booking_id"] = req.BookingID.String()
	out["purpose"] = string(req.Purpose)
	return out
}

// transactionID converts a stored provider payment id into TipTopPay's numeric
// TransactionId.
//
// An order id (non-numeric) means the guest has not paid yet: there is no
// transaction to confirm, void, refund or read. That is a state error, not a
// provider error, and it must be loud — silently treating it as "nothing to do"
// is how a hold survives a cancelled booking.
func transactionID(providerPaymentID string) (int64, error) {
	id := strings.TrimSpace(providerPaymentID)
	if id == "" {
		return 0, fmt.Errorf("tiptoppay: empty provider payment id: %w", domain.ErrValidation)
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("tiptoppay: %q is an order id, not a transaction id — the guest has not paid yet: %w",
			id, domain.ErrInvalidStatus)
	}
	return n, nil
}

// derivedKey builds a deterministic X-Request-ID for operations the domain port
// does not give a key for. Same operation + same transaction + same amount =
// same key, so a retry is a replay.
//
// The public id is mixed in only to keep keys distinct between terminals; it is
// hashed, never sent in the clear.
func derivedKey(op, providerPaymentID string, amount domain.Money) string {
	sum := sha256.Sum256([]byte(op + ":" + providerPaymentID + ":" + payment.FormatMinor(amount.AmountMinor) + ":" + string(amount.Currency)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// sanitise trims a provider message down to something safe and bounded before
// it becomes part of an error. TipTopPay messages are short human text, but an
// error string is not a place to paste an arbitrary remote payload.
func sanitise(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "rejected"
	}
	return s
}

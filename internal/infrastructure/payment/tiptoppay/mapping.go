package tiptoppay

import (
	"encoding/json"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// This file is the anti-corruption layer (spec §2): every TipTopPay word is
// translated into a domain word HERE and nowhere else. Nothing outside this
// package may branch on a TipTopPay status string or reason code.

// envelope is the answer shape of every TipTopPay API method.
type envelope struct {
	Model   json.RawMessage `json:"Model"`
	Success bool            `json:"Success"`
	Message *string         `json:"Message"`
}

// orderModel is /orders/create.
type orderModel struct {
	ID             string `json:"Id"`
	Number         int64  `json:"Number"`
	Currency       string `json:"Currency"`
	Description    string `json:"Description"`
	URL            string `json:"Url"`
	Status         string `json:"Status"`
	StatusCode     int    `json:"StatusCode"`
	CreatedDateIso string `json:"CreatedDateIso"`
	InternalID     int64  `json:"InternalId"`
}

// transactionModel is /payments/get and /v2/payments/find. Card fields are
// deliberately NOT declared: what we do not decode cannot end up in a log
// (spec §8). Amount is decoded as json.Number so it never becomes a float64.
type transactionModel struct {
	TransactionID     int64       `json:"TransactionId"`
	Amount            json.Number `json:"Amount"`
	Currency          string      `json:"Currency"`
	InvoiceID         string      `json:"InvoiceId"`
	Status            string      `json:"Status"`
	StatusCode        int         `json:"StatusCode"`
	Reason            string      `json:"Reason"`
	ReasonCode        int         `json:"ReasonCode"`
	AuthDateIso       string      `json:"AuthDateIso"`
	ConfirmDateIso    string      `json:"ConfirmDateIso"`
	CardHolderMessage string      `json:"CardHolderMessage"`
	Refunded          bool        `json:"Refunded"`
	TestMode          bool        `json:"TestMode"`
}

// refundModel is /payments/refund: it answers with the refund's own
// transaction id and nothing else.
type refundModel struct {
	TransactionID int64 `json:"TransactionId"`
}

// TipTopPay transaction statuses, verbatim from the "Статусы операций"
// reference table.
const (
	statusAwaitingAuthentication = "AwaitingAuthentication"
	statusAuthorized             = "Authorized"
	statusCompleted              = "Completed"
	statusCancelled              = "Cancelled"
	statusDeclined               = "Declined"
)

// mapStatus translates a TipTopPay transaction status into a domain one.
//
//	AwaitingAuthentication  the guest is at the issuer's 3-D Secure page  → created
//	Authorized              funds are held, two-stage flow                → authorized
//	Completed               confirmed / charged                          → captured
//	Cancelled               the hold was released or the payment voided   → voided
//	Declined                the acquirer or the issuer said no            → failed
//
// A status we do not know maps to `created` and is reported as unmapped: an
// unknown word from an acquirer must never be optimistically read as "paid".
func mapStatus(s string) (domain.PaymentStatus, bool) {
	switch strings.TrimSpace(s) {
	case statusAwaitingAuthentication:
		return domain.PaymentCreated, true
	case statusAuthorized:
		return domain.PaymentAuthorized, true
	case statusCompleted:
		return domain.PaymentCaptured, true
	case statusCancelled:
		return domain.PaymentVoided, true
	case statusDeclined:
		return domain.PaymentFailed, true
	default:
		return domain.PaymentCreated, false
	}
}

// Notification types, verbatim from the "Уведомления" section. TipTopPay sends
// each of them to its OWN configured URL, so the type is a property of the
// route, not of the body — the transport layer passes it in the
// NotificationTypeHeader (see webhook.go).
const (
	notifyCheck   = "check"
	notifyPay     = "pay"
	notifyFail    = "fail"
	notifyConfirm = "confirm"
	notifyRefund  = "refund"
	notifyCancel  = "cancel"
)

// mapNotification translates a notification type plus the transaction status it
// carries into a domain webhook event type.
//
// `pay` is ambiguous by design at TipTopPay: it fires for both one-stage
// charges (Status=Completed) and two-stage holds (Status=Authorized), which is
// why the status is consulted rather than assumed.
//
// `check` is the pre-authorisation callback. We answer it 200/{"code":0} but
// take no money decision from it, so it maps to WebhookUnknown — recorded,
// never acted upon.
func mapNotification(kind string, status domain.PaymentStatus) domain.WebhookEventType {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case notifyPay:
		if status == domain.PaymentCaptured {
			return domain.WebhookPaymentCaptured
		}
		return domain.WebhookPaymentAuthorized
	case notifyConfirm:
		return domain.WebhookPaymentCaptured
	case notifyFail:
		return domain.WebhookPaymentFailed
	case notifyCancel:
		return domain.WebhookPaymentVoided
	case notifyRefund:
		return domain.WebhookRefundSucceeded
	case notifyCheck:
		return domain.WebhookUnknown
	default:
		return domain.WebhookUnknown
	}
}

// statusForNotification is the payment status implied by a notification type
// when the body carries none (cancel and refund notifications have no Status
// field).
func statusForNotification(kind string, fallback domain.PaymentStatus) domain.PaymentStatus {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case notifyCancel:
		return domain.PaymentVoided
	case notifyFail:
		return domain.PaymentFailed
	case notifyConfirm:
		return domain.PaymentCaptured
	default:
		return fallback
	}
}

// notificationTimeLayout is the DateTime format of every notification:
// "yyyy-MM-dd HH:mm:ss" in UTC.
const notificationTimeLayout = "2006-01-02 15:04:05"

// isoTimeLayout is the *DateIso format of API models: "2021-10-30T04:00:02",
// documented as UTC and without a zone suffix.
const isoTimeLayout = "2006-01-02T15:04:05"

func parseNotificationTime(s string) time.Time {
	t, err := time.ParseInLocation(notificationTimeLayout, strings.TrimSpace(s), time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseIsoTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.ParseInLocation(isoTimeLayout, s, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}

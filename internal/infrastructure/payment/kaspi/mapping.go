package kaspi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// tiynPerTenge is the currency granularity Kaspi actually accepts. The Kaspi
// service (and Kaspi itself) takes a WHOLE number of tenge and rejects
// anything else, while every amount in this codebase is an integer number of
// tiyn. 100 is therefore not a formatting detail but a hard constraint on what
// this acquirer can be asked to charge — see MinChargeableUnitMinor.
const tiynPerTenge = 100

// MinChargeableUnitMinor reports the smallest amount step Kaspi can charge, in
// minor units: one whole tenge.
//
// It is an OPTIONAL capability the checkout type-asserts for (see
// usecase/payments.amountGranularity), not part of domain.PaymentGateway: only
// an acquirer that cannot charge sub-unit amounts needs it. The checkout uses
// it to round the payment total UP to a whole tenge BEFORE the payment row is
// written, so the amount we store, the amount we charge and the amount the
// webhook reports back are the same number. Rounding here instead, inside
// Authorize, would charge one number and record another — which is exactly how
// a ledger stops matching a bank statement.
func (g *Gateway) MinChargeableUnitMinor() int64 { return tiynPerTenge }

// RequiresMerchantAccount reports that this acquirer cannot charge for a venue
// that has no Kaspi company mapped to it (restaurant_split_accounts): the
// money belongs to a COMPANY, and there is no default company to fall back on.
//
// It is the DECLARATION of the rule validateAuthorize already enforces — same
// answer, only askable before a payment exists, so the guest app can be told
// whether this venue takes online payment at all instead of finding out from a
// refused charge. Another OPTIONAL capability the usecase type-asserts for
// (usecase/payments.merchantAccountRequirer), for the same reason
// MinChargeableUnitMinor is one: no other acquirer here needs it.
func (g *Gateway) RequiresMerchantAccount() bool { return true }

// toTenge converts minor units to the whole tenge the Kaspi service expects.
// A fractional amount is REFUSED, never rounded: the caller has already
// promised (via MinChargeableUnitMinor) that the total is whole, and silently
// charging a rounded number would break the amount check the capture webhook
// performs against the payment's own total.
func toTenge(m domain.Money) (int64, error) {
	if m.AmountMinor <= 0 {
		return 0, fmt.Errorf("kaspi: amount must be positive: %w", domain.ErrValidation)
	}
	if m.AmountMinor%tiynPerTenge != 0 {
		return 0, fmt.Errorf(
			"kaspi: amount %d tiyn is not a whole number of tenge and Kaspi cannot charge it: %w",
			m.AmountMinor, domain.ErrValidation)
	}
	if m.Currency != domain.CurrencyKZT {
		return 0, fmt.Errorf("kaspi: unsupported currency %q, Kaspi settles KZT only: %w", m.Currency, domain.ErrValidation)
	}
	return m.AmountMinor / tiynPerTenge, nil
}

// fromTenge converts a whole-tenge amount reported by the service back into
// minor units.
func fromTenge(tenge json.Number) (int64, error) {
	minor, err := payment.ParseMinor(tenge.String())
	if err != nil {
		return 0, fmt.Errorf("kaspi: unreadable amount: %w", payment.ErrProviderMalformed)
	}
	return minor, nil
}

// qrStatuses maps Kaspi's own QR statuses onto the payment state machine. It
// is a copy of the mapping the Kaspi service uses to decide which webhook to
// fire (src/kaspi/status.js) — the two must stay in step, which is why the
// names are listed literally instead of being guessed by prefix.
//
// The rule that must never be inverted: a status this build does not
// recognise is NOT success. It returns ok=false and the caller treats it as an
// unknown outcome to be reconciled, never as money received.
var qrStatuses = map[string]domain.PaymentStatus{
	// still in flight — the guest has not finished (or even opened) the link
	"QrTokenCreated": domain.PaymentCreated,
	"Wait":           domain.PaymentCreated,
	// the money moved
	"Processed": domain.PaymentCaptured,
	// the link died without a payment
	"QrTokenDiscarded": domain.PaymentExpired,
	"Expired":          domain.PaymentExpired,
	// definite refusals
	"CancelledByUser":           domain.PaymentFailed,
	"NotConfirmedByUser":        domain.PaymentFailed,
	"CancelledByExternalSource": domain.PaymentFailed,
	"ProcessingFailed":          domain.PaymentFailed,
	"Rejected":                  domain.PaymentFailed,
	"InsufficientFunds":         domain.PaymentFailed,
	"InsufficientFundsError":    domain.PaymentFailed,
	"Error":                     domain.PaymentFailed,
	"IrisSrcBlockCode1":         domain.PaymentFailed,
	"IrisSrcBlockCode3":         domain.PaymentFailed,
	"IrisSrcBlockCode9":         domain.PaymentFailed,
	"IrisDestBlockCode3":        domain.PaymentFailed,
	"IrisDestBlockCode5":        domain.PaymentFailed,
	"IrisDestBlockCode7":        domain.PaymentFailed,
	"IrisDestBlockCode10":       domain.PaymentFailed,
}

// mapQrStatus translates a Kaspi QR status. ok=false means "not a status this
// build knows" — the caller must treat that as unknown, never as paid.
func mapQrStatus(status string) (domain.PaymentStatus, bool) {
	s, ok := qrStatuses[strings.TrimSpace(status)]
	return s, ok
}

// almatyOffset is the fixed UTC offset Kazakhstan has used countrywide since
// 1 March 2024 (UTC+5, no DST). It is used only as the fallback for a
// timestamp that arrives WITHOUT an offset.
//
// TODO(verify): проверить на реальном ответе Kaspi — приходит ли ExpireDate с
// таймзоной. FreedomPay's sandbox turned up a field in local Almaty time with
// no offset right next to one in UTC, so an unmarked timestamp is not a
// theoretical case; if Kaspi's is actually UTC this fallback makes a link look
// five hours longer-lived than it is.
var almatyOffset = time.FixedZone("Asia/Almaty", 5*60*60)

// parseKaspiTime reads a timestamp from the service. It accepts RFC3339 (what
// the service's own dry-run mode and its SQLite rows produce) and, as a
// fallback, an offset-less local timestamp, which is read as Almaty time.
// An unparseable value yields the zero time and no error: a missing expiry is
// a degraded answer, not a reason to refuse a payment link that was already
// created.
func parseKaspiTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, almatyOffset); err == nil {
			return t
		}
	}
	return time.Time{}
}

// payLink normalises the token the service returns into the link a guest can
// open. The service already rewrites qr.kaspi.kz → pay.kaspi.kz for the
// multi-tenant route; this repeats the rewrite so that a payload from the
// older single-tenant path (or a future change on that side) cannot hand a
// guest a URL their phone will not open.
func payLink(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return strings.Replace(token, "https://qr.kaspi.kz/", "https://pay.kaspi.kz/pay/", 1)
}

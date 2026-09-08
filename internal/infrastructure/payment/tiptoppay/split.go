package tiptoppay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/payment"
)

// ---------------------------------------------------------------------------
// Split payments — https://developers.tiptoppay.kz, section «Сплитование
// платежей» (fetched 2026-08-20, doc version 1.0.2 of 23.04.2025).
//
// What the documentation actually says, because half of the traps are in what
// it does NOT say:
//
//   - The wire shape is one array, `Splits`, of {PublicId, Amount, JsonData?}.
//     Amount is decimal with a dot and two decimals, and the sum of all
//     Splits.Amount MUST equal the request's own Amount — otherwise "Amount is
//     not equal to request amount".
//   - Splits are listed as supported on /payments/cards/{charge,auth} and
//     /payments/tokens/{charge,auth}. They are NOT listed on /orders/create,
//     which is the method this adapter's Authorize uses. See
//     Config.SplitViaOrders and docs/payments/tiptoppay-splits.md — this is the one open
//     question that blocks a live split end to end.
//   - Two-stage flow: /payments/confirm MUST carry the Splits to confirm.
//     Omitting a PublicId that was sent at payment time CANCELS that share in
//     full. Confirming more than the share was worth is an error.
//   - /payments/void needs no change: it releases every share.
//   - /payments/refund MUST carry Splits for a split payment ("Field
//     \"Splits\" is required"), full or partial, and never more than what was
//     charged for that PublicId.
//   - /payments/get, /v2/payments/find and the Pay/Confirm/Refund/Fail/Cancel
//     notifications echo back the Splits that were sent. That echo is what lets
//     Capture and Refund below rebuild the original division without inventing
//     a second store of it.
//   - Subscriptions are not supported for split payments; not a concern here
//     (this adapter never creates one).
// ---------------------------------------------------------------------------

// splitEntry is one element of the TipTopPay `Splits` array.
//
// Amount is json.Number produced by payment.FormatMinor from integer minor
// units: no float64 ever touches a share, for the same reason it never touches
// a total.
//
// JsonData is left as a raw message and is only ever populated by what the
// caller already gave us. It is the documented place for a per-share fiscal
// receipt (the cash-register integration wraps it in a PaymentData object); we
// have no cash register wired, so we send nothing rather than guess a receipt
// shape.
type splitEntry struct {
	PublicID string          `json:"PublicId"`
	Amount   json.Number     `json:"Amount"`
	JSONData json.RawMessage `json:"JsonData,omitempty"`
}

// toSplitEntries converts a validated domain plan into the wire array.
//
// It re-runs the domain validation against total on purpose. The usecase
// validates too, but this adapter is the last place before the money moves and
// it is reached from more than one caller (Authorize, Capture, Refund) — a
// check that only exists upstream is a check that a future caller can skip.
func toSplitEntries(plan domain.PaymentSplitPlan, total domain.Money) ([]splitEntry, error) {
	if plan.IsZero() {
		return nil, nil
	}
	if err := plan.Validate(total); err != nil {
		return nil, fmt.Errorf("tiptoppay: %w", err)
	}
	out := make([]splitEntry, 0, len(plan))
	for _, s := range plan {
		out = append(out, splitEntry{
			PublicID: strings.TrimSpace(s.AccountRef),
			Amount:   json.Number(payment.FormatMinor(s.Amount.AmountMinor)),
		})
	}
	return out, nil
}

// parseSplitEntries reads the array TipTopPay echoed back into integer minor
// units, rejecting anything that is not a usable share.
//
// Note what it does NOT do: it does not try to work out which share is the
// venue's and which is ours. A confirm or a refund only has to hand back the
// same accounts with new amounts, and inventing a payee role for an echoed
// account id would be a guess this package has no way to check.
func parseSplitEntries(entries []splitEntry) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]int64, len(entries))
	for i, e := range entries {
		ref := strings.TrimSpace(e.PublicID)
		if ref == "" {
			return nil, fmt.Errorf("tiptoppay: split %d came back without a PublicId: %w", i, payment.ErrProviderMalformed)
		}
		if _, dup := seen[ref]; dup {
			return nil, domain.WithCode(domain.CodeSplitAccountDuplicate,
				fmt.Errorf("tiptoppay: split %d repeats an account: %w", i, payment.ErrProviderMalformed))
		}
		seen[ref] = struct{}{}

		minor, err := payment.ParseMinor(e.Amount.String())
		if err != nil {
			return nil, fmt.Errorf("tiptoppay: split %d amount: %w", i, payment.ErrProviderMalformed)
		}
		if minor <= 0 {
			return nil, domain.WithCode(domain.CodeSplitShareInvalid,
				fmt.Errorf("tiptoppay: split %d came back as %d minor units: %w", i, minor, payment.ErrProviderMalformed))
		}
		out[i] = minor
	}
	return out, nil
}

// scaleSplitEntries re-divides echoed shares for a SMALLER total — a partial
// confirm or a partial refund — so that the parts still add up to targetMinor
// exactly (domain.DistributeProportionally, largest-remainder).
//
// Shares that scale to zero are dropped rather than sent as zero. At
// /payments/confirm that is the documented meaning of an absent PublicId ("...
// приведет к отмене такого сплит платежа на всю его сумму"), which is exactly
// right for a recipient whose share of the confirmed amount is nothing; at
// /payments/refund an absent PublicId simply returns nothing from that account.
func scaleSplitEntries(entries []splitEntry, targetMinor int64) ([]splitEntry, error) {
	weights, err := parseSplitEntries(entries)
	if err != nil {
		return nil, err
	}
	if len(weights) == 0 {
		return nil, nil
	}

	var total int64
	for _, w := range weights {
		total += w
	}
	if targetMinor <= 0 {
		return nil, domain.WithCode(domain.CodeSplitShareInvalid,
			fmt.Errorf("tiptoppay: split target %d must be positive: %w", targetMinor, domain.ErrValidation))
	}
	if targetMinor > total {
		return nil, domain.WithCode(domain.CodeSplitAmountTooBig,
			fmt.Errorf("tiptoppay: %s exceeds what the splits are worth: %w",
				payment.FormatMinor(targetMinor), domain.ErrValidation))
	}
	if targetMinor == total {
		out := make([]splitEntry, len(entries))
		copy(out, entries)
		for i := range out {
			out[i].Amount = json.Number(payment.FormatMinor(weights[i]))
			out[i].JSONData = nil
		}
		return out, nil
	}

	parts, err := domain.DistributeProportionally(weights, targetMinor)
	if err != nil {
		return nil, err
	}
	out := make([]splitEntry, 0, len(entries))
	for i, part := range parts {
		if part == 0 {
			continue
		}
		out = append(out, splitEntry{
			PublicID: strings.TrimSpace(entries[i].PublicID),
			Amount:   json.Number(payment.FormatMinor(part)),
		})
	}
	if len(out) == 0 {
		return nil, domain.WithCode(domain.CodeSplitShareInvalid,
			fmt.Errorf("tiptoppay: %s leaves every split share at zero: %w",
				payment.FormatMinor(targetMinor), domain.ErrValidation))
	}
	return out, nil
}

// originalSplits reads back the division a payment was created with.
//
// This is deliberately NOT a second local store of the split. The acquirer is
// the authority on what it was told; a copy of ours could drift from it (a
// half-written row, a payment created by an earlier build, a share the acquirer
// itself adjusted), and a confirm sent against a drifted copy silently cancels
// somebody's share — that is the documented consequence of an omitted PublicId.
// One extra read before a money movement is a cheap price for that.
//
// It returns nil (and no error) for a payment that has no splits, which is what
// makes Capture / Refund behave exactly as before for every non-split payment.
func (g *Gateway) originalSplits(ctx context.Context, txID int64) ([]splitEntry, error) {
	var model transactionModel
	if _, err := g.call(ctx, "payments/get", "/payments/get", "", getRequest{TransactionID: txID}, &model); err != nil {
		return nil, err
	}
	return model.Splits, nil
}

// splitsFor builds the Splits array for a confirm or a refund of amount on a
// payment that may or may not be split.
//
// Full amount → the original shares verbatim (the documented way to confirm or
// fully refund a split). Partial amount → the shares scaled proportionally,
// which is the part the documentation does NOT specify: it only says each share
// must not exceed what was charged for it. Proportional is the reading that
// keeps the platform's commission the same PERCENTAGE of the money that
// actually moved — the alternative readings (take the reduction out of the
// venue's share first, or out of ours first) both silently change the
// commercial deal on every partial capture.
//
// TODO(verify): проверить на песочнице — какое распределение TipTop Pay/бизнес
// ожидает при ЧАСТИЧНОМ подтверждении и частичном возврате сплита: пропорционально
// всем долям (как здесь) или сначала уменьшается доля заведения.
func (g *Gateway) splitsFor(ctx context.Context, txID int64, amount domain.Money) ([]splitEntry, error) {
	if !g.cfg.SplitsEnabled {
		return nil, nil
	}
	entries, err := g.originalSplits(ctx, txID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return scaleSplitEntries(entries, amount.AmountMinor)
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// splitErrorCodes maps TipTopPay's documented split refusals (section
// «Ошибки») onto our stable error codes. Matching is on a lowercased SUBSTRING
// because two of the messages carry a variable tail ("SubMerchants is disabled:
// <ids>") and one is a truncated Russian sentence in the documentation itself.
//
// The message text is the only discriminator TipTopPay gives us: ErrorCode is
// null for all of these but one (6018). Matching on remote text is normally
// forbidden here — see postgres/payment/errors.go, which refuses to read a
// driver's message — but the alternative is a single opaque "rejected" for
// nine different operator actions, and unlike a money-safety decision this only
// chooses which sentence an operator reads. A message TipTopPay rewords stops
// being recognised and falls back to the generic rejection; it never changes
// what happens to the payment.
var splitErrorCodes = []struct {
	needle string
	code   domain.ErrorCode
}{
	{"split transaction is not supported on the acquirer side", domain.CodeSplitNotSupported},
	{"split transaction is not supported", domain.CodeSplitNotSupported},
	{"subscription is not supported for splits", domain.CodeSplitNotSupported},
	{"submerchant not found", domain.CodeSplitSubMerchantUnknown},
	{"submerchants is disabled", domain.CodeSplitSubMerchantUnknown},
	{"terminals modes not equal", domain.CodeSplitTerminalMismatch},
	{"issplitsubmerchant", domain.CodeSplitTerminalMismatch},
	{"amount is not equal to request amount", domain.CodeSplitSumMismatch},
	{"duplicated publicids for splits", domain.CodeSplitAccountDuplicate},
	{`field "splits" is required`, domain.CodeSplitRequired},
	{"amount is too big", domain.CodeSplitAmountTooBig},
}

// splitErrorCode reports the error code a TipTopPay rejection message maps to.
func splitErrorCode(message string) (domain.ErrorCode, bool) {
	m := strings.ToLower(strings.TrimSpace(message))
	if m == "" {
		return "", false
	}
	for _, c := range splitErrorCodes {
		if strings.Contains(m, c.needle) {
			return c.code, true
		}
	}
	return "", false
}

// authorizeSplits turns the domain plan on an AuthorizeRequest into the wire
// array, refusing — loudly, before any HTTP call — every configuration in which
// sending it would move money to the wrong account.
//
// Both refusals wrap domain.ErrUnavailable rather than ErrValidation: the
// caller's request is fine, it is this deployment that is not ready, and the
// honest answer to the guest is "not right now", not "you did something wrong".
func (g *Gateway) authorizeSplits(req domain.AuthorizeRequest) ([]splitEntry, error) {
	if req.Splits.IsZero() {
		return nil, nil
	}
	if !g.cfg.SplitsEnabled {
		return nil, domain.WithCode(domain.CodeSplitNotSupported, fmt.Errorf(
			"tiptoppay: this terminal is not set up for split payments (TIPTOPPAY_SPLITS_ENABLED is off): %w",
			domain.ErrUnavailable))
	}
	if !g.cfg.SplitViaOrders {
		return nil, domain.WithCode(domain.CodeSplitFlowUnsupported, fmt.Errorf(
			"tiptoppay: splits over the hosted order flow (/orders/create) are not documented by the provider "+
				"and are disabled until verified on the sandbox — see docs/payments/tiptoppay-splits.md: %w",
			domain.ErrUnavailable))
	}
	return toSplitEntries(req.Splits, req.Amount)
}

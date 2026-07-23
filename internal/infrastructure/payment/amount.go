package payment

import (
	"fmt"
	"strconv"
	"strings"

	"backend-core/internal/domain"
)

// FormatMinor renders an amount in minor units as the decimal string both
// acquirers expect ("10350" tiyn → "103.50").
//
// It is integer arithmetic on purpose. Turning money into a float64 to print it
// is how 103.50 becomes 103.49999999999999 in somebody's refund.
func FormatMinor(minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	s := fmt.Sprintf("%d.%02d", minor/100, minor%100)
	if neg {
		return "-" + s
	}
	return s
}

// ParseMinor is the inverse of FormatMinor: it reads an acquirer's decimal
// amount back into minor units without ever touching a float.
//
// It accepts "103", "103.5", "103.50" and "103,50" (some FreedomPay-family
// responses use a comma). More than two decimals is rejected rather than
// rounded — silently dropping a fraction of somebody's money is not our call.
func ParseMinor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount: %w", domain.ErrValidation)
	}
	s = strings.Replace(s, ",", ".", 1)

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, domain.ErrValidation)
	}

	var cents int64
	if hasFrac {
		switch len(frac) {
		case 0:
		case 1:
			frac += "0"
			fallthrough
		case 2:
			cents, err = strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("amount %q: %w", s, domain.ErrValidation)
			}
		default:
			// Trailing zeros beyond two decimals are harmless; real precision
			// we cannot represent is not.
			if strings.Trim(frac[2:], "0") != "" {
				return 0, fmt.Errorf("amount %q has more precision than the currency: %w", s, domain.ErrValidation)
			}
			cents, err = strconv.ParseInt(frac[:2], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("amount %q: %w", s, domain.ErrValidation)
			}
		}
	}

	total := units*100 + cents
	if neg {
		total = -total
	}
	return total, nil
}

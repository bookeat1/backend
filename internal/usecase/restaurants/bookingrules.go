package restaurants

import (
	"math"
	"strings"

	"backend-core/internal/domain"
)

// BookingRulesDefaults are the platform-wide fallbacks for a venue's optional
// guest-facing booking-rules copy (Trello BNjLdfSP): shown at booking
// confirmation and in the pre-visit reminder when the venue has not set its
// own. Populated from bootstrap.Config.Booking; see ResolveBookingRules.
type BookingRulesDefaults struct {
	// HoldMinutes is the platform default for "how long is the table held".
	// env: BOOKING_DEFAULT_HOLD_MINUTES (15).
	HoldMinutes int
	// LateArrivalText is the platform default "running late" note. env:
	// BOOKING_DEFAULT_LATE_ARRIVAL_TEXT. Russian only — the platform default
	// carries no translations of its own; a venue that wants the note in
	// another language sets its own text (with LateArrivalTextI18n).
	LateArrivalText string
	// DefaultFreeCancelWindowMinutes mirrors PAYMENTS_FREE_CANCEL_WINDOW_MINUTES
	// (usecase/payments.Config.FreeCancelWindow) for the rare venue whose
	// Restaurant.FreeCancelWindowMinutes was not loaded (a nil pointer — see
	// its doc comment). Every real row has a concrete value (the column is
	// NOT NULL), so this only guards a caller that forgot to read the column.
	DefaultFreeCancelWindowMinutes int
}

// ResolveBookingRules merges a venue's optional booking-rules overrides with
// the platform defaults — the read-modify-write shape of
// usecase/bookings.resolvePolicy: an override that is present but nonsensical
// (a non-positive hold, a blank text) is ignored rather than trusted, so a
// stray value can never surface as "the table is held for 0 minutes".
//
// FreeCancelHours is NOT an independent override — see
// domain.Restaurant.FreeCancelWindowMinutes for why: it is rounded from the
// SAME column usecase/payments enforces the deposit deadline from, so the
// guest-facing copy and the money path can never say two different things.
//
// lang resolves LateArrivalText through the venue's own translations
// (domain.I18n.Resolve); the platform default text is not localized.
func ResolveBookingRules(r domain.Restaurant, d BookingRulesDefaults, lang string) domain.EffectiveBookingRules {
	hold := d.HoldMinutes
	if v := r.BookingRules.HoldMinutes; v != nil && *v > 0 {
		hold = *v
	}

	freeCancelMinutes := d.DefaultFreeCancelWindowMinutes
	if v := r.FreeCancelWindowMinutes; v != nil && *v >= 0 {
		freeCancelMinutes = *v
	}

	text := d.LateArrivalText
	if v := r.BookingRules.LateArrivalText; v != nil && strings.TrimSpace(*v) != "" {
		text = r.BookingRules.LateArrivalTextI18n.Resolve(lang, *v)
	}

	return domain.EffectiveBookingRules{
		HoldMinutes:     hold,
		FreeCancelHours: int(math.Round(float64(freeCancelMinutes) / 60.0)),
		LateArrivalText: text,
	}
}

package bookings

import (
	"time"

	"backend-core/internal/domain"
)

// CancelDeadlineFor is the server-trusted "free cancellation ends at" moment
// for a booking derived from cancel_deadline_minutes.
//
// DEPRECATED / SUPERSEDED (owner decision, single-window consolidation): the
// money-path free-cancellation deadline now comes from
// restaurants.free_cancel_window_minutes via
// usecase/payments.FreeCancelDeadlineFor, and the guest self-cancel action is
// no longer time-gated at all (see status.go authorizeTransition). This helper
// and the cancel_deadline_minutes column it reads are retained only so existing
// callers/DTOs keep compiling and no prod migration drops a live column near
// launch; nothing in the cancel/money path relies on it anymore.
func CancelDeadlineFor(restaurant domain.Restaurant, cfg Config, startsAt time.Time) time.Time {
	policy := resolvePolicy(restaurant, cfg)
	return startsAt.Add(-policy.CancelDeadline)
}

// VenueTimezone returns the IANA timezone this venue's calendar maths runs in:
// its own restaurants.timezone override when it is set AND loadable on this
// host, otherwise the platform fallback (BOOKING_TIMEZONE_FALLBACK, default
// Asia/Almaty). It is exported so the public catalog can compute "open now" in
// the venue's own zone through the SAME resolution the availability engine
// uses, instead of growing a second source of truth (bound to
// usecase/restaurants.VenueTimezoneResolver in bootstrap/deps.go).
func (c Config) VenueTimezone(r domain.Restaurant) string {
	return resolvePolicy(r, c).Timezone
}

// VenueCapacity returns the venue's resolved availability mode and, in seats
// mode, the number of guests it can seat at once (zero in table mode).
//
// Exported for the same reason as VenueTimezone: the public catalog has to
// answer "can a guest book here at all", and in seats mode the answer has
// nothing to do with the table list — a table-less venue is bookable with zero
// tables. Routing that question through resolvePolicy keeps the catalog's
// answer and the availability engine's behaviour derived from ONE rule,
// including its validation (a seats mode with a missing or out-of-range seat
// count degrades to table mode here exactly as it does for a booking).
func (c Config) VenueCapacity(r domain.Restaurant) (domain.CapacityMode, int) {
	p := resolvePolicy(r, c)
	return p.CapacityMode, p.CapacitySeats
}

// resolvePolicy merges the two policy levels of spec §4.2: the global env
// defaults (Config) and the restaurant's optional per-field overrides. A NULL
// (nil) override falls back to the global value; an override that is present
// but nonsensical (non-positive duration, negative buffer, unknown timezone)
// is ignored rather than trusted — the DB columns are plain nullable integers
// with no CHECK behind them.
func resolvePolicy(r domain.Restaurant, cfg Config) domain.BookingPolicy {
	cfg = cfg.withDefaults()
	o := r.BookingPolicy

	p := domain.BookingPolicy{
		Timezone:            cfg.TimezoneFallback,
		Duration:            cfg.DefaultDuration,
		Buffer:              cfg.DefaultBuffer,
		Lead:                cfg.DefaultLead,
		HorizonDays:         cfg.DefaultHorizonDays,
		CancelDeadline:      cfg.DefaultCancelDeadline,
		ConfirmSLA:          cfg.DefaultConfirmSLA,
		MaxGuestsPerBooking: cfg.DefaultMaxGuests,
		AutoConfirm:         cfg.DefaultAutoConfirm,
		// DEFAULT DECISION (owner, 25.07.2026): table mode. A NULL
		// booking_capacity_mode — which is every venue that existed before
		// migration 0054 — resolves to exactly the behaviour it had yesterday.
		// Nobody is switched silently: a venue with tables keeps its tables,
		// and a venue without tables stays as unbookable as it was until
		// someone deliberately declares a capacity through the admin API. The
		// alternative (defaulting the tableless venues to capacity mode) would
		// have required the platform to INVENT a seat count for a venue it has
		// never measured, and a made-up capacity is exactly the kind of number
		// that turns into a double-booked Saturday.
		CapacityMode: domain.CapacityModeTables,
	}

	if o.Timezone != nil && *o.Timezone != "" {
		if _, err := time.LoadLocation(*o.Timezone); err == nil {
			p.Timezone = *o.Timezone
		}
	}
	if v := o.BookingDurationMinutes; v != nil && *v > 0 {
		p.Duration = time.Duration(*v) * time.Minute
	}
	if v := o.BookingBufferMinutes; v != nil && *v >= 0 {
		p.Buffer = time.Duration(*v) * time.Minute
	}
	if v := o.BookingLeadMinutes; v != nil && *v >= 0 {
		p.Lead = time.Duration(*v) * time.Minute
	}
	if v := o.BookingHorizonDays; v != nil && *v > 0 {
		p.HorizonDays = *v
	}
	if v := o.CancelDeadlineMinutes; v != nil && *v >= 0 {
		p.CancelDeadline = time.Duration(*v) * time.Minute
	}
	if v := o.ConfirmSLAMinutes; v != nil && *v > 0 {
		p.ConfirmSLA = time.Duration(*v) * time.Minute
	}
	if v := o.MaxGuestsPerBooking; v != nil && *v > 0 {
		p.MaxGuestsPerBooking = *v
	}
	if o.AutoConfirm != nil {
		p.AutoConfirm = *o.AutoConfirm
	}
	// Capacity mode is only honoured together with a usable seat count: a row
	// claiming 'seats' with a NULL/absurd capacity would make the venue silently
	// unbookable, which is the very failure this feature exists to remove. The
	// DB CHECKs of 0054 make that pair impossible going forward; this guard
	// covers rows written before them (or by hand).
	if v := o.BookingCapacityMode; v != nil && *v == domain.CapacityModeSeats {
		if s := o.BookingCapacitySeats; s != nil && *s > 0 && *s <= maxCapacitySeats {
			p.CapacityMode = domain.CapacityModeSeats
			p.CapacitySeats = *s
		}
	}
	return p
}

// policyLocation returns the venue's location, falling back to UTC when the
// stored IANA name is unknown on this host (missing tzdata). Calendar maths
// must never panic on a bad DB value.
func policyLocation(p domain.BookingPolicy) *time.Location {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

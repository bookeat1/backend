package bookings

import (
	"time"

	"backend-core/internal/domain"
)

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

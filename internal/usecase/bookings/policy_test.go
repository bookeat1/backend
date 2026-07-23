package bookings

import (
	"testing"
	"time"

	"backend-core/internal/domain"
)

func iptr(v int) *int       { return &v }
func bptr(v bool) *bool     { return &v }
func sptr(v string) *string { return &v }

func testConfig() Config {
	return Config{
		DefaultDuration:       120 * time.Minute,
		DefaultBuffer:         15 * time.Minute,
		DefaultLead:           60 * time.Minute,
		DefaultHorizonDays:    60,
		DefaultCancelDeadline: 180 * time.Minute,
		DefaultConfirmSLA:     120 * time.Minute,
		DefaultMaxGuests:      20,
		DefaultAutoConfirm:    true,
		TimezoneFallback:      "Asia/Almaty",
		RateWindow:            time.Hour,
		RateLimit:             10,
		SlotStep:              30 * time.Minute,
	}
}

// Table over every override field: NULL falls back to the env value, a valid
// value wins, a nonsensical value is ignored.
func TestResolvePolicy(t *testing.T) {
	cfg := testConfig()

	cases := []struct {
		name     string
		override domain.BookingPolicyOverride
		want     domain.BookingPolicy
	}{
		{
			name: "all NULL falls back to env",
			want: domain.BookingPolicy{
				Timezone: "Asia/Almaty", Duration: 120 * time.Minute, Buffer: 15 * time.Minute,
				Lead: 60 * time.Minute, HorizonDays: 60, CancelDeadline: 180 * time.Minute,
				ConfirmSLA: 120 * time.Minute, MaxGuestsPerBooking: 20, AutoConfirm: true,
			},
		},
		{
			name: "all overridden",
			override: domain.BookingPolicyOverride{
				Timezone: sptr("UTC"), BookingDurationMinutes: iptr(90), BookingBufferMinutes: iptr(0),
				BookingLeadMinutes: iptr(15), BookingHorizonDays: iptr(7), CancelDeadlineMinutes: iptr(30),
				ConfirmSLAMinutes: iptr(45), MaxGuestsPerBooking: iptr(8), AutoConfirm: bptr(false),
			},
			want: domain.BookingPolicy{
				Timezone: "UTC", Duration: 90 * time.Minute, Buffer: 0,
				Lead: 15 * time.Minute, HorizonDays: 7, CancelDeadline: 30 * time.Minute,
				ConfirmSLA: 45 * time.Minute, MaxGuestsPerBooking: 8, AutoConfirm: false,
			},
		},
		{
			name:     "auto_confirm disabled only",
			override: domain.BookingPolicyOverride{AutoConfirm: bptr(false)},
			want: domain.BookingPolicy{
				Timezone: "Asia/Almaty", Duration: 120 * time.Minute, Buffer: 15 * time.Minute,
				Lead: 60 * time.Minute, HorizonDays: 60, CancelDeadline: 180 * time.Minute,
				ConfirmSLA: 120 * time.Minute, MaxGuestsPerBooking: 20, AutoConfirm: false,
			},
		},
		{
			name:     "zero buffer and zero lead are meaningful overrides",
			override: domain.BookingPolicyOverride{BookingBufferMinutes: iptr(0), BookingLeadMinutes: iptr(0)},
			want: domain.BookingPolicy{
				Timezone: "Asia/Almaty", Duration: 120 * time.Minute, Buffer: 0,
				Lead: 0, HorizonDays: 60, CancelDeadline: 180 * time.Minute,
				ConfirmSLA: 120 * time.Minute, MaxGuestsPerBooking: 20, AutoConfirm: true,
			},
		},
		{
			name: "nonsensical overrides are ignored",
			override: domain.BookingPolicyOverride{
				Timezone: sptr("Mars/Olympus"), BookingDurationMinutes: iptr(0),
				BookingBufferMinutes: iptr(-5), BookingLeadMinutes: iptr(-1),
				BookingHorizonDays: iptr(0), CancelDeadlineMinutes: iptr(-10),
				ConfirmSLAMinutes: iptr(-1), MaxGuestsPerBooking: iptr(0),
			},
			want: domain.BookingPolicy{
				Timezone: "Asia/Almaty", Duration: 120 * time.Minute, Buffer: 15 * time.Minute,
				Lead: 60 * time.Minute, HorizonDays: 60, CancelDeadline: 180 * time.Minute,
				ConfirmSLA: 120 * time.Minute, MaxGuestsPerBooking: 20, AutoConfirm: true,
			},
		},
		{
			name:     "empty timezone string falls back",
			override: domain.BookingPolicyOverride{Timezone: sptr("")},
			want: domain.BookingPolicy{
				Timezone: "Asia/Almaty", Duration: 120 * time.Minute, Buffer: 15 * time.Minute,
				Lead: 60 * time.Minute, HorizonDays: 60, CancelDeadline: 180 * time.Minute,
				ConfirmSLA: 120 * time.Minute, MaxGuestsPerBooking: 20, AutoConfirm: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePolicy(domain.Restaurant{BookingPolicy: tc.override}, cfg)
			if got != tc.want {
				t.Fatalf("resolvePolicy() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An empty Config (nothing wired) must still produce a usable policy.
func TestResolvePolicyZeroConfig(t *testing.T) {
	p := resolvePolicy(domain.Restaurant{}, Config{})
	if p.Duration != defaultDuration || p.HorizonDays != defaultHorizonDays ||
		p.MaxGuestsPerBooking != defaultMaxGuests || p.Timezone != defaultTimezone {
		t.Fatalf("zero config policy = %+v", p)
	}
}

func TestPolicyLocationFallsBackToUTC(t *testing.T) {
	if loc := policyLocation(domain.BookingPolicy{Timezone: "Mars/Olympus"}); loc != time.UTC {
		t.Fatalf("policyLocation(bad) = %v, want UTC", loc)
	}
	if loc := policyLocation(domain.BookingPolicy{Timezone: "Asia/Almaty"}); loc.String() != "Asia/Almaty" {
		t.Fatalf("policyLocation(Asia/Almaty) = %v", loc)
	}
}

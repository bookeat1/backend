package bookings

import (
	"testing"

	"backend-core/internal/domain"
)

// The whole point of splitting the flag: instant confirmation and the
// after-SLA safety net are separate decisions, and the combination venues
// actually want ("let me answer, but do not leave the guest hanging") has to be
// expressible. With one field it was not.
func TestConfirmOnCreateAndAutoConfirmResolveIndependently(t *testing.T) {
	cfg := Config{
		DefaultAutoConfirm:     true,
		DefaultConfirmOnCreate: false,
		TimezoneFallback:       "Asia/Almaty",
	}
	yes, no := true, false

	cases := []struct {
		name            string
		override        domain.BookingPolicyOverride
		wantOnCreate    bool
		wantAutoConfirm bool
	}{
		{
			name:            "no override: the venue answers, the SLA worker is the safety net",
			override:        domain.BookingPolicyOverride{},
			wantOnCreate:    false,
			wantAutoConfirm: true,
		},
		{
			name:            "venue wants instant confirmation",
			override:        domain.BookingPolicyOverride{ConfirmOnCreate: &yes},
			wantOnCreate:    true,
			wantAutoConfirm: true,
		},
		{
			name:            "venue keeps the decision even after the SLA",
			override:        domain.BookingPolicyOverride{AutoConfirm: &no},
			wantOnCreate:    false,
			wantAutoConfirm: false,
		},
		{
			name:            "instant on, safety net off — still expressible",
			override:        domain.BookingPolicyOverride{ConfirmOnCreate: &yes, AutoConfirm: &no},
			wantOnCreate:    true,
			wantAutoConfirm: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resolvePolicy(domain.Restaurant{BookingPolicy: tc.override}, cfg)
			if p.ConfirmOnCreate != tc.wantOnCreate {
				t.Errorf("ConfirmOnCreate = %v, want %v", p.ConfirmOnCreate, tc.wantOnCreate)
			}
			if p.AutoConfirm != tc.wantAutoConfirm {
				t.Errorf("AutoConfirm = %v, want %v", p.AutoConfirm, tc.wantAutoConfirm)
			}
		})
	}
}

// A regression guard for the reason this change exists: turning instant
// confirmation off must NOT turn the safety net off with it.
func TestTurningOffInstantConfirmationKeepsTheSafetyNet(t *testing.T) {
	cfg := Config{DefaultAutoConfirm: true, DefaultConfirmOnCreate: false, TimezoneFallback: "Asia/Almaty"}
	p := resolvePolicy(domain.Restaurant{}, cfg)

	if p.ConfirmOnCreate {
		t.Fatal("a new booking must arrive as a request the venue can answer")
	}
	if !p.AutoConfirm {
		t.Fatal("a venue that never answers must not leave the guest pending forever")
	}
}

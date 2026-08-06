package pos

import (
	"context"
	"errors"
	"testing"

	"backend-core/internal/domain"
)

// ---------------------------------------------------------------------------
// hand-written fake (project convention: no mock framework)
// ---------------------------------------------------------------------------

type fakeConnector struct{ name domain.POSProvider }

func (f fakeConnector) PushBooking(context.Context, domain.POSBookingRequest) (*domain.POSBookingRef, error) {
	return nil, nil
}
func (f fakeConnector) CancelBooking(context.Context, string) error { return nil }
func (f fakeConnector) FetchOccupancy(context.Context, domain.POSOccupancyQuery) ([]domain.POSTableOccupancy, error) {
	return nil, nil
}
func (f fakeConnector) VerifyWebhook([]byte, map[string]string) (*domain.POSEvent, error) {
	return nil, nil
}
func (f fakeConnector) Name() domain.POSProvider { return f.name }

// ---------------------------------------------------------------------------

func TestNewRegistryRejectsDuplicateNilAndUnknownConnectors(t *testing.T) {
	tests := []struct {
		name       string
		connectors []domain.POSConnector
		wantErr    error // errors.Is target; nil means "any non-nil error"
	}{
		{
			name:       "duplicate connector",
			connectors: []domain.POSConnector{fakeConnector{domain.POSIiko}, fakeConnector{domain.POSIiko}},
		},
		{
			name:       "unknown provider code",
			connectors: []domain.POSConnector{fakeConnector{"square"}},
			wantErr:    ErrProviderUnknown,
		},
		{
			name:       "nil connector",
			connectors: []domain.POSConnector{nil},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRegistry(tc.connectors...)
			if err == nil {
				t.Fatalf("NewRegistry(%s) = nil error, want an error", tc.name)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryFor(t *testing.T) {
	// iiko is wired; the other three are known codes but left unwired (as they
	// are in this scaffold), and "square" is an unknown code.
	r, err := NewRegistry(fakeConnector{domain.POSIiko})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tests := []struct {
		name     string
		provider domain.POSProvider
		wantErr  error // nil means "expect success"
	}{
		{name: "wired provider", provider: domain.POSIiko, wantErr: nil},
		{name: "known but unwired: not configured", provider: domain.POSRKeeper, wantErr: ErrProviderNotConfigured},
		{name: "known but unwired: not configured (poster)", provider: domain.POSPoster, wantErr: ErrProviderNotConfigured},
		{name: "unknown code: unknown provider", provider: "square", wantErr: ErrProviderUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := r.For(tc.provider)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("For(%s): %v", tc.provider, err)
				}
				if c.Name() != tc.provider {
					t.Errorf("got connector %s, want %s", c.Name(), tc.provider)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("For(%s) err = %v, want %v", tc.provider, err, tc.wantErr)
			}
		})
	}
}

func TestRegistryForSentinelMapping(t *testing.T) {
	r, _ := NewRegistry(fakeConnector{domain.POSIiko})

	if _, err := r.For("square"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unknown code err = %v, want it to wrap domain.ErrValidation", err)
	}
	if _, err := r.For(domain.POSKwaaka); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unwired code err = %v, want it to wrap domain.ErrNotFound", err)
	}
}

func TestRegistryConfigured(t *testing.T) {
	r, _ := NewRegistry(fakeConnector{domain.POSIiko})
	if !r.Configured(domain.POSIiko) {
		t.Error("iiko should be configured")
	}
	if r.Configured(domain.POSRKeeper) {
		t.Error("rkeeper should not be configured")
	}
}

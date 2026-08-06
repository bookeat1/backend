// Package kwaaka is a TEMPLATE adapter for Kwaaka (https://kwaaka.com) behind
// domain.POSConnector.
//
// # Why Kwaaka is just another adapter
//
// Kwaaka is an AGGREGATOR: it itself fronts iiko, r_keeper and Poster (among
// others) behind one API. From BookEat's side that fan-out is Kwaaka's concern,
// not ours — so Kwaaka is modelled as one more POSConnector adapter with no
// special casing. A venue is bound to EITHER a direct POS adapter (iiko/…) OR
// the Kwaaka adapter, never both; which one is the restaurant→POS binding's
// job, a follow-up (see the pos package doc).
//
// # What is confirmed
//
// Nothing. There is no Kwaaka test access, no credentials and no pinned API
// contract yet. This package exists so the port has a Kwaaka implementation the
// moment those land: every method compiles, satisfies domain.POSConnector, and
// returns ErrNotWired instead of guessing a protocol — the same honesty
// discipline as infrastructure/payment/partnerspay and the iiko adapter.
//
// # What is NOT known and must come before this adapter does anything real
//
//   - Kwaaka's auth scheme and how the underlying POS is selected per venue;
//   - the reservation-create request/response shape and status vocabulary;
//   - whether table occupancy is polled or only pushed, and in what shape
//     (decides FetchOccupancy);
//   - the webhook transport, event set and signature scheme — until known,
//     VerifyWebhook must NOT interpret a payload;
//   - idempotency of reservation creation, for PushBooking's IdempotencyKey.
//
// When credentials arrive this package gains a config.go modelled on the iiko
// adapter's; it has none today because there is nothing to configure yet.
package kwaaka

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ErrNotWired is returned by every operation below. It wraps
// domain.ErrNotImplemented — a plain, non-money-moving failure while Kwaaka's
// contract and credentials are pending, not a panic-worthy bug.
var ErrNotWired = fmt.Errorf(
	"kwaaka: adapter is a scaffold, not implemented until the POS API contract and credentials are wired: %w",
	domain.ErrNotImplemented,
)

// Connector is the Kwaaka implementation of domain.POSConnector.
type Connector struct {
	log *slog.Logger
}

var _ domain.POSConnector = (*Connector)(nil)

// New builds the adapter. It takes no config yet: Kwaaka has nothing to
// configure until an API contract and credentials exist (see the package doc).
func New(log *slog.Logger) *Connector {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Connector{log: log}
}

// Name reports the provider code this adapter serves.
func (c *Connector) Name() domain.POSProvider { return domain.POSKwaaka }

// PushBooking is not implemented — see the package doc for what is missing.
func (c *Connector) PushBooking(ctx context.Context, req domain.POSBookingRequest) (*domain.POSBookingRef, error) {
	if req.BookingID == uuid.Nil {
		return nil, fmt.Errorf("kwaaka: push without a booking id: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("kwaaka: PushBooking: %w", ErrNotWired)
}

// CancelBooking is not implemented — see the package doc for what is missing.
func (c *Connector) CancelBooking(ctx context.Context, providerBookingID string) error {
	if strings.TrimSpace(providerBookingID) == "" {
		return fmt.Errorf("kwaaka: empty provider booking id: %w", domain.ErrValidation)
	}
	return fmt.Errorf("kwaaka: CancelBooking: %w", ErrNotWired)
}

// FetchOccupancy is not implemented — see the package doc for what is missing.
func (c *Connector) FetchOccupancy(ctx context.Context, q domain.POSOccupancyQuery) ([]domain.POSTableOccupancy, error) {
	if q.RestaurantID == uuid.Nil {
		return nil, fmt.Errorf("kwaaka: occupancy query without a restaurant id: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("kwaaka: FetchOccupancy: %w", ErrNotWired)
}

// VerifyWebhook is not implemented. Whatever the real scheme turns out to be,
// signature verification comes before interpreting the payload; until the
// scheme is known nothing is interpreted at all.
func (c *Connector) VerifyWebhook(raw []byte, headers map[string]string) (*domain.POSEvent, error) {
	return nil, fmt.Errorf("kwaaka: VerifyWebhook: %w", ErrNotWired)
}

// Package poster is a TEMPLATE adapter for the Poster point-of-sale system
// (https://joinposter.com) behind domain.POSConnector.
//
// # What is confirmed
//
// Nothing. There is no Poster test access, no credentials and no pinned API
// contract yet. This package exists so the port has a Poster implementation the
// moment those land: every method compiles, satisfies domain.POSConnector, and
// returns ErrNotWired instead of guessing a protocol — the same honesty
// discipline as infrastructure/payment/partnerspay and the iiko adapter.
//
// # What is NOT known and must come before this adapter does anything real
//
//   - Poster's auth (its public REST API uses an application access token /
//     OAuth per account — the exact flow BookEat uses is unconfirmed);
//   - whether Poster even models table reservations the way BookEat needs, and
//     the reservation-create request/response shape and status vocabulary;
//   - whether table occupancy is polled or only pushed, and in what shape
//     (decides FetchOccupancy);
//   - the webhook transport, event set and signature scheme — until known,
//     VerifyWebhook must NOT interpret a payload;
//   - idempotency of reservation creation, for PushBooking's IdempotencyKey.
//
// When credentials arrive this package gains a config.go modelled on the iiko
// adapter's; it has none today because there is nothing to configure yet.
package poster

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ErrNotWired is returned by every operation below. It wraps
// domain.ErrNotImplemented — a plain, non-money-moving failure while Poster's
// contract and credentials are pending, not a panic-worthy bug.
var ErrNotWired = fmt.Errorf(
	"poster: adapter is a scaffold, not implemented until the POS API contract and credentials are wired: %w",
	domain.ErrNotImplemented,
)

// Connector is the Poster implementation of domain.POSConnector.
type Connector struct {
	log *slog.Logger
}

var _ domain.POSConnector = (*Connector)(nil)

// New builds the adapter. It takes no config yet: Poster has nothing to
// configure until an API contract and credentials exist (see the package doc).
func New(log *slog.Logger) *Connector {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Connector{log: log}
}

// Name reports the provider code this adapter serves.
func (c *Connector) Name() domain.POSProvider { return domain.POSPoster }

// PushBooking is not implemented — see the package doc for what is missing.
func (c *Connector) PushBooking(ctx context.Context, req domain.POSBookingRequest) (*domain.POSBookingRef, error) {
	if req.BookingID == uuid.Nil {
		return nil, fmt.Errorf("poster: push without a booking id: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("poster: PushBooking: %w", ErrNotWired)
}

// CancelBooking is not implemented — see the package doc for what is missing.
func (c *Connector) CancelBooking(ctx context.Context, providerBookingID string) error {
	if strings.TrimSpace(providerBookingID) == "" {
		return fmt.Errorf("poster: empty provider booking id: %w", domain.ErrValidation)
	}
	return fmt.Errorf("poster: CancelBooking: %w", ErrNotWired)
}

// FetchOccupancy is not implemented — see the package doc for what is missing.
func (c *Connector) FetchOccupancy(ctx context.Context, q domain.POSOccupancyQuery) ([]domain.POSTableOccupancy, error) {
	if q.RestaurantID == uuid.Nil {
		return nil, fmt.Errorf("poster: occupancy query without a restaurant id: %w", domain.ErrValidation)
	}
	return nil, fmt.Errorf("poster: FetchOccupancy: %w", ErrNotWired)
}

// VerifyWebhook is not implemented. Whatever the real scheme turns out to be,
// signature verification comes before interpreting the payload; until the
// scheme is known nothing is interpreted at all.
func (c *Connector) VerifyWebhook(raw []byte, headers map[string]string) (*domain.POSEvent, error) {
	return nil, fmt.Errorf("poster: VerifyWebhook: %w", ErrNotWired)
}

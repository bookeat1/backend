package iiko

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// ErrNotWired is returned by every operation below. It wraps
// domain.ErrNotImplemented: the transport layer and usecases already treat
// "not implemented" as a plain, non-money-moving failure, which is exactly the
// caller-visible situation while iiko's credentials and contract are pending.
// An admin trying to route a booking through iiko before it is finished is an
// expected, ordinary mistake to guard against, not a panic-worthy bug.
var ErrNotWired = fmt.Errorf(
	"iiko: adapter is a scaffold, not implemented until the POS API contract and credentials are wired: %w",
	domain.ErrNotImplemented,
)

// Connector is the iiko implementation of domain.POSConnector.
//
// It carries the config so that filling in the real iikoTransport calls later
// is a matter of writing method bodies, not redesigning the type. cfg is unused
// by every method today — TODO(verify): once PushBooking is implemented for
// real, it will be the first to actually use cfg.APILogin / cfg.OrganizationID
// against cfg.BaseURL.
type Connector struct {
	cfg Config
	log *slog.Logger
}

var _ domain.POSConnector = (*Connector)(nil)

// New builds the adapter. It validates the config so a half-credential wiring
// fails at construction rather than on the first call.
func New(cfg Config, log *slog.Logger) (*Connector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, log: log}, nil
}

// Name reports the provider code this adapter serves.
func (c *Connector) Name() domain.POSProvider { return domain.POSIiko }

// ---------------------------------------------------------------------------
// domain.POSConnector — every method below is a scaffold
// ---------------------------------------------------------------------------

// PushBooking would register a confirmed BookEat booking as an iiko
// reservation. TODO(verify): build the /api/1/reserve/create request from req
// (organizationId from cfg, party size, window, optional TableRef, guest name/
// phone, req.IdempotencyKey mapped to whatever iiko uses to de-duplicate a
// retry), call it with a fresh bearer token, and translate the answer into a
// *domain.POSBookingRef.
func (c *Connector) PushBooking(ctx context.Context, req domain.POSBookingRequest) (*domain.POSBookingRef, error) {
	if err := validatePush(req); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("iiko: PushBooking: %w", ErrNotWired)
}

// CancelBooking would cancel a previously pushed iiko reservation by its
// POS-side id. TODO(verify): confirm the cancel endpoint and that cancelling an
// already-cancelled reservation is reported as success, so this stays
// idempotent for the caller.
func (c *Connector) CancelBooking(ctx context.Context, providerBookingID string) error {
	if err := requireID(providerBookingID); err != nil {
		return err
	}
	return fmt.Errorf("iiko: CancelBooking: %w", ErrNotWired)
}

// FetchOccupancy would pull external table occupancy so the availability engine
// never resells a slot iiko already sat someone in. TODO(verify): confirm
// whether occupancy is polled (which endpoint) or only pushed via webhook —
// that decision shapes this method and VerifyWebhook.
func (c *Connector) FetchOccupancy(ctx context.Context, q domain.POSOccupancyQuery) ([]domain.POSTableOccupancy, error) {
	if err := requireRestaurant(q.RestaurantID); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("iiko: FetchOccupancy: %w", ErrNotWired)
}

// VerifyWebhook would validate an iiko callback's signature FIRST and only then
// translate it into a *domain.POSEvent. TODO(verify): the webhook transport,
// event set and signature scheme are all unknown; until they are, no payload is
// interpreted. "Verify before you read" is the one rule that does not change
// whatever the contract turns out to be (mirrors the acquirer adapters).
func (c *Connector) VerifyWebhook(raw []byte, headers map[string]string) (*domain.POSEvent, error) {
	return nil, fmt.Errorf("iiko: VerifyWebhook: %w", ErrNotWired)
}

// ---------------------------------------------------------------------------
// helpers — the validation shape real bodies can be dropped into unchanged.
// ---------------------------------------------------------------------------

func validatePush(req domain.POSBookingRequest) error {
	switch {
	case req.BookingID == uuid.Nil:
		return fmt.Errorf("iiko: push without a booking id: %w", domain.ErrValidation)
	case req.RestaurantID == uuid.Nil:
		return fmt.Errorf("iiko: push without a restaurant id: %w", domain.ErrValidation)
	case req.IdempotencyKey == "":
		return fmt.Errorf("iiko: push without an idempotency key: %w", domain.ErrValidation)
	case req.Guests <= 0:
		return fmt.Errorf("iiko: party size must be positive: %w", domain.ErrValidation)
	case !req.EndsAt.After(req.StartsAt):
		return fmt.Errorf("iiko: reservation window must be non-empty: %w", domain.ErrValidation)
	}
	return nil
}

func requireID(providerBookingID string) error {
	if strings.TrimSpace(providerBookingID) == "" {
		return fmt.Errorf("iiko: empty provider booking id: %w", domain.ErrValidation)
	}
	return nil
}

func requireRestaurant(id uuid.UUID) error {
	if id == uuid.Nil {
		return fmt.Errorf("iiko: occupancy query without a restaurant id: %w", domain.ErrValidation)
	}
	return nil
}

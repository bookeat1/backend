package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// POSProvider is a point-of-sale system code, stored as VARCHAR wherever a
// restaurant's POS binding lands (a follow-up, see infrastructure/pos/doc.go).
// The domain knows the codes, never the protocols — those live in
// infrastructure/pos/<provider>, exactly as PaymentProvider relates to
// infrastructure/payment/<provider>.
type POSProvider string

const (
	// POSIiko is iiko (https://iiko.ru / iikoCloud/iikoTransport API). It is the
	// first POS BookEat integrates: iiko granted test access, credentials are
	// pending — see infrastructure/pos/iiko.
	POSIiko POSProvider = "iiko"
	// POSRKeeper is r_keeper (https://rkeeper.com). Template stub for now.
	POSRKeeper POSProvider = "rkeeper"
	// POSPoster is Poster (https://joinposter.com). Template stub for now.
	POSPoster POSProvider = "poster"
	// POSKwaaka is Kwaaka (https://kwaaka.com), an aggregator that itself fronts
	// iiko / r_keeper / Poster. From BookEat's side it is just another adapter
	// behind the same port; the fan-out to underlying POS systems is Kwaaka's
	// concern, not ours. Template stub for now.
	POSKwaaka POSProvider = "kwaaka"
)

// Valid reports whether p is a known POS provider code.
func (p POSProvider) Valid() bool {
	switch p {
	case POSIiko, POSRKeeper, POSPoster, POSKwaaka:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// POSConnector — the point-of-sale port
// ---------------------------------------------------------------------------

// POSConnector is the only thing the domain knows about POS systems. iiko,
// r_keeper, Poster and Kwaaka are adapters behind it
// (infrastructure/pos/...); adding a fifth POS must not require touching a
// single line in this package.
//
// Every implementation must be an anti-corruption layer: provider table ids,
// order/reservation states, error codes and payloads are translated into the
// domain types below and never leak outward. Provider ids and secrets live in
// each adapter's own env configuration, never in these request/response types.
type POSConnector interface {
	// PushBooking sends a confirmed BookEat booking to the venue's POS so the
	// floor staff see it in their own system. The returned POSBookingRef.ID is
	// the POS-side reservation id, to be stored by the caller for later
	// CancelBooking / correlation. Idempotency is carried on the request
	// (POSBookingRequest.IdempotencyKey) so a retry after a timeout resolves to
	// the same POS reservation instead of creating a second one.
	PushBooking(ctx context.Context, req POSBookingRequest) (*POSBookingRef, error)
	// CancelBooking cancels a previously pushed booking by its POS-side id
	// (POSBookingRef.ID). Cancelling one that the POS already dropped must be
	// treated as success by the adapter, not an error — cancellation is
	// idempotent from the caller's point of view.
	CancelBooking(ctx context.Context, providerBookingID string) error
	// FetchOccupancy pulls external table occupancy from the POS so the
	// availability engine never resells a slot the venue already sat someone in
	// through its own system. The results feed the source-agnostic occupancy
	// holds introduced in PR #22 (BookingSource "pos"); the adapter owns the
	// translation from POS order/reservation rows into POSTableOccupancy.
	FetchOccupancy(ctx context.Context, q POSOccupancyQuery) ([]POSTableOccupancy, error)
	// VerifyWebhook validates the signature and translates the raw body into a
	// domain event. It MUST do the signature check FIRST and return an error
	// without interpreting an unverified payload — a POS callback is no more
	// trustworthy than an acquirer's (mirrors PaymentGateway.VerifyWebhook).
	VerifyWebhook(raw []byte, headers map[string]string) (*POSEvent, error)
	// Name is the provider code this adapter serves.
	Name() POSProvider
}

// POSBookingRequest is everything a POS needs to register a BookEat booking,
// expressed in domain terms only. It carries no provider-specific field:
// terminal codes, organisation/point ids, table-map ids and secrets come from
// the adapter's own env configuration, never from here.
type POSBookingRequest struct {
	// BookingID is OUR booking id, echoed back by the POS in webhooks where the
	// POS supports pass-through metadata, so a callback resolves to a booking we
	// already know rather than creating one from thin air.
	BookingID uuid.UUID
	// RestaurantID selects which venue (and therefore which POS organisation /
	// terminal binding) this booking belongs to. The adapter maps it to the
	// provider-side ids from its own configuration.
	RestaurantID uuid.UUID
	// IdempotencyKey makes a retried PushBooking resolve to the same POS
	// reservation. Never empty for a real push.
	IdempotencyKey string
	// GuestName / GuestPhone identify the guest to the floor staff. Phone is
	// E.164. Nothing secret belongs here.
	GuestName  string
	GuestPhone string
	// Guests is the party size.
	Guests int
	// StartsAt / EndsAt is the reservation window, timezone-aware
	// (the caller passes venue-local instants as time.Time).
	StartsAt time.Time
	EndsAt   time.Time
	// TableRef optionally pins a specific POS table when BookEat already knows
	// which one; empty means "let the POS place the party". It is the POS-side
	// table identifier as the adapter understands it, resolved by the caller
	// from the venue's table binding — the domain does not model POS table maps.
	TableRef string
	// Notes is a free-text comment for the floor staff (allergies, occasion).
	Notes string
}

// POSBookingRef is the POS's acknowledgement of a pushed booking, already
// translated into domain terms by the adapter.
type POSBookingRef struct {
	// Provider is the POS that owns ID.
	Provider POSProvider
	// ID is the POS-side reservation identifier, stored by the caller for later
	// CancelBooking and webhook correlation.
	ID string
}

// POSOccupancyQuery bounds an occupancy pull to one venue and one time window,
// so the availability engine asks the POS only for what it is about to sell.
type POSOccupancyQuery struct {
	// RestaurantID selects the venue whose POS is queried.
	RestaurantID uuid.UUID
	// From / To is the half-open window [From, To) to read occupancy for. The
	// caller passes the same window it is computing availability for.
	From time.Time
	To   time.Time
}

// POSTableOccupancy is one occupied span reported by a POS, in domain terms.
// It is source-agnostic on purpose: the availability engine treats it exactly
// like a BookEat-owned hold (BookingSource "pos") and never resells the slot.
type POSTableOccupancy struct {
	// Provider is the POS this span came from.
	Provider POSProvider
	// TableRef is the POS-side table identifier the span occupies. Resolving it
	// to a BookEat table is the caller's job via the venue's table binding.
	TableRef string
	// StartsAt / EndsAt is the occupied window, half-open [StartsAt, EndsAt).
	StartsAt time.Time
	EndsAt   time.Time
	// Guests is the party size when the POS reports it; 0 when unknown.
	Guests int
	// ProviderRef is the POS-side order/reservation id backing this span, kept
	// for correlation and de-duplication against a booking we pushed ourselves.
	ProviderRef string
}

// POSEventType is the normalised meaning of a POS callback. Provider event
// names are mapped onto this closed set by the adapter.
type POSEventType string

const (
	// POSBookingConfirmed — the POS accepted/seated a reservation.
	POSBookingConfirmed POSEventType = "booking.confirmed"
	// POSBookingCancelled — the POS cancelled a reservation on its side.
	POSBookingCancelled POSEventType = "booking.cancelled"
	// POSTableOccupied — a table became occupied in the POS (walk-in or a
	// booking made directly in the POS), which the availability engine must
	// respect.
	POSTableOccupied POSEventType = "table.occupied"
	// POSTableFreed — a previously occupied table was released in the POS.
	POSTableFreed POSEventType = "table.freed"
	// POSEventUnknown is a callback we recognise as authentic but do not act on.
	// It is still worth recording — silence about a signed message from a POS is
	// how a double-booking slips through unnoticed.
	POSEventUnknown POSEventType = "unknown"
)

// POSEvent is a verified POS callback in domain terms. SignatureValid is
// carried explicitly rather than implied: a failed verification is evidence
// worth recording, not merely a dropped request (mirrors WebhookEvent).
type POSEvent struct {
	Provider POSProvider
	Type     POSEventType
	// ProviderEventID is the POS-side event id, used by the caller as an
	// idempotency key so a POS that retries a callback cannot apply it twice.
	ProviderEventID string
	// ProviderBookingID is the POS-side reservation id the event is about, when
	// the event concerns a booking (POSBookingRef.ID).
	ProviderBookingID string
	// BookingID is OUR booking id when the POS echoes it back in the callback
	// metadata; uuid.Nil when the event originated in the POS and has no BookEat
	// counterpart yet.
	BookingID uuid.UUID
	// Occupancy carries the table span when Type is POSTableOccupied /
	// POSTableFreed; nil otherwise.
	Occupancy *POSTableOccupancy
	// SignatureValid records the outcome of the signature check. An adapter
	// returns an error on a failed check AND never fills the rest of this
	// struct from an unverified payload; the flag exists so a caller that logs
	// the attempt has an explicit field rather than an inference.
	SignatureValid bool
}

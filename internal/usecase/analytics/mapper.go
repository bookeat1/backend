package analytics

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// mapRow projects one raw outbox row onto a PII-free analytics Event. The bool
// reports whether the row is a TRACKED event: false means "valid row, but not a
// product event we ship" (e.g. booking.waitlisted) — the worker skips it and
// still advances the cursor past it. A non-nil error means the payload could
// not be decoded (a poison row); the worker logs and skips it too.
//
// The whole no-PII guarantee lives here: each branch decodes the payload into
// an allow-list struct that has NO name/phone/email field, so those values are
// physically unreachable even though the source payload carries them.
func mapRow(source SourceName, row SourceRow) (Event, bool, error) {
	switch source {
	case SourceBookingOutbox:
		return mapBooking(row)
	case SourcePaymentOutbox:
		return mapPayment(row)
	default:
		return Event{}, false, fmt.Errorf("analytics: unknown source %q", source)
	}
}

// bookingProps is the ALLOW-LIST view of a booking outbox payload. It
// deliberately omits name/phone/email — the mapper cannot leak what it cannot
// decode.
type bookingProps struct {
	ID           uuid.UUID  `json:"id"`
	RestaurantID uuid.UUID  `json:"restaurant_id"`
	UserID       *uuid.UUID `json:"user_id"`
	Guests       int        `json:"guests"`
	Status       string     `json:"status"`
	Source       string     `json:"source"`
}

func bookingEventType(t string) (EventType, bool) {
	switch t {
	case "booking.created":
		return EventBookingCreated, true
	case "booking.confirmed":
		return EventBookingConfirmed, true
	case "booking.cancelled":
		return EventBookingCancelled, true
	case "booking.no_show":
		return EventNoShow, true
	default:
		// waitlisted / arrived / completed / updated / escalated /
		// message_created are real booking events but not shipped as product
		// analytics in this initial set.
		return "", false
	}
}

func mapBooking(row SourceRow) (Event, bool, error) {
	et, ok := bookingEventType(row.EventType)
	if !ok {
		return Event{}, false, nil
	}
	var p bookingProps
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return Event{}, false, fmt.Errorf("analytics: decode booking payload: %w", err)
	}
	userID := ""
	if p.UserID != nil && *p.UserID != uuid.Nil {
		userID = p.UserID.String()
	}
	return Event{
		Type:     et,
		UserID:   userID,
		DeviceID: deviceIDForBooking(p.ID),
		InsertID: row.ID.String(),
		Time:     row.CreatedAt,
		Properties: map[string]any{
			"restaurant_id": p.RestaurantID.String(),
			"guests":        p.Guests,
			"status":        p.Status,
			"source":        p.Source,
		},
	}, true, nil
}

// paymentProps is the ALLOW-LIST view of a payment outbox payload — ids, coarse
// money bucket and enums only, never a card token or acquirer body.
type paymentProps struct {
	BookingID    uuid.UUID `json:"booking_id"`
	RestaurantID uuid.UUID `json:"restaurant_id"`
	Purpose      string    `json:"purpose"`
	Status       string    `json:"status"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
}

func paymentEventType(t string) (EventType, bool) {
	switch t {
	case "payment.captured":
		return EventPaymentCaptured, true
	case "payment.refunded":
		return EventPaymentRefunded, true
	default:
		// created / authorized / voided / failed / expired /
		// partially_refunded / settled are not shipped in this initial set.
		return "", false
	}
}

func mapPayment(row SourceRow) (Event, bool, error) {
	et, ok := paymentEventType(row.EventType)
	if !ok {
		return Event{}, false, nil
	}
	var p paymentProps
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		return Event{}, false, fmt.Errorf("analytics: decode payment payload: %w", err)
	}
	// The payment outbox payload carries no user_id; the payment is still
	// attributable to a session via its booking. We set device_id from the
	// booking id (stable dedupe) and leave user_id empty.
	return Event{
		Type:     et,
		UserID:   "",
		DeviceID: deviceIDForBooking(p.BookingID),
		InsertID: row.ID.String(),
		Time:     row.CreatedAt,
		Properties: map[string]any{
			"restaurant_id": p.RestaurantID.String(),
			"booking_id":    p.BookingID.String(),
			"purpose":       p.Purpose,
			"status":        p.Status,
			"currency":      p.Currency,
			"amount_bucket": amountBucket(p.AmountMinor),
		},
	}, true, nil
}

// deviceIDForBooking builds a stable, non-PII Amplitude device_id (>=5 chars,
// Amplitude's minimum) from a booking id. Constant per booking so Amplitude's
// device_id+insert_id dedupe holds across a reship.
func deviceIDForBooking(bookingID uuid.UUID) string { return "bk-" + bookingID.String() }

// amountBucket coarsens an amount in minor units into a non-identifying bucket,
// so a chart can segment by order size without the exact figure. Thresholds are
// in minor units and currency-agnostic (KZT is the only live currency today).
func amountBucket(minor int64) string {
	switch {
	case minor <= 0:
		return "0"
	case minor < 500_000: // < 5 000.00
		return "lt_5000"
	case minor < 2_000_000: // < 20 000.00
		return "5000_20000"
	case minor < 10_000_000: // < 100 000.00
		return "20000_100000"
	default:
		return "gte_100000"
	}
}

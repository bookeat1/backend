package promocodes

import (
	"context"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Facade is the promo-code application layer.
//
// The split between the three guest-facing methods is the whole design:
// Precheck answers a question ("would this code work?") and changes nothing,
// ResolveForBooking decides WHICH campaign a booking joins before the
// transaction opens, and ConsumeTx is the only one that may say "the limit is
// spent" — because it is the only one holding the row lock that makes the
// answer true a moment later (ADR-047).
type Facade interface {
	// Precheck is the light "is this code valid for me, here?" the client
	// calls before the guest presses «Забронировать». It spends nothing and
	// holds no lock, so its limit verdict is a SNAPSHOT: a code it called good
	// can still lose the last place to a booking that commits a second later.
	// That is why the booking path re-decides under the lock instead of
	// trusting this answer.
	Precheck(ctx context.Context, code string, restaurantID uuid.UUID, userID uuid.UUID) (*domain.PromoCodeResolution, error)

	// ResolveForBooking validates everything about a code that does not need a
	// lock — it exists, it is active, its window is open, the campaign behind
	// it is live and runs at THIS venue — and returns what the booking should
	// be tagged with. Limits are deliberately NOT decided here.
	ResolveForBooking(ctx context.Context, code string, restaurantID uuid.UUID, userID uuid.UUID) (*domain.PromoCodeResolution, error)

	// ConsumeTx spends one activation of the code for userID. It MUST be
	// called inside the booking-creation transaction, as its FIRST statement:
	// it takes the promo_codes row lock, and the lock order promo_code → venue
	// is one-way (ADR-047).
	//
	// It writes nothing. "Spending" a code is inserting the booking that
	// carries it; this call is what makes that insert safe to do.
	ConsumeTx(ctx context.Context, promoCodeID uuid.UUID, userID uuid.UUID) error
}

type facade struct {
	codes    domain.PromoCodeRepository
	promos   promoReader
	bookings usageCounter
	// now is the clock, injectable so a test can place a code's window around
	// a fixed moment instead of racing the wall clock.
	now func() time.Time
}

// NewFacade builds the promo-code facade.
func NewFacade(codes domain.PromoCodeRepository, promos promoReader, bookings usageCounter) Facade {
	return &facade{codes: codes, promos: promos, bookings: bookings, now: time.Now}
}

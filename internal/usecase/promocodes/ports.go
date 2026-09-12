// Package promocodes is the application logic of guest promo codes: turning a
// string a guest typed into "which campaign does this booking join", and
// deciding — under the promo_codes row lock, inside the booking transaction —
// whether one more booking still fits the code's limits (ADR-047).
//
// Layering notes:
//   - the package never imports another domain's concrete repository: the two
//     things it needs from outside (is the campaign behind the code live, and
//     how much of the limit do the BOOKINGS say is spent) are declared here as
//     minimal local ports and bound in bootstrap/deps.go;
//   - there is no activation counter to keep in step, on purpose: every number
//     this package acts on is counted from the bookings themselves, so a
//     cancellation frees its place with no code that could drift.
package promocodes

import (
	"context"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// promoReader is the minimal slice of the promos context this package needs:
// confirm the campaign a code points at is a REAL, currently live one, and
// read the text shown next to the code field.
//
// GetPublicDetail's own visibility rule (published, window contains now) is
// exactly "a campaign a guest could honestly be joining today" — a draft,
// hidden or expired promo answers domain.ErrNotFound here, same as an absent
// one, and the guest is told the code is not being accepted rather than being
// tagged into a campaign nobody is running. Bound to usecase/promos.Facade.
type promoReader interface {
	GetPublicDetail(ctx context.Context, promoID uuid.UUID) (*domain.PromoListItem, error)
}

// usageCounter is the single read that decides a limit: how many distinct
// guests already carry the code and how many bookings this one guest does
// (domain.BookingRepository.CountPromoCodeUsage). Declared as a port rather
// than taking the booking repository whole so this package cannot write a
// booking, and bound to the same repository in deps.
//
// Inside ConsumeTx this MUST run after the code's row lock; see ConsumeTx.
type usageCounter interface {
	CountPromoCodeUsage(ctx context.Context, promoCodeID uuid.UUID, userID uuid.UUID) (domain.PromoCodeUsage, error)
}

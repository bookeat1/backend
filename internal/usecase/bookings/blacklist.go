package bookings

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

// BlacklistUseCase is the venue stop list (spec §7: GET|POST
// /restaurants/{id}/blacklist). Every method resolves venue access first;
// a manager of another restaurant is rejected with ErrForbidden.
//
// Entries created here are always venue-scoped, for EVERY actor including an
// admin: a stop-list write reached through /restaurants/{id}/blacklist is a
// statement about that venue, and letting the same call mean "ban platform-wide"
// depending on who is logged in is exactly the kind of ambient authority that
// gets used by accident.
//
// Global entries (restaurant_id IS NULL) are therefore NOT creatable here. They
// are readable by the venue — they explain a refusal the manager did not cause
// — and Remove refuses to let a manager lift one. Creating them belongs to a
// separate admin endpoint that does not exist yet; until it does, they are
// seeded out of band.
type BlacklistUseCase interface {
	List(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.BlacklistEntry, error)
	Add(ctx context.Context, actor Actor, restaurantID uuid.UUID, in BlacklistInput) (*domain.BlacklistEntry, error)
	Remove(ctx context.Context, actor Actor, restaurantID, entryID uuid.UUID) error
}

// BlacklistInput identifies the guest to block. At least one of UserID, Phone
// or Email must be given; the phone is normalized to E.164 and the email
// lower-cased, otherwise the stop list would never match a real booking.
type BlacklistInput struct {
	UserID *uuid.UUID
	Phone  string
	Email  string
	Reason *string
}

type blacklistUseCase struct {
	entries  domain.BookingBlacklistRepository
	managers managerChecker
}

// NewBlacklistUseCase constructs the stop-list usecase.
func NewBlacklistUseCase(entries domain.BookingBlacklistRepository, managers managerChecker) BlacklistUseCase {
	return &blacklistUseCase{entries: entries, managers: managers}
}

func (u *blacklistUseCase) List(ctx context.Context, actor Actor, restaurantID uuid.UUID) ([]domain.BlacklistEntry, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	return u.entries.ListByRestaurant(ctx, restaurantID)
}

func (u *blacklistUseCase) Add(ctx context.Context, actor Actor, restaurantID uuid.UUID, in BlacklistInput) (*domain.BlacklistEntry, error) {
	if _, err := requireStaff(ctx, u.managers, actor, restaurantID); err != nil {
		return nil, err
	}
	// Always venue-scoped — an admin acting here bans the guest at this venue
	// only, never platform-wide (see the interface doc).
	e := &domain.BlacklistEntry{
		ID:           uuid.New(),
		RestaurantID: &restaurantID,
		UserID:       in.UserID,
		Reason:       in.Reason,
		CreatedBy:    actorID(actor),
		IsActive:     true,
	}
	if p := strings.TrimSpace(in.Phone); p != "" {
		normalized := phone.Normalize(p)
		if normalized == "" {
			return nil, fmt.Errorf("%w: phone is not a valid number", domain.ErrValidation)
		}
		e.PhoneNormalized = &normalized
	}
	if m := strings.ToLower(strings.TrimSpace(in.Email)); m != "" {
		e.Email = &m
	}
	if e.UserID == nil && e.PhoneNormalized == nil && e.Email == nil {
		return nil, fmt.Errorf("%w: user_id, phone or email is required", domain.ErrValidation)
	}
	if err := u.entries.Create(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// Remove deactivates one entry. The entry is looked up through the venue's own
// list first: Deactivate takes an id alone, so calling it directly would let a
// manager of venue A delete venue B's entry (cross-tenant IDOR). Global entries
// are visible to the venue but only an admin may lift them.
func (u *blacklistUseCase) Remove(ctx context.Context, actor Actor, restaurantID, entryID uuid.UUID) error {
	acc, err := requireStaff(ctx, u.managers, actor, restaurantID)
	if err != nil {
		return err
	}
	entries, err := u.entries.ListByRestaurant(ctx, restaurantID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID != entryID {
			continue
		}
		if e.RestaurantID == nil && !acc.admin {
			return fmt.Errorf("%w: this is a platform-wide entry", domain.ErrForbidden)
		}
		return u.entries.Deactivate(ctx, entryID)
	}
	return fmt.Errorf("%w: blacklist entry", domain.ErrNotFound)
}

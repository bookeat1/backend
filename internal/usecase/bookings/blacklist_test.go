package bookings

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type blacklistHarness struct {
	uc      BlacklistUseCase
	entries *fakeBlacklist

	restaurantID uuid.UUID
	manager      Actor
	otherManager Actor
	admin        Actor
	guest        Actor
}

func newBlacklistHarness(t *testing.T) *blacklistHarness {
	t.Helper()
	rid := uuid.New()
	h := &blacklistHarness{
		entries:      &fakeBlacklist{},
		restaurantID: rid,
		manager:      Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		otherManager: Actor{UserID: uuid.New(), Role: domain.RoleRestaurant},
		admin:        Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
		guest:        Actor{UserID: uuid.New(), Role: domain.RoleUser},
	}
	h.uc = NewBlacklistUseCase(h.entries, newFakeManagers([2]uuid.UUID{h.manager.UserID, rid}))
	return h
}

// Entries added through /restaurants/{id}/blacklist are venue-scoped for EVERY
// actor, admin included: this endpoint cannot mint a platform-wide ban. The
// package doc says so; this test is what keeps it true.
func TestBlacklistAddIsAlwaysVenueScoped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor func(h *blacklistHarness) Actor
	}{
		{"venue manager", func(h *blacklistHarness) Actor { return h.manager }},
		{"platform admin", func(h *blacklistHarness) Actor { return h.admin }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newBlacklistHarness(t)
			got, err := h.uc.Add(context.Background(), tc.actor(h), h.restaurantID, BlacklistInput{
				Phone: "8 (707) 123-45-67",
				Email: "  Damir@Example.COM ",
			})
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if got.RestaurantID == nil || *got.RestaurantID != h.restaurantID {
				t.Fatalf("restaurant_id = %v, want the venue — a global entry must not be creatable here", got.RestaurantID)
			}
			if len(h.entries.created) != 1 || h.entries.created[0].RestaurantID == nil {
				t.Fatalf("persisted entry = %+v, want venue-scoped", h.entries.created)
			}
			// Contacts are normalized, or the stop list would never match.
			if got.PhoneNormalized == nil || *got.PhoneNormalized != "+77071234567" {
				t.Fatalf("phone_normalized = %v", got.PhoneNormalized)
			}
			if got.Email == nil || *got.Email != "damir@example.com" {
				t.Fatalf("email = %v", got.Email)
			}
		})
	}
}

func TestBlacklistAddRejectsEmptyIdentity(t *testing.T) {
	h := newBlacklistHarness(t)
	_, err := h.uc.Add(context.Background(), h.manager, h.restaurantID, BlacklistInput{})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("= %v, want ErrValidation", err)
	}
	if len(h.entries.created) != 0 {
		t.Fatal("a rejected entry must not be persisted")
	}
}

func TestBlacklistAccess(t *testing.T) {
	h := newBlacklistHarness(t)
	for _, actor := range []Actor{h.otherManager, h.guest} {
		if _, err := h.uc.Add(context.Background(), actor, h.restaurantID,
			BlacklistInput{Phone: "+77071234567"}); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("Add as %s = %v, want ErrForbidden", actor.Role, err)
		}
		if _, err := h.uc.List(context.Background(), actor, h.restaurantID); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("List as %s = %v, want ErrForbidden", actor.Role, err)
		}
	}
}

// A manager may lift their own venue's entry but not a global one; an admin may
// lift both. Removal goes through the venue's own list, so a manager cannot
// reach another venue's entry by id.
func TestBlacklistRemoveScoping(t *testing.T) {
	venueEntry := domain.BlacklistEntry{ID: uuid.New(), RestaurantID: new(uuid.UUID), IsActive: true}
	globalEntry := domain.BlacklistEntry{ID: uuid.New(), IsActive: true}

	t.Run("manager removes the venue entry", func(t *testing.T) {
		h := newBlacklistHarness(t)
		*venueEntry.RestaurantID = h.restaurantID
		h.entries.list = []domain.BlacklistEntry{venueEntry, globalEntry}
		if err := h.uc.Remove(context.Background(), h.manager, h.restaurantID, venueEntry.ID); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if len(h.entries.deactivated) != 1 || h.entries.deactivated[0] != venueEntry.ID {
			t.Fatalf("deactivated = %v", h.entries.deactivated)
		}
	})
	t.Run("manager may not lift a global entry", func(t *testing.T) {
		h := newBlacklistHarness(t)
		h.entries.list = []domain.BlacklistEntry{globalEntry}
		if err := h.uc.Remove(context.Background(), h.manager, h.restaurantID, globalEntry.ID); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("= %v, want ErrForbidden", err)
		}
		if len(h.entries.deactivated) != 0 {
			t.Fatal("nothing must be deactivated")
		}
	})
	t.Run("admin may lift a global entry", func(t *testing.T) {
		h := newBlacklistHarness(t)
		h.entries.list = []domain.BlacklistEntry{globalEntry}
		if err := h.uc.Remove(context.Background(), h.admin, h.restaurantID, globalEntry.ID); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	})
	t.Run("an entry outside the venue's list is not found", func(t *testing.T) {
		h := newBlacklistHarness(t)
		if err := h.uc.Remove(context.Background(), h.manager, h.restaurantID, uuid.New()); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("= %v, want ErrNotFound", err)
		}
	})
}

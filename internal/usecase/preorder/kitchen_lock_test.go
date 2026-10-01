package preorder

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeKitchen struct {
	o   *domain.KitchenOrder
	err error
}

func (f fakeKitchen) GetByBookingID(context.Context, uuid.UUID) (*domain.KitchenOrder, error) {
	if f.o == nil {
		return nil, domain.ErrNotFound
	}
	return f.o, f.err
}

func TestReplace_SentToKitchenLocksEveryone(t *testing.T) {
	cases := []struct {
		status domain.KitchenOrderStatus
		locked bool
	}{
		{domain.KitchenOrderSending, true}, {domain.KitchenOrderSent, true}, {domain.KitchenOrderFailedUnknown, true},
		{domain.KitchenOrderCancelling, true}, {domain.KitchenOrderCancelFailed, true},
		{domain.KitchenOrderFailed, false}, {domain.KitchenOrderCancelled, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			owner := uuid.New()
			staff := uuid.New()
			for name, actor := range map[string]Actor{
				"guest": {UserID: owner, Role: domain.RoleUser},
				"staff": {UserID: staff, Role: domain.RoleRestaurant},
				"admin": {UserID: uuid.New(), Role: domain.RoleAdmin},
			} {
				h := newHarness(t, &owner, domain.BookingPending, map[string]bool{staff.String() + "|" + restA.String(): true})
				h.uc.WithKitchenOrders(fakeKitchen{o: &domain.KitchenOrder{Status: tc.status}})
				_, err := h.uc.Replace(context.Background(), actor, h.booking.ID, []Line{{MenuItemID: h.dishA.ID, Quantity: 1}})
				code, _ := domain.CodeOf(err)
				got := code == domain.CodePreorderSentToKitchen
				if got != tc.locked {
					t.Fatalf("%s: err=%v locked=%v want %v", name, err, got, tc.locked)
				}
			}
		})
	}
}

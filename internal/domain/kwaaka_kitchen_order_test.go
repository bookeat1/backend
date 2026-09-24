package domain

import "testing"

func TestKitchenOrderTransitions(t *testing.T) {
	all := []KitchenOrderStatus{KitchenOrderSending, KitchenOrderSent, KitchenOrderCancelling, KitchenOrderCancelled,
		KitchenOrderFailed, KitchenOrderFailedUnknown, KitchenOrderCancelFailed}
	legal := map[[2]KitchenOrderStatus]bool{
		{KitchenOrderSending, KitchenOrderSent}:            true,
		{KitchenOrderSending, KitchenOrderFailed}:          true,
		{KitchenOrderSending, KitchenOrderFailedUnknown}:   true,
		{KitchenOrderSending, KitchenOrderCancelled}:       true,
		{KitchenOrderSending, KitchenOrderCancelling}:      true,
		{KitchenOrderSent, KitchenOrderCancelling}:         true,
		{KitchenOrderCancelling, KitchenOrderCancelled}:    true,
		{KitchenOrderCancelling, KitchenOrderCancelFailed}: true,
	}
	for _, from := range all {
		for _, to := range all {
			if got, want := from.CanTransitionTo(to), legal[[2]KitchenOrderStatus{from, to}]; got != want {
				t.Errorf("%s -> %s: got %v want %v", from, to, got, want)
			}
		}
		if !from.Valid() {
			t.Errorf("%s should be valid", from)
		}
	}
	for _, s := range []KitchenOrderStatus{KitchenOrderCancelled, KitchenOrderFailed, KitchenOrderFailedUnknown, KitchenOrderCancelFailed} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
		for _, to := range all {
			if s.CanTransitionTo(to) {
				t.Errorf("terminal %s must not go to %s", s, to)
			}
		}
	}
	if KitchenOrderStatus("bogus").Valid() {
		t.Error("bogus must be invalid")
	}
}

func TestPosOrderStateRank(t *testing.T) {
	if !(PosStateOpen.Rank() < PosStateBillPrinted.Rank() && PosStateBillPrinted.Rank() < PosStateClosed.Rank()) {
		t.Fatal("rank must grow open < bill_printed < closed")
	}
	if PosStateClosed.Rank() != PosStateDeleted.Rank() || PosStateUnknown.Rank() != 0 {
		t.Fatal("closed = deleted, unknown = 0")
	}
}

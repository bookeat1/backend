package kwaaka

import (
	"strings"

	"backend-core/internal/domain"
)

// MapPosStatus is the ONE place that maps Kwaaka's raw order status string to
// the coarse domain state. Kwaaka has not sent the list of status values
// (decision 24.09), so the dictionary below is a best guess covering the common
// iiko/r_keeper vocabulary; anything else is PosStateUnknown and is stored raw,
// never applied. When Kwaaka names its statuses, edit ONLY this function.
func MapPosStatus(raw string) domain.PosOrderState {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "new", "open", "opened", "created", "accepted", "in_progress", "cooking", "ready", "served":
		return domain.PosStateOpen
	case "bill", "bill_printed", "billed", "printed", "waiting_payment":
		return domain.PosStateBillPrinted
	case "closed", "close", "paid", "completed", "done", "finished":
		return domain.PosStateClosed
	case "deleted", "delete", "cancelled", "canceled", "cancel", "removed", "rejected":
		return domain.PosStateDeleted
	}
	return domain.PosStateUnknown
}

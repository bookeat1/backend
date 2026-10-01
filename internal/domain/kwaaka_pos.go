package domain

import (
	"context"
	"time"
)

// KitchenSnapshotItem is one POS position of a kitchen order. PriceMinor is
// the frozen booking-item price; the adapter converts to Kwaaka's decimal.
type KitchenSnapshotItem struct {
	ProductID  string `json:"product_id"`
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
}

// KitchenSnapshot is OUR frozen copy of the order (stored in
// kwaaka_kitchen_orders.request_snapshot). A retry re-sends exactly this, so
// the wire body is the same bytes each time (order_id, table, positions).
type KitchenSnapshot struct {
	OrderID       string                `json:"order_id"`
	TableID       string                `json:"table_id"`
	CustomerName  string                `json:"customer_name"`
	CustomerPhone string                `json:"customer_phone"`
	Items         []KitchenSnapshotItem `json:"items"`
	Comment       string                `json:"comment"`
}

// PosCallOutcome classifies ONE HTTP attempt against Kwaaka (plan §2.6).
type PosCallOutcome string

const (
	PosCreated   PosCallOutcome = "created"    // 200 (also 200 with an unparseable body)
	PosRejected  PosCallOutcome = "rejected"   // 400/404: definitive "no"
	PosAuth      PosCallOutcome = "auth"       // 401/403: retry later, alert in log
	PosRateLimit PosCallOutcome = "rate_limit" // 429
	PosUnknown   PosCallOutcome = "unknown"    // 5xx, timeout, connection reset: may have been applied
)

// PosCreateResult is the outcome of CreateTableOrder. PosOrderID is Kwaaka's
// id, empty when the 200 body did not parse.
type PosCreateResult struct {
	Outcome    PosCallOutcome
	PosOrderID string
	Message    string
}

// PosCancelResult is the outcome of CancelTableOrder (same classification;
// PosCreated means "cancelled").
type PosCancelResult struct {
	Outcome PosCallOutcome
	Message string
}

// PosOrder is a POS order in domain terms.
type PosOrder struct {
	ID              string
	Raw             string // status as Kwaaka sent it
	State           PosOrderState
	TableIDs        []string
	WhenBillPrinted *time.Time
	WhenClosed      *time.Time
}

// PosTable is one table of the venue's POS.
type PosTable struct {
	ID              string
	Number          int
	Name            string
	SeatingCapacity int
	SectionName     string
}

// KwaakaOrderPOS is the port to Kwaaka's order and table endpoints.
// Implemented by infrastructure/kwaaka; declared here because usecase/
// kwaakaorders and the admin facade both consume it.
type KwaakaOrderPOS interface {
	// CreateTableOrder makes exactly ONE HTTP attempt and classifies it.
	CreateTableOrder(ctx context.Context, kwaakaRestaurantID string, s KitchenSnapshot) PosCreateResult
	// CancelTableOrder makes exactly ONE HTTP attempt.
	CancelTableOrder(ctx context.Context, kwaakaRestaurantID, orderID, reason string) PosCancelResult
	// GetOrder looks an order up by our order_id or Kwaaka's id. ErrNotFound
	// when Kwaaka has no such order; ErrUnavailable when it cannot tell.
	GetOrder(ctx context.Context, kwaakaRestaurantID, orderID string) (*PosOrder, error)
	// ListOrdersByTables returns orders on the given tables (all, open or not).
	ListOrdersByTables(ctx context.Context, kwaakaRestaurantID string, tableIDs []string) ([]PosOrder, error)
	GetTables(ctx context.Context, kwaakaRestaurantID string) ([]PosTable, error)
}

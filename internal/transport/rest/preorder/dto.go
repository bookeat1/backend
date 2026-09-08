package preorder

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/preorder"
)

// replaceRequest is the full desired pre-order for a booking. The client sends
// ONLY the menu item id, quantity and an optional comment per line — never a
// price. An empty (or omitted) items list clears the pre-order.
type replaceRequest struct {
	Items []lineRequest `json:"items"`
}

type lineRequest struct {
	MenuItemID string  `json:"menu_item_id"`
	Quantity   int     `json:"quantity"`
	Comment    *string `json:"comment"`
}

// toLines validates the id shapes and maps to the usecase input. Business
// validation (item exists / belongs to the venue / is available / price) is the
// usecase's job; here we only reject a malformed uuid so it surfaces as 422, not
// a 500 deep in the usecase.
func (r replaceRequest) toLines() ([]uc.Line, error) {
	out := make([]uc.Line, 0, len(r.Items))
	for _, it := range r.Items {
		id, err := uuid.Parse(it.MenuItemID)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid menu_item_id %q", domain.ErrValidation, it.MenuItemID)
		}
		out = append(out, uc.Line{MenuItemID: id, Quantity: it.Quantity, Comment: it.Comment})
	}
	return out, nil
}

// preorderResponse is a booking's pre-order: its priced lines plus the
// server-computed total (the amount the pre-order payment charges).
type preorderResponse struct {
	BookingID  string         `json:"booking_id"`
	Items      []itemResponse `json:"items"`
	TotalMinor int64          `json:"total_minor"`
	Currency   string         `json:"currency"`
}

type itemResponse struct {
	ID         string  `json:"id"`
	MenuItemID *string `json:"menu_item_id"`
	Name       string  `json:"name"`
	PriceMinor int64   `json:"price_minor"`
	Quantity   int     `json:"quantity"`
	TotalMinor int64   `json:"total_minor"`
	Status     string  `json:"status"`
	Comment    *string `json:"comment"`
}

func toResponse(p *uc.Preorder) preorderResponse {
	items := make([]itemResponse, 0, len(p.Items))
	for i := range p.Items {
		it := p.Items[i]
		var menuID *string
		if it.MenuItemID != nil {
			s := it.MenuItemID.String()
			menuID = &s
		}
		items = append(items, itemResponse{
			ID:         it.ID.String(),
			MenuItemID: menuID,
			Name:       it.ItemName,
			PriceMinor: it.PriceMinor,
			Quantity:   it.Quantity,
			TotalMinor: it.TotalMinor(),
			Status:     string(it.Status),
			Comment:    it.Comment,
		})
	}
	return preorderResponse{
		BookingID:  p.BookingID.String(),
		Items:      items,
		TotalMinor: p.TotalMinor,
		Currency:   p.Currency,
	}
}

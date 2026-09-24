package kwaaka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"backend-core/internal/domain"
)

// OrderPOS implements domain.KwaakaOrderPOS over the Table Booking API
// (v1.1.0): POST /restaurants/{id}/order, /order/cancel, GET /orders, /tables.
type OrderPOS struct{ client *Client }

// NewOrderPOS builds the order adapter on a shared Client.
func NewOrderPOS(c *Client) *OrderPOS { return &OrderPOS{client: c} }

var _ domain.KwaakaOrderPOS = (*OrderPOS)(nil)

// wirePrice / wireProduct / wireTableOrder mirror TableOrder in the swagger.
type wirePrice struct {
	Value        float64 `json:"value"`
	CurrencyCode string  `json:"currency_code"`
}
type wireProduct struct {
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	Quantity int       `json:"quantity"`
	Price    wirePrice `json:"price"`
}
type wireTableOrder struct {
	OrderID       string        `json:"order_id"`
	TableID       string        `json:"table_id"`
	CustomerName  string        `json:"customer_name,omitempty"`
	CustomerPhone string        `json:"customer_phone,omitempty"`
	Products      []wireProduct `json:"products,omitempty"`
	Comment       string        `json:"comment,omitempty"`
}

// BuildTableOrderBody renders the snapshot as the TableOrder JSON. It is
// deterministic (struct field order, no maps), so every retry sends the same bytes.
func BuildTableOrderBody(s domain.KitchenSnapshot) ([]byte, error) {
	w := wireTableOrder{OrderID: s.OrderID, TableID: s.TableID, CustomerName: s.CustomerName,
		CustomerPhone: s.CustomerPhone, Comment: s.Comment}
	for _, it := range s.Items {
		w.Products = append(w.Products, wireProduct{ID: it.ProductID, Name: it.Name, Quantity: it.Quantity,
			Price: wirePrice{Value: float64(it.PriceMinor) / 100, CurrencyCode: it.Currency}})
	}
	return json.Marshal(w)
}

func (o *OrderPOS) endpoint(kwaakaRestaurantID, path string, extra url.Values) string {
	q := url.Values{}
	q.Set("service", o.client.cfg.Service)
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return o.client.cfg.BaseURL + o.client.cfg.PathPrefix + "/restaurants/" + url.PathEscape(kwaakaRestaurantID) + path + "?" + q.Encode()
}

// postOnce performs exactly one POST (no retries: the caller owns the retry
// policy, ADR-048) and classifies the result.
func (o *OrderPOS) postOnce(ctx context.Context, fullURL string, body []byte) (domain.PosCallOutcome, []byte, int, string) {
	ctx, cancel := context.WithTimeout(ctx, o.client.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return domain.PosUnknown, nil, 0, "build request: " + err.Error()
	}
	req.Header.Set("Authorization", o.client.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.doer.Do(req)
	if err != nil {
		return domain.PosUnknown, nil, 0, "transport: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// Status was received but the body broke: on 200 the order exists.
		if resp.StatusCode == http.StatusOK {
			return domain.PosCreated, nil, 200, ""
		}
		return domain.PosUnknown, nil, resp.StatusCode, "read response: " + err.Error()
	}
	return classify(resp.StatusCode), raw, resp.StatusCode, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(raw))
}

func classify(status int) domain.PosCallOutcome {
	switch {
	case status == http.StatusOK || status == http.StatusNoContent:
		return domain.PosCreated
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return domain.PosRejected
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.PosAuth
	case status == http.StatusTooManyRequests:
		return domain.PosRateLimit
	default: // 5xx and anything unexpected: we cannot tell whether it applied
		return domain.PosUnknown
	}
}

func (o *OrderPOS) CreateTableOrder(ctx context.Context, kwaakaRestaurantID string, s domain.KitchenSnapshot) domain.PosCreateResult {
	body, err := BuildTableOrderBody(s)
	if err != nil {
		return domain.PosCreateResult{Outcome: domain.PosRejected, Message: "marshal: " + err.Error()}
	}
	outcome, raw, _, msg := o.postOnce(ctx, o.endpoint(kwaakaRestaurantID, "/order", nil), body)
	res := domain.PosCreateResult{Outcome: outcome, Message: msg}
	if outcome == domain.PosCreated {
		// Response is a JSON string (the id); tolerate a bare token.
		var id string
		if err := json.Unmarshal(raw, &id); err == nil {
			res.PosOrderID = strings.TrimSpace(id)
		}
	}
	return res
}

func (o *OrderPOS) CancelTableOrder(ctx context.Context, kwaakaRestaurantID, orderID, reason string) domain.PosCancelResult {
	body, _ := json.Marshal(struct {
		OrderID      string `json:"order_id"`
		CancelReason string `json:"cancel_reason,omitempty"`
	}{orderID, reason})
	outcome, _, _, msg := o.postOnce(ctx, o.endpoint(kwaakaRestaurantID, "/order/cancel", nil), body)
	return domain.PosCancelResult{Outcome: outcome, Message: msg}
}

type rawOrder struct {
	ID              string   `json:"id"`
	TableIDs        []string `json:"table_ids"`
	Status          string   `json:"status"`
	WhenBillPrinted string   `json:"when_bill_printed"`
	WhenClosed      string   `json:"when_closed"`
}

type rawOrders struct {
	Orders []rawOrder `json:"orders"`
}

func parseWhen(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// toPosOrder maps a wire order. A when_closed timestamp means closed even if
// the status string is unfamiliar; when_bill_printed means bill_printed.
func toPosOrder(r rawOrder) domain.PosOrder {
	p := domain.PosOrder{ID: r.ID, Raw: r.Status, TableIDs: r.TableIDs,
		WhenBillPrinted: parseWhen(r.WhenBillPrinted), WhenClosed: parseWhen(r.WhenClosed)}
	p.State = MapPosStatus(r.Status)
	if p.State == domain.PosStateUnknown || p.State == domain.PosStateOpen {
		switch {
		case p.WhenClosed != nil:
			p.State = domain.PosStateClosed
		case p.WhenBillPrinted != nil && p.State != domain.PosStateOpen:
			p.State = domain.PosStateBillPrinted
		}
	}
	return p
}

func (o *OrderPOS) fetchOrders(ctx context.Context, kwaakaRestaurantID string, q url.Values) ([]domain.PosOrder, error) {
	// client.get takes a path relative to BaseURL+PathPrefix.
	full := o.endpoint(kwaakaRestaurantID, "/orders", q)
	path := strings.TrimPrefix(full, o.client.cfg.BaseURL+o.client.cfg.PathPrefix)
	raw, err := o.client.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var parsed rawOrders
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: unparseable orders response: %w", domain.ErrUnavailable, err)
	}
	out := make([]domain.PosOrder, 0, len(parsed.Orders))
	for _, r := range parsed.Orders {
		out = append(out, toPosOrder(r))
	}
	return out, nil
}

func (o *OrderPOS) GetOrder(ctx context.Context, kwaakaRestaurantID, orderID string) (*domain.PosOrder, error) {
	orders, err := o.fetchOrders(ctx, kwaakaRestaurantID, url.Values{"orderId": {orderID}})
	if err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return nil, fmt.Errorf("kwaaka order %s: %w", orderID, domain.ErrNotFound)
	}
	return &orders[0], nil
}

// ListOrdersByTables sends tableIds as a repeated parameter (explode: true).
func (o *OrderPOS) ListOrdersByTables(ctx context.Context, kwaakaRestaurantID string, tableIDs []string) ([]domain.PosOrder, error) {
	if len(tableIDs) == 0 {
		return nil, errors.New("kwaaka: no table ids")
	}
	return o.fetchOrders(ctx, kwaakaRestaurantID, url.Values{"tableIds": tableIDs})
}

type rawTables struct {
	Tables []struct {
		ID              string `json:"id"`
		Number          int    `json:"number"`
		Name            string `json:"name"`
		SeatingCapacity int    `json:"seating_capacity"`
		SectionName     string `json:"section_name"`
	} `json:"tables"`
}

func (o *OrderPOS) GetTables(ctx context.Context, kwaakaRestaurantID string) ([]domain.PosTable, error) {
	full := o.endpoint(kwaakaRestaurantID, "/tables", nil)
	raw, err := o.client.get(ctx, strings.TrimPrefix(full, o.client.cfg.BaseURL+o.client.cfg.PathPrefix))
	if err != nil {
		return nil, err
	}
	var parsed rawTables
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: unparseable tables response: %w", domain.ErrUnavailable, err)
	}
	out := make([]domain.PosTable, 0, len(parsed.Tables))
	for _, t := range parsed.Tables {
		out = append(out, domain.PosTable{ID: t.ID, Number: t.Number, Name: t.Name,
			SeatingCapacity: t.SeatingCapacity, SectionName: t.SectionName})
	}
	return out, nil
}

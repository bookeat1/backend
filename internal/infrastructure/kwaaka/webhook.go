package kwaaka

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"backend-core/internal/domain"
)

// This file is the ONLY place that knows the JSON shape of Kwaaka's webhooks
// (decision 24.09). When Kwaaka changes or extends the format, edit this file
// and its fixtures, nothing else. Status strings are stored raw; the mapping to
// a coarse state is MapPosStatus (posstatus.go).

// ErrWebhookUnparseable marks a body that is not a usable status message.
var ErrWebhookUnparseable = errors.New("kwaaka webhook: unparseable body")

type orderStatusBody struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
}

// ParseOrderStatus reads {"orderId": "...", "status": "..."}.
func ParseOrderStatus(body []byte) (domain.KwaakaOrderStatusEvent, error) {
	var b orderStatusBody
	if err := json.Unmarshal(body, &b); err != nil {
		return domain.KwaakaOrderStatusEvent{}, fmt.Errorf("%w: %v", ErrWebhookUnparseable, err)
	}
	b.OrderID, b.Status = strings.TrimSpace(b.OrderID), strings.TrimSpace(b.Status)
	if b.OrderID == "" || b.Status == "" {
		return domain.KwaakaOrderStatusEvent{}, fmt.Errorf("%w: orderId and status are required", ErrWebhookUnparseable)
	}
	return domain.KwaakaOrderStatusEvent{OrderID: b.OrderID, Raw: b.Status, State: MapPosStatus(b.Status)}, nil
}

type reserveStatusBody struct {
	ReserveID string `json:"reserveId"`
	Status    string `json:"status"`
}

// ParseReserveStatus reads {"reserveId": "...", "status": "..."}. Phase 2 does
// not send reservations to the POS, so the result is only used to validate and
// log; nothing is applied.
func ParseReserveStatus(body []byte) (reserveID, status string, err error) {
	var b reserveStatusBody
	if err := json.Unmarshal(body, &b); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrWebhookUnparseable, err)
	}
	return b.ReserveID, b.Status, nil
}

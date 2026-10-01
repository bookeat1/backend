package kwaaka

import (
	"errors"
	"testing"

	"backend-core/internal/domain"
)

func TestParseOrderStatus(t *testing.T) {
	ev, err := ParseOrderStatus([]byte(`{"orderId":"K-1","status":"CLOSED","extra":1}`))
	if err != nil || ev.OrderID != "K-1" || ev.Raw != "CLOSED" || ev.State != domain.PosStateClosed {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseOrderStatus([]byte(`{"orderId":"K-2","status":"strange"}`))
	if err != nil || ev.State != domain.PosStateUnknown || ev.Raw != "strange" {
		t.Fatalf("unknown status must be kept raw: %+v %v", ev, err)
	}
	for _, bad := range []string{``, `garbage`, `{}`, `{"orderId":"x"}`, `{"status":"open"}`, `[]`, `{"orderId":1,"status":"open"}`} {
		if _, err := ParseOrderStatus([]byte(bad)); !errors.Is(err, ErrWebhookUnparseable) {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}

func TestParseReserveStatus(t *testing.T) {
	id, st, err := ParseReserveStatus([]byte(`{"reserveId":"R","status":"confirmed"}`))
	if err != nil || id != "R" || st != "confirmed" {
		t.Fatal(id, st, err)
	}
	if _, _, err := ParseReserveStatus([]byte(`nope`)); !errors.Is(err, ErrWebhookUnparseable) {
		t.Fatal(err)
	}
}

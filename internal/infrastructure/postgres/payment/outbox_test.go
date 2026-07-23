package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

func TestOutboxCreateExistsAndMarkPublished(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	outbox := NewOutbox(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	e := &domain.PaymentOutboxEvent{PaymentID: p.ID, EventType: domain.EventPaymentCaptured}
	if err := outbox.Create(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}

	exists, err := outbox.ExistsForPayment(ctx, p.ID, domain.EventPaymentCaptured)
	if err != nil || !exists {
		t.Fatalf("exists for payment = %v, err=%v, want true", exists, err)
	}
	exists, err = outbox.ExistsForPayment(ctx, p.ID, domain.EventPaymentRefunded)
	if err != nil || exists {
		t.Fatalf("exists for a different event type = %v, err=%v, want false", exists, err)
	}

	at := time.Now()
	if err := outbox.MarkPublished(ctx, []uuid.UUID{e.ID}, at); err != nil {
		t.Fatalf("mark published: %v", err)
	}
}

func TestOutboxClaimUnpublishedSkipLocked(t *testing.T) {
	pool, ctx := setup(t)
	rid := seedRestaurant(t, pool)
	bid := seedBooking(t, pool, rid)
	payments := New(pool)
	outbox := NewOutbox(pool)
	txm := sqltx.NewManager(pool)
	p := newCapturedPayment(t, ctx, payments, bid, rid)

	e1 := &domain.PaymentOutboxEvent{PaymentID: p.ID, EventType: domain.EventPaymentCaptured}
	if err := outbox.Create(ctx, e1); err != nil {
		t.Fatalf("create e1: %v", err)
	}
	e2 := &domain.PaymentOutboxEvent{PaymentID: p.ID, EventType: domain.EventPaymentSettled}
	if err := outbox.Create(ctx, e2); err != nil {
		t.Fatalf("create e2: %v", err)
	}
	if err := outbox.MarkPublished(ctx, []uuid.UUID{e2.ID}, time.Now()); err != nil {
		t.Fatalf("mark e2 published: %v", err)
	}

	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := outbox.ClaimUnpublished(ctx, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != e1.ID {
			t.Errorf("claim unpublished = %d rows, want only e1", len(due))
		}

		locked, err := outbox.ClaimUnpublished(context.Background(), 10)
		if err != nil {
			return err
		}
		if len(locked) != 0 {
			t.Errorf("second worker claimed %d locked rows, want 0", len(locked))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("claim tx: %v", err)
	}
}

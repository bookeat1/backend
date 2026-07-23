package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/infrastructure/sqltx"
)

func newEvent(provider domain.PaymentProvider, providerEventID string) *domain.PaymentEvent {
	return &domain.PaymentEvent{
		ID: uuid.New(), Provider: provider, ProviderEventID: providerEventID,
		Payload: []byte(`{"ok":true}`), SignatureValid: true,
	}
}

func TestEventCRUD(t *testing.T) {
	pool, ctx := setup(t)
	events := NewEvents(pool)

	e := newEvent(domain.ProviderFreedomPay, "evt-1")
	if err := events.Create(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "evt-1")
	if err != nil || got.ID != e.ID || !got.SignatureValid {
		t.Fatalf("roundtrip mismatch: %+v, err=%v", got, err)
	}
	if _, err := events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "unknown"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get(missing) = %v, want ErrNotFound", err)
	}

	if err := events.RecordProcessingError(ctx, e.ID, "apply failed: temp error"); err != nil {
		t.Fatalf("record processing error: %v", err)
	}
	got, _ = events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "evt-1")
	if got.ProcessedAt != nil {
		t.Fatal("RecordProcessingError must not close the event")
	}
	if got.ProcessError == nil || *got.ProcessError != "apply failed: temp error" {
		t.Fatalf("process error not recorded: %+v", got)
	}

	// MarkProcessed on success (empty processErr) must NOT erase the earlier
	// error text — it is the audit trail of "failed once, then succeeded".
	if err := events.MarkProcessed(ctx, e.ID, time.Now(), ""); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	got, _ = events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "evt-1")
	if got.ProcessedAt == nil {
		t.Fatal("mark processed did not stamp processed_at")
	}
	if got.ProcessError == nil || *got.ProcessError != "apply failed: temp error" {
		t.Fatalf("mark processed erased the earlier error text: %+v", got)
	}

	if err := events.SetPaymentID(ctx, e.ID, uuid.New()); err != nil {
		t.Fatalf("set payment id: %v", err)
	}
	got, _ = events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "evt-1")
	if got.PaymentID == nil {
		t.Fatal("set payment id did not persist")
	}
}

// TestEventDuplicateProviderEventIDConflicts proves
// idx_payment_events_provider_event is translated into domain.ErrAlreadyExists
// — an acquirer redelivering the same callback must never create a second
// row, which is the entire idempotency mechanism for webhooks (spec §7).
func TestEventDuplicateProviderEventIDConflicts(t *testing.T) {
	pool, ctx := setup(t)
	events := NewEvents(pool)

	e1 := newEvent(domain.ProviderFreedomPay, "evt-dup")
	if err := events.Create(ctx, e1); err != nil {
		t.Fatalf("create e1: %v", err)
	}
	e2 := newEvent(domain.ProviderFreedomPay, "evt-dup") // redelivery
	e2.Payload = []byte(`{"ok":true,"redelivered":true}`)
	if err := events.Create(ctx, e2); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("create e2 (redelivered event) = %v, want ErrAlreadyExists", err)
	}
	// The original row must be exactly what is stored — no dup, no overwrite.
	got, err := events.GetByProviderEventID(ctx, domain.ProviderFreedomPay, "evt-dup")
	if err != nil || got.ID != e1.ID {
		t.Fatalf("stored event is not e1: %+v, err=%v", got, err)
	}
}

func TestEventClaimUnprocessedSkipLocked(t *testing.T) {
	pool, ctx := setup(t)
	events := NewEvents(pool)
	txm := sqltx.NewManager(pool)

	e1 := newEvent(domain.ProviderFreedomPay, "evt-unproc-1")
	if err := events.Create(ctx, e1); err != nil {
		t.Fatalf("create e1: %v", err)
	}
	e2 := newEvent(domain.ProviderFreedomPay, "evt-unproc-2")
	if err := events.Create(ctx, e2); err != nil {
		t.Fatalf("create e2: %v", err)
	}
	if err := events.MarkProcessed(ctx, e2.ID, time.Now(), ""); err != nil {
		t.Fatalf("mark e2 processed: %v", err)
	}

	err := txm.WithinTx(ctx, func(ctx context.Context) error {
		due, err := events.ClaimUnprocessed(ctx, 10)
		if err != nil {
			return err
		}
		if len(due) != 1 || due[0].ID != e1.ID {
			t.Errorf("claim unprocessed = %d rows, want only e1", len(due))
		}

		locked, err := events.ClaimUnprocessed(context.Background(), 10)
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

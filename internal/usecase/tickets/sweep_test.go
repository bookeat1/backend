package tickets

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeSweepPayments struct{ byID map[uuid.UUID]*domain.Payment }

func (f *fakeSweepPayments) GetByID(_ context.Context, id uuid.UUID) (*domain.Payment, error) {
	if p, ok := f.byID[id]; ok {
		return p, nil
	}
	return nil, domain.ErrNotFound
}

func newSweeper(repo *fakeTicketRepo, pays *fakeSweepPayments) *PendingSweeper {
	return NewPendingSweeper(repo, pays, SweepConfig{StaleAfter: time.Hour, BatchSize: 100},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestSweepReleasesPendingWithNoPayment: a stale pending ticket with NO payment
// row (the crash/SetPaymentID-failure case) is cancelled and its seat freed —
// the payments reconciler cannot see it because there is no payment.
func TestSweepReleasesPendingWithNoPayment(t *testing.T) {
	repo := newFakeTicketRepo()
	eventID := uuid.New()
	stale := &domain.EventTicket{
		ID: uuid.New(), EventID: eventID, Status: domain.TicketPending, Quantity: 2,
		CreatedAt: time.Now().Add(-2 * time.Hour), // older than StaleAfter
	}
	repo.byID[stale.ID] = stale

	if sold, _ := repo.SoldCount(context.Background(), eventID); sold != 2 {
		t.Fatalf("precondition sold = %d, want 2", sold)
	}
	sw := newSweeper(repo, &fakeSweepPayments{byID: map[uuid.UUID]*domain.Payment{}})
	n, err := sw.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("released = %d, want 1", n)
	}
	if repo.byID[stale.ID].Status != domain.TicketCancelled {
		t.Fatalf("ticket status = %s, want cancelled", repo.byID[stale.ID].Status)
	}
	if sold, _ := repo.SoldCount(context.Background(), eventID); sold != 0 {
		t.Fatalf("seat not freed: sold = %d, want 0", sold)
	}
}

// TestSweepLeavesLiveHold: a stale pending ticket whose payment is still a live
// hold (authorized) is NOT swept — cancelling under a capture that may still
// land would leave a paid guest with a cancelled ticket.
func TestSweepLeavesLiveHold(t *testing.T) {
	repo := newFakeTicketRepo()
	pid := uuid.New()
	stale := &domain.EventTicket{
		ID: uuid.New(), EventID: uuid.New(), Status: domain.TicketPending, Quantity: 1,
		CreatedAt: time.Now().Add(-2 * time.Hour), PaymentID: &pid,
	}
	repo.byID[stale.ID] = stale
	pays := &fakeSweepPayments{byID: map[uuid.UUID]*domain.Payment{
		pid: {ID: pid, Status: domain.PaymentAuthorized},
	}}
	sw := newSweeper(repo, pays)
	n, err := sw.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 || repo.byID[stale.ID].Status != domain.TicketPending {
		t.Fatalf("live-hold ticket should be left pending: n=%d status=%s", n, repo.byID[stale.ID].Status)
	}
}

// TestSweepProjectsCaptured: a stale pending ticket whose payment is actually
// captured (the observer missed it) is projected to paid, not cancelled.
func TestSweepProjectsCaptured(t *testing.T) {
	repo := newFakeTicketRepo()
	pid := uuid.New()
	stale := &domain.EventTicket{
		ID: uuid.New(), EventID: uuid.New(), Status: domain.TicketPending, Quantity: 1,
		CreatedAt: time.Now().Add(-2 * time.Hour), PaymentID: &pid,
	}
	repo.byID[stale.ID] = stale
	pays := &fakeSweepPayments{byID: map[uuid.UUID]*domain.Payment{
		pid: {ID: pid, Status: domain.PaymentCaptured},
	}}
	sw := newSweeper(repo, pays)
	if _, err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if repo.byID[stale.ID].Status != domain.TicketPaid {
		t.Fatalf("status = %s, want paid", repo.byID[stale.ID].Status)
	}
}

package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func TestStatusUseCase_GuestSeesOwnPayment(t *testing.T) {
	owner := uuid.New()
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	p.UserID = &owner
	repo := newFakePaymentRepo(p)
	u := NewStatusUseCase(repo)

	got, err := u.Get(context.Background(), Actor{UserID: &owner, Role: domain.RoleUser}, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("got payment %s, want %s", got.ID, p.ID)
	}
}

func TestStatusUseCase_AnotherGuestIsRejectedAsNotFound(t *testing.T) {
	owner := uuid.New()
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1")
	p.UserID = &owner
	repo := newFakePaymentRepo(p)
	u := NewStatusUseCase(repo)

	stranger := uuid.New()
	_, err := u.Get(context.Background(), Actor{UserID: &stranger, Role: domain.RoleUser}, p.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound (no enumeration oracle)", err)
	}
}

func TestStatusUseCase_StaffSeesAnyPayment(t *testing.T) {
	owner := uuid.New()
	p := testPayment(uuid.New(), domain.PaymentCaptured, "gw-1")
	p.UserID = &owner
	repo := newFakePaymentRepo(p)
	u := NewStatusUseCase(repo)

	got, err := u.GetForBooking(context.Background(), staffActor, p.BookingID)
	if err != nil {
		t.Fatalf("GetForBooking() error = %v", err)
	}
	if got.Status != domain.PaymentCaptured {
		t.Fatalf("status = %s, want captured", got.Status)
	}
}

func TestStatusUseCase_GuestCheckoutWithNoAccountIsReadable(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentAuthorized, "gw-1") // UserID nil
	repo := newFakePaymentRepo(p)
	u := NewStatusUseCase(repo)

	got, err := u.Get(context.Background(), Actor{}, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("got payment %s, want %s", got.ID, p.ID)
	}
}

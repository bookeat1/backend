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
	u := NewStatusUseCase(repo, newFakeManagerChecker())

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
	u := NewStatusUseCase(repo, newFakeManagerChecker())

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
	u := NewStatusUseCase(repo, newFakeManagerChecker())

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
	u := NewStatusUseCase(repo, newFakeManagerChecker())

	got, err := u.Get(context.Background(), Actor{}, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("got payment %s, want %s", got.ID, p.ID)
	}
}

// TestStatusUseCase_CrossTenantStaffIsRejected is report item #13: staff of
// a DIFFERENT restaurant must not be able to read this payment merely by
// knowing its booking id.
func TestStatusUseCase_CrossTenantStaffIsRejected(t *testing.T) {
	p := testPayment(uuid.New(), domain.PaymentCaptured, "gw-1")
	repo := newFakePaymentRepo(p)
	strangerStaff := uuid.New()
	managers := &fakeManagerChecker{managed: map[uuid.UUID]map[uuid.UUID]bool{}, allowAllByDefault: false}
	managers.set(strangerStaff, p.RestaurantID, false)
	u := NewStatusUseCase(repo, managers)

	_, err := u.Get(context.Background(), Actor{UserID: &strangerStaff, Role: domain.RoleRestaurant}, p.ID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden (staff of a different restaurant)", err)
	}
}

package restaurants

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The bug this whole method exists for: before it, whatsapp_opt_in and
// whatsapp_phone could ONLY be written by Assign — i.e. at the moment the staff
// row was created. Every venue whose owner predates the WhatsApp channel had no
// way to switch alerts on at all, which is why all nine live venues were sitting
// at (false, NULL).
func TestSetWhatsAppTurnsTheChannelOnForAnExistingRow(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	m, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		OptIn: boolp(true), Phone: strp("8 701 000 00 01"),
	})
	if err != nil {
		t.Fatalf("SetWhatsApp: %v", err)
	}
	if !m.WhatsappOptIn {
		t.Error("opt-in was not stored")
	}
	// Normalized to E.164 — the same discipline as the venue-level number, so
	// "8 701…" and "+7701…" can never become two different targets.
	if m.WhatsappPhone == nil || *m.WhatsappPhone != "+77010000001" {
		t.Fatalf("phone = %v, want the E.164 form", m.WhatsappPhone)
	}
	// And it actually reached the repository, not just the returned struct.
	if got := fm.rows[0]; !got.WhatsappOptIn || got.WhatsappPhone == nil || *got.WhatsappPhone != "+77010000001" {
		t.Fatalf("stored row = %+v — the write was lost", got)
	}
}

// A sole owner does not outrank themselves, so the SetRole rank guard would
// lock them out of their own settings. Consent to be messaged on a personal
// number is personal: the row's own user may always set it.
func TestSetWhatsAppOwnerMaySetTheirOwnRow(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	if _, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		OptIn: boolp(true), Phone: strp("+77010000001"),
	}); err != nil {
		t.Fatalf("an owner must be able to switch on their own alerts: %v", err)
	}
}

// A hostess may set her own number without holding staff.manage.
func TestSetWhatsAppStaffMaySetTheirOwnRow(t *testing.T) {
	rid, uid := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: uid, Role: domain.StaffRoleHostess},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	if _, err := u.SetWhatsApp(context.Background(), ownerActor(uid), mid, SetWhatsAppInput{
		OptIn: boolp(true), Phone: strp("+77010000009"),
	}); err != nil {
		t.Fatalf("staff must be able to set their own number: %v", err)
	}
}

// …but nobody may redirect SOMEONE ELSE's alerts to their own phone without
// outranking them. This is the lateral-takeover guard SetRole carries.
func TestSetWhatsAppRejectsALateralTakeover(t *testing.T) {
	rid, ownerA, ownerB := uuid.New(), uuid.New(), uuid.New()
	midB := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerA, Role: domain.StaffRoleOwner},
		{ID: midB, RestaurantID: rid, UserID: ownerB, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	_, err := u.SetWhatsApp(context.Background(), ownerActor(ownerA), midB, SetWhatsAppInput{
		Phone: strp("+77010000002"),
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden — a co-owner's alerts are not yours to redirect", err)
	}
}

// A stranger to the venue cannot touch its roster at all.
func TestSetWhatsAppRejectsAnOutsider(t *testing.T) {
	rid := uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleManager},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	_, err := u.SetWhatsApp(context.Background(), guestActor(uuid.New()), mid, SetWhatsAppInput{
		OptIn: boolp(true), Phone: strp("+77010000003"),
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// Consent that ends up ON must end up with a number. The alternative — opt_in
// true with no phone — reads as "switched on" in the cabinet and delivers
// nothing, which is exactly the failure this feature exists to fix.
func TestSetWhatsAppRejectsConsentWithoutANumber(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	if _, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		OptIn: boolp(true),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	// The same trap the other way round: clearing the number while consent
	// stays on.
	fm.rows[0].WhatsappOptIn = true
	fm.rows[0].WhatsappPhone = strp("+77010000001")
	if _, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		Phone: strp(""),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation when the number is cleared under a live consent", err)
	}
}

func TestSetWhatsAppRejectsAMalformedNumber(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	for _, bad := range []string{"7701", "abc", "+7 701 00"} {
		if _, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
			OptIn: boolp(true), Phone: strp(bad),
		}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("phone %q: err = %v, want ErrValidation", bad, err)
		}
	}
}

// Switching the channel off keeps the number (the venue may want it back) but
// must actually silence the alerts.
func TestSetWhatsAppOptOutKeepsTheNumber(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner,
			WhatsappOptIn: true, WhatsappPhone: strp("+77010000001")},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	m, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		OptIn: boolp(false),
	})
	if err != nil {
		t.Fatalf("SetWhatsApp: %v", err)
	}
	if m.WhatsappOptIn {
		t.Error("opt-out was not applied")
	}
	if m.WhatsappPhone == nil || *m.WhatsappPhone != "+77010000001" {
		t.Errorf("phone = %v, want it kept", m.WhatsappPhone)
	}
}

// Clearing both at once is the "forget my number" path and must be allowed.
func TestSetWhatsAppClearsBoth(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	mid := uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: mid, RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner,
			WhatsappOptIn: true, WhatsappPhone: strp("+77010000001")},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	m, err := u.SetWhatsApp(context.Background(), ownerActor(ownerID), mid, SetWhatsAppInput{
		OptIn: boolp(false), Phone: strp(""),
	})
	if err != nil {
		t.Fatalf("SetWhatsApp: %v", err)
	}
	if m.WhatsappOptIn || m.WhatsappPhone != nil {
		t.Fatalf("row = %+v, want both cleared", m)
	}
}

func TestSetWhatsAppMissingRow(t *testing.T) {
	fm := &fakeManagers{}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})
	if _, err := u.SetWhatsApp(context.Background(), adminActor(), uuid.New(), SetWhatsAppInput{
		OptIn: boolp(false),
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Assign normalizes too: one person written "8 701…" on one row and "+7701…"
// on another would be two targets, and the notifier would message the same
// handset twice.
func TestAssignNormalizesWhatsAppPhone(t *testing.T) {
	rid, ownerID := uuid.New(), uuid.New()
	fm := &fakeManagers{rows: []domain.RestaurantManager{
		{ID: uuid.New(), RestaurantID: rid, UserID: ownerID, Role: domain.StaffRoleOwner},
	}}
	u := NewManagerUseCase(fm, &fakeUsers{}, &inlineTx{})

	m, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleManager,
		WhatsappOptIn: true, WhatsappPhone: strp("8 701 000 00 01"),
	})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if m.WhatsappPhone == nil || *m.WhatsappPhone != "+77010000001" {
		t.Fatalf("phone = %v, want the E.164 form", m.WhatsappPhone)
	}

	// A malformed number is refused at creation as well as at update.
	if _, err := u.Assign(context.Background(), ownerActor(ownerID), AssignManagerInput{
		RestaurantID: rid, UserID: uuid.New(), Role: domain.StaffRoleManager,
		WhatsappPhone: strp("7701"),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

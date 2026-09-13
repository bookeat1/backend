package promocodes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeAdminPromos answers for EVERY promo it knows, whatever its status — the
// cabinet's reader, as opposed to the guest-facing one that hides everything
// that is not live.
type fakeAdminPromos struct{ byID map[uuid.UUID]domain.Promo }

func newFakeAdminPromos(items ...domain.Promo) *fakeAdminPromos {
	f := &fakeAdminPromos{byID: map[uuid.UUID]domain.Promo{}}
	for _, p := range items {
		f.byID[p.ID] = p
	}
	return f
}

func (f *fakeAdminPromos) GetByID(_ context.Context, id uuid.UUID) (*domain.Promo, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("get promo: %w", domain.ErrNotFound)
	}
	return &p, nil
}

func adminFixture(usage domain.PromoCodeUsage) (*fakeCodes, domain.PromoCode, domain.Promo, Editor) {
	promo := domain.Promo{
		ID: uuid.New(), Title: "Алматы марафон", Status: domain.PromoHidden,
		EndsAt: time.Now().Add(30 * 24 * time.Hour),
	}
	total := 100
	code := domain.PromoCode{
		ID: uuid.New(), Code: "MARATHON26", PromotionID: promo.ID,
		StartsAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour),
		MaxUsesTotal: &total, MaxUsesPerUser: 1, Status: domain.PromoCodeActive,
	}
	codes := newFakeCodes(code)
	e := NewEditor(codes, newFakeAdminPromos(promo), fakeUsage{usage: usage})
	return codes, code, promo, e
}

// The listing must show the campaign's REAL status: a live code in front of a
// hidden campaign is the silent failure this column exists to make visible.
func TestAdminListShowsActivationsAndTheRealPromoStatus(t *testing.T) {
	_, code, promo, e := adminFixture(domain.PromoCodeUsage{DistinctUsers: 3})

	items, err := e.List(context.Background(), domain.PromoCodeFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	got := items[0]
	if got.Code.ID != code.ID {
		t.Errorf("code id = %v, want %v", got.Code.ID, code.ID)
	}
	if got.Activations != 3 {
		t.Errorf("activations = %d, want 3 (counted from the bookings)", got.Activations)
	}
	if got.PromoStatus != domain.PromoHidden || got.PromoTitle != promo.Title {
		t.Errorf("promo = %q/%q, want the real hidden campaign", got.PromoTitle, got.PromoStatus)
	}
	if got.PromoMissing {
		t.Error("promo_missing = true for a campaign that exists")
	}
}

// Deleting a code somebody already redeemed would rewrite history: the booking
// keeps the id and the string, and the participant list is counted from those
// rows. Archiving is the supported retirement.
func TestAdminDeleteRefusesAnActivatedCode(t *testing.T) {
	codes, code, _, e := adminFixture(domain.PromoCodeUsage{DistinctUsers: 1})

	err := e.Delete(context.Background(), code.ID)
	if got, _ := domain.CodeOf(err); got != domain.CodePromoCodeActivated {
		t.Fatalf("code = %q, want %q (err %v)", got, domain.CodePromoCodeActivated, err)
	}
	if _, err := codes.GetByID(context.Background(), code.ID); err != nil {
		t.Errorf("the code must still be there: %v", err)
	}
}

func TestAdminDeleteRemovesAnUnusedCode(t *testing.T) {
	codes, code, _, e := adminFixture(domain.PromoCodeUsage{})

	if err := e.Delete(context.Background(), code.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := codes.GetByID(context.Background(), code.ID); err == nil {
		t.Error("the code is still there after a delete")
	}
}

// Pause/resume is the whole reason this is a PATCH: one field moves and
// nothing else in the row is touched.
func TestAdminPatchPausesWithoutTouchingAnythingElse(t *testing.T) {
	_, code, _, e := adminFixture(domain.PromoCodeUsage{DistinctUsers: 2})
	paused := domain.PromoCodePaused

	got, err := e.Patch(context.Background(), code.ID, PatchPromoCodeInput{Status: &paused})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got.Code.Status != domain.PromoCodePaused {
		t.Errorf("status = %q, want paused", got.Code.Status)
	}
	if got.Code.Code != code.Code || got.Code.MaxUsesPerUser != code.MaxUsesPerUser ||
		!got.Code.ExpiresAt.Equal(code.ExpiresAt) || got.Code.MaxUsesTotal == nil ||
		*got.Code.MaxUsesTotal != *code.MaxUsesTotal {
		t.Errorf("patch rewrote fields it was not given: %+v", got.Code)
	}
	if got.Activations != 2 {
		t.Errorf("activations = %d, want the count to survive a patch", got.Activations)
	}
}

// An archived code is retired for good; "resume" on it must be a refusal with
// its own code, not a silently ignored no-op.
func TestAdminPatchRefusesAnImpossibleTransition(t *testing.T) {
	codes, code, _, e := adminFixture(domain.PromoCodeUsage{})
	archived := domain.PromoCodeArchived
	if _, err := e.Patch(context.Background(), code.ID, PatchPromoCodeInput{Status: &archived}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	active := domain.PromoCodeActive
	_, err := e.Patch(context.Background(), code.ID, PatchPromoCodeInput{Status: &active})
	if got, _ := domain.CodeOf(err); got != domain.CodePromoCodeBadTransition {
		t.Fatalf("code = %q, want %q (err %v)", got, domain.CodePromoCodeBadTransition, err)
	}
	stored, _ := codes.GetByID(context.Background(), code.ID)
	if stored.Status != domain.PromoCodeArchived {
		t.Errorf("status = %q, want it to stay archived", stored.Status)
	}
}

// Re-sending the SAME code string is how a cabinet form that posts every field
// behaves and must not be an error; a different one is refused loudly, because
// the repository would ignore it silently.
func TestAdminPatchOnTheCodeString(t *testing.T) {
	_, code, _, e := adminFixture(domain.PromoCodeUsage{DistinctUsers: 1})

	same := " marathon-26 "
	if _, err := e.Patch(context.Background(), code.ID, PatchPromoCodeInput{Code: &same}); err != nil {
		t.Fatalf("re-sending the same code (any spelling) must be a no-op: %v", err)
	}

	other := "MARATHON27"
	_, err := e.Patch(context.Background(), code.ID, PatchPromoCodeInput{Code: &other})
	if got, _ := domain.CodeOf(err); got != domain.CodePromoCodeActivated {
		t.Fatalf("code = %q, want %q (err %v)", got, domain.CodePromoCodeActivated, err)
	}
}

// Create normalizes the guest-facing string and refuses a shape the database's
// CHECK would refuse too, before Postgres sees it.
func TestAdminCreateNormalizesAndValidates(t *testing.T) {
	_, _, promo, e := adminFixture(domain.PromoCodeUsage{})
	in := CreatePromoCodeInput{
		Code: " new-code 26 ", PromotionID: promo.ID,
		StartsAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	actor := uuid.New()
	got, err := e.Create(context.Background(), in, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Code.Code != "NEWCODE26" {
		t.Errorf("code = %q, want the normalized form", got.Code.Code)
	}
	if got.Code.Status != domain.PromoCodeDraft {
		t.Errorf("status = %q, want draft — a half-filled form must not start accepting guests", got.Code.Status)
	}
	if got.Code.MaxUsesPerUser != 1 {
		t.Errorf("max_uses_per_user = %d, want the default 1", got.Code.MaxUsesPerUser)
	}
	if got.Code.CreatedBy == nil || *got.Code.CreatedBy != actor {
		t.Errorf("created_by = %v, want %v", got.Code.CreatedBy, actor)
	}

	bad := in
	bad.Code = "ab"
	if _, err := e.Create(context.Background(), bad, actor); err == nil {
		t.Error("a two-character code must be refused")
	}
	dup := in
	dup.Code = "newcode26"
	if _, err := e.Create(context.Background(), dup, actor); err == nil {
		t.Error("a duplicate code must be refused")
	}
}

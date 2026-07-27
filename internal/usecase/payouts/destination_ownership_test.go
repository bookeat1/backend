package payouts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// The hole these tests close: nothing between "a card handle was typed into the
// venue cabinet" and "money left the platform" ever asked whether that card is
// the one registered to the venue being paid. The payout froze whatever
// destination it was handed at generation and dispatched it later, unchecked.
//
// Every test below asserts the same two things about a refusal, because both
// matter and only one of them is visible in the status: the acquirer was NOT
// called (no money moved), and the claimed ledger entries were released (the
// money is owed again and payable to the right card tomorrow).

// owedOneEntry seeds a single unpaid ledger entry for a venue and returns it.
func owedOneEntry(h *harness, rid uuid.UUID, amount int64) uuid.UUID {
	entry := uuid.New()
	h.owed.byRestaurant[rid] = []domain.OwedBalance{{
		RestaurantID: rid, Currency: domain.CurrencyKZT, AmountMinor: amount,
		Entries: []domain.OwedEntry{{LedgerEntryID: entry, AmountSignedMinor: amount, Currency: domain.CurrencyKZT}},
	}}
	return entry
}

// TestGenerate_RefusesACardRegisteredToAnotherVenue is the hostile case: the
// destination read for this venue turns out to carry another restaurant's id
// (an IDOR, a mis-scoped query, a hand-written repair row). No payout may be
// created against it — a payout row is an ADDRESS for money, and writing one
// against somebody else's card is the whole theft.
func TestGenerate_RefusesACardRegisteredToAnotherVenue(t *testing.T) {
	ctx := context.Background()
	rid, otherVenue := uuid.New(), uuid.New()
	h := newHarness()
	// Stored under rid's key, but the row itself belongs to another venue.
	if err := h.dest.Upsert(ctx, &domain.PayoutDestination{
		RestaurantID: otherVenue, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
		ProviderCustomerRef: "fp-user-other",
	}); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	h.dest.byRestaurant[rid] = h.dest.byRestaurant[otherVenue]
	entry := owedOneEntry(h, rid, 500_000)

	_, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err == nil {
		t.Fatal("a payout must not be generated against another venue's card")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodePayoutDestinationMismatch {
		t.Fatalf("expected %q, got %v", domain.CodePayoutDestinationMismatch, err)
	}
	if _, claimed := h.items.byEntry[entry]; claimed {
		t.Fatal("a refused generation must not claim the venue's ledger entries")
	}
}

// TestGenerate_RefusesACardWithoutProviderUserID: a tokenized card is addressed
// by the PAIR (provider user id, token). Without the user id the token does not
// say whose card it is, and the acquirer adapter refuses it — but only AFTER
// the payout has been claimed into `sent`, which used to strand the payout and
// send the reconciler asking about an order that was never dispatched.
func TestGenerate_RefusesACardWithoutProviderUserID(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	if err := h.dest.Upsert(ctx, &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
	}); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	entry := owedOneEntry(h, rid, 500_000)

	_, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if code, _ := domain.CodeOf(err); code != domain.CodePayoutDestinationIncomplete {
		t.Fatalf("expected %q, got %v", domain.CodePayoutDestinationIncomplete, err)
	}
	if _, claimed := h.items.byEntry[entry]; claimed {
		t.Fatal("a refused generation must not claim the venue's ledger entries")
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("the acquirer must not be called, got %d calls", h.gw.payoutCalls)
	}
}

// TestSendPayout_RefusesACardTheVenueNoLongerOwns is the window this check
// exists for: the payout was generated while one card was on file, the venue
// then replaced it (a manager's card swapped back to the owner's), and the
// frozen snapshot now addresses a card nobody at that venue owns. The money
// must not go there — and must not be silently re-pointed at the new card
// either, because automatic re-addressing is exactly what an attacker wants.
func TestSendPayout_RefusesACardTheVenueNoLongerOwns(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	entry := owedOneEntry(h, rid, 500_000)

	created, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err != nil || len(created) != 1 {
		t.Fatalf("generate: %v (n=%d)", err, len(created))
	}
	id := created[0].ID

	// The venue replaces its payout card between generation and dispatch.
	newToken := uuid.NewString()
	if err := h.dest.Upsert(ctx, &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: newToken,
		ProviderCustomerRef: "fp-user-owner",
	}); err != nil {
		t.Fatalf("replace destination: %v", err)
	}

	_, err = h.uc.SendPayout(ctx, superadmin(), id)
	if err == nil {
		t.Fatal("a payout addressing a card the venue no longer owns must be refused")
	}
	if code, _ := domain.CodeOf(err); code != domain.CodePayoutDestinationMismatch {
		t.Fatalf("expected %q, got %v", domain.CodePayoutDestinationMismatch, err)
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("the acquirer must never be called for a refused payout, got %d calls", h.gw.payoutCalls)
	}
	got, err := h.payouts.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("read payout: %v", err)
	}
	if got.Status != domain.PayoutFailed {
		t.Fatalf("a refused payout must be failed, not left claimable; got %s", got.Status)
	}
	if got.DestinationToken == newToken {
		t.Fatal("the payout must not be re-pointed at the new card")
	}
	if _, claimed := h.items.byEntry[entry]; claimed {
		t.Fatal("the money must be owed again so the next generation pays the correct card")
	}
}

// TestSendPayout_RefusesACardThatIsAttachedToNobody: the venue has no
// destination at all any more (removed, or the venue was re-created). A payout
// generated earlier still carries a live card handle — and it must not be paid.
func TestSendPayout_RefusesACardThatIsAttachedToNobody(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	entry := owedOneEntry(h, rid, 500_000)

	created, err := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	if err != nil || len(created) != 1 {
		t.Fatalf("generate: %v (n=%d)", err, len(created))
	}
	id := created[0].ID
	delete(h.dest.byRestaurant, rid)

	_, err = h.uc.SendPayout(ctx, superadmin(), id)
	if code, _ := domain.CodeOf(err); code != domain.CodePayoutDestinationMissing {
		t.Fatalf("expected %q, got %v", domain.CodePayoutDestinationMissing, err)
	}
	if h.gw.payoutCalls != 0 {
		t.Fatalf("the acquirer must never be called for a refused payout, got %d calls", h.gw.payoutCalls)
	}
	got, _ := h.payouts.GetByID(ctx, id)
	if got.Status != domain.PayoutFailed {
		t.Fatalf("a refused payout must be failed; got %s", got.Status)
	}
	if _, claimed := h.items.byEntry[entry]; claimed {
		t.Fatal("the money must be owed again once the venue registers a card")
	}
}

// TestSendPayout_RefusalNeverNamesTheCard: the refusal text is stored in
// failure_reason and written to logs. The card token is the address of the
// money and must never appear there.
func TestSendPayout_RefusalNeverNamesTheCard(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	seedDestination(h, rid)
	owedOneEntry(h, rid, 500_000)

	created, _ := h.uc.GenerateForRestaurant(ctx, superadmin(), rid)
	id := created[0].ID
	stale, _ := h.payouts.GetByID(ctx, id)
	staleToken := stale.DestinationToken

	if err := h.dest.Upsert(ctx, &domain.PayoutDestination{
		RestaurantID: rid, Provider: domain.ProviderFreedomPay,
		Method: domain.PayoutMethodFreedomPayCardToken, Token: uuid.NewString(),
		ProviderCustomerRef: "fp-user-owner",
	}); err != nil {
		t.Fatalf("replace destination: %v", err)
	}
	_, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), staleToken) {
		t.Fatalf("the refusal must not name the card token: %q", err.Error())
	}
	got, _ := h.payouts.GetByID(ctx, id)
	if got.FailureReason != nil && strings.Contains(*got.FailureReason, staleToken) {
		t.Fatalf("failure_reason must not name the card token: %q", *got.FailureReason)
	}
	if got.FailureCode == nil || *got.FailureCode != "destination_unverified" {
		t.Fatalf("expected failure_code destination_unverified, got %v", got.FailureCode)
	}
}

// TestSendPayout_AlreadyPaidIsNotReverifiedIntoAFailure guards the check
// against becoming destructive: a payout that has already reached a terminal
// state is returned as-is, exactly as before, and must not be failed by a later
// destination change.
func TestSendPayout_AlreadyPaidIsNotReverifiedIntoAFailure(t *testing.T) {
	ctx := context.Background()
	rid := uuid.New()
	h := newHarness()
	id := uuid.New()
	if err := h.payouts.Create(ctx, &domain.Payout{
		ID: id, RestaurantID: rid, AmountMinor: 5000, Currency: domain.CurrencyKZT,
		Status: domain.PayoutPaid, Method: domain.PayoutMethodFreedomPayCardToken,
		DestinationToken: uuid.NewString(), DestinationCustomerRef: "fp-user-1",
		IdempotencyKey: "payout:" + id.String(), StatusChangedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed payout: %v", err)
	}
	got, err := h.uc.SendPayout(ctx, superadmin(), id)
	if err != nil {
		t.Fatalf("a paid payout must be returned as-is: %v", err)
	}
	if got.Status != domain.PayoutPaid {
		t.Fatalf("expected paid, got %s", got.Status)
	}
}

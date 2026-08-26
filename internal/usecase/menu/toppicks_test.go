package menu

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// dish builds one menu item of rid. `slot` nil means "the venue did not mark
// it"; that is the state every existing venue is in.
func dish(rid uuid.UUID, name string, available bool, slot *int) domain.MenuItem {
	return domain.MenuItem{
		ID: uuid.New(), RestaurantID: rid, Name: name, Price: "1000.00",
		IsAvailable: available, TopPickPosition: slot,
	}
}

// seed puts the items into the fake in the order the repository would return
// them (display_order NULLS LAST, name) and makes them addressable by id.
func seed(items ...domain.MenuItem) *fakeItems {
	f := newFakeItems()
	f.list = items
	for i := range items {
		m := items[i]
		f.store[m.ID] = &m
	}
	return f
}

func names(items []domain.MenuItem) []string {
	out := make([]string, 0, len(items))
	for _, m := range items {
		out = append(out, m.Name)
	}
	return out
}

// The whole point of the feature: what the venue marked comes FIRST, in the
// venue's own order, and the derivation that used to be the entire rail is
// demoted to filling the tail.
func TestHighlightsPutTheVenuesOwnPicksAheadOfTheDerivedOnes(t *testing.T) {
	rid := uuid.New()
	items := seed(
		dish(rid, "первое по display_order", true, nil),
		dish(rid, "второе по display_order", true, nil),
		dish(rid, "отмечено вторым", true, intp(2)),
		dish(rid, "отмечено первым", true, intp(1)),
	)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	got, err := f.ListHighlights(context.Background(), rid, nil, 4)
	if err != nil {
		t.Fatalf("list highlights: %v", err)
	}
	want := []string{"отмечено первым", "отмечено вторым", "первое по display_order", "второе по display_order"}
	if diff := names(got); !equal(diff, want) {
		t.Fatalf("rail order:\n got %v\nwant %v", diff, want)
	}
}

// Unmarking must hand the rail back to the old rule exactly — no residue, no
// "used to be picked" ordering. This is the guarantee that every venue which
// never touches the panel keeps today's behaviour.
func TestHighlightsFallBackToTheDerivedListWhenNothingIsMarked(t *testing.T) {
	rid := uuid.New()
	a := dish(rid, "первое по display_order", true, nil)
	b := dish(rid, "второе по display_order", true, nil)
	marked := dish(rid, "второе по display_order, но отмечено", true, intp(1))
	items := seed(a, b, marked)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	ctx := context.Background()

	withMark, err := f.ListHighlights(ctx, rid, nil, 8)
	if err != nil {
		t.Fatalf("list highlights: %v", err)
	}
	if withMark[0].Name != marked.Name {
		t.Fatalf("precondition: marked dish must lead, got %v", names(withMark))
	}

	if err := f.SetTopPick(ctx, rid, marked.ID, false); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	// The repository's listing reflects the write; the fake's `list` is a
	// snapshot, so refresh it from the store the same way a re-read would.
	items.list = []domain.MenuItem{*items.store[a.ID], *items.store[b.ID], *items.store[marked.ID]}

	after, err := f.ListHighlights(ctx, rid, nil, 8)
	if err != nil {
		t.Fatalf("list highlights: %v", err)
	}
	want := []string{a.Name, b.Name, marked.Name}
	if got := names(after); !equal(got, want) {
		t.Fatalf("after unmarking the rail must be the plain derived list:\n got %v\nwant %v", got, want)
	}
}

// A dish the kitchen has stopped must leave the rail even though it keeps its
// slot in the database — otherwise the storefront's "best of" advertises
// something nobody can order.
func TestHighlightsExcludeAMarkedDishThatIsUnavailable(t *testing.T) {
	rid := uuid.New()
	stopped := dish(rid, "отмечено, но в стоп-листе", false, intp(1))
	items := seed(
		stopped,
		dish(rid, "отмечено и доступно", true, intp(2)),
		dish(rid, "не отмечено, доступно", true, nil),
	)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	got, err := f.ListHighlights(context.Background(), rid, nil, 8)
	if err != nil {
		t.Fatalf("list highlights: %v", err)
	}
	for _, m := range got {
		if m.ID == stopped.ID {
			t.Fatalf("a stop-listed dish must not appear in the rail, got %v", names(got))
		}
	}
	want := []string{"отмечено и доступно", "не отмечено, доступно"}
	if names := names(got); !equal(names, want) {
		t.Fatalf("rail:\n got %v\nwant %v", names, want)
	}
}

// The rail is short by design; limit is clamped in the usecase so neither an
// omitted value nor ?limit=5000 reaches the read.
func TestHighlightsClampTheRailSize(t *testing.T) {
	rid := uuid.New()
	many := make([]domain.MenuItem, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, dish(rid, "блюдо", true, nil))
	}
	f := NewFacade(seed(many...), &fakeCategories{}, &inlineTx{})
	ctx := context.Background()

	for _, tc := range []struct{ in, want int }{
		{0, highlightLimitDefault},
		{-3, highlightLimitDefault},
		{5, 5},
		{5000, highlightLimitMax},
	} {
		got, err := f.ListHighlights(ctx, rid, nil, tc.in)
		if err != nil {
			t.Fatalf("limit %d: %v", tc.in, err)
		}
		if len(got) != tc.want {
			t.Fatalf("limit %d: want %d dishes, got %d", tc.in, tc.want, len(got))
		}
	}
}

// The cap is a product decision (8 = what the rail renders) and it must be
// reported with its own code so the panel can explain it.
func TestSetTopPickRefusesToOverfillTheRail(t *testing.T) {
	rid := uuid.New()
	all := make([]domain.MenuItem, 0, domain.MenuTopPickLimit+1)
	for i := 0; i < domain.MenuTopPickLimit; i++ {
		all = append(all, dish(rid, "уже отмечено", true, intp(i+1)))
	}
	extra := dish(rid, "девятое", true, nil)
	all = append(all, extra)
	f := NewFacade(seed(all...), &fakeCategories{}, &inlineTx{})

	err := f.SetTopPick(context.Background(), rid, extra.ID, true)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	code, ok := domain.CodeOf(err)
	if !ok || code != domain.CodeMenuTopPicksLimit {
		t.Fatalf("want code %q, got %q (present=%v)", domain.CodeMenuTopPicksLimit, code, ok)
	}
}

// Marking takes the LOWEST free slot, so a rail with a hole (someone unmarked
// slot 2) is refilled instead of growing at the end.
func TestSetTopPickTakesTheLowestFreeSlot(t *testing.T) {
	rid := uuid.New()
	fresh := dish(rid, "новое", true, nil)
	items := seed(
		dish(rid, "слот 1", true, intp(1)),
		dish(rid, "слот 3", true, intp(3)),
		fresh,
	)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.SetTopPick(context.Background(), rid, fresh.ID, true); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got := items.store[fresh.ID].TopPickPosition
	if got == nil || *got != 2 {
		t.Fatalf("want slot 2, got %v", got)
	}
}

// Re-marking an already marked dish must not move it: the venue ordered its
// rail deliberately and a double tap in the panel is not a re-order.
func TestSetTopPickIsIdempotentForAnAlreadyMarkedDish(t *testing.T) {
	rid := uuid.New()
	marked := dish(rid, "уже второе", true, intp(2))
	items := seed(dish(rid, "первое", true, intp(1)), marked)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.SetTopPick(context.Background(), rid, marked.ID, true); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if got := items.store[marked.ID].TopPickPosition; got == nil || *got != 2 {
		t.Fatalf("re-marking moved the dish: slot %v", got)
	}
}

// Two managers marking at the same instant compute the same free slot; the
// database rejects the loser. The loser must retry, not surface a 409 the panel
// cannot explain.
func TestSetTopPickRetriesWhenItLosesTheSlotRace(t *testing.T) {
	rid := uuid.New()
	fresh := dish(rid, "новое", true, nil)
	items := seed(dish(rid, "слот 1", true, intp(1)), fresh)
	items.slotErrOnce = domain.ErrAlreadyExists
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.SetTopPick(context.Background(), rid, fresh.ID, true); err != nil {
		t.Fatalf("a lost slot race must be retried, got %v", err)
	}
	if got := items.store[fresh.ID].TopPickPosition; got == nil || *got != 2 {
		t.Fatalf("want slot 2 after the retry, got %v", got)
	}
}

// A dish of another venue is ErrNotFound, never a promotion: the tenant guard
// lives in SQL and the usecase must not paper over it.
func TestSetTopPickRefusesAnotherVenuesDish(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	foreign := dish(theirs, "чужое блюдо", true, nil)
	items := seed(dish(mine, "моё", true, nil), foreign)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.SetTopPick(context.Background(), mine, foreign.ID, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if items.store[foreign.ID].TopPickPosition != nil {
		t.Fatal("another venue's dish was marked")
	}
}

// ReplaceTopPicks is what a drag-and-drop editor calls: the list IS the order.
func TestReplaceTopPicksWritesTheGivenOrder(t *testing.T) {
	rid := uuid.New()
	a, b, c := dish(rid, "a", true, intp(1)), dish(rid, "b", true, nil), dish(rid, "c", true, nil)
	items := seed(a, b, c)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.ReplaceTopPicks(context.Background(), rid, []uuid.UUID{c.ID, a.ID}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := items.store[c.ID].TopPickPosition; got == nil || *got != 1 {
		t.Fatalf("c must be slot 1, got %v", got)
	}
	if got := items.store[a.ID].TopPickPosition; got == nil || *got != 2 {
		t.Fatalf("a must be slot 2, got %v", got)
	}
	if items.store[b.ID].TopPickPosition != nil {
		t.Fatal("b was never named and must stay unmarked")
	}
	if items.cleared != 1 {
		t.Fatalf("the old arrangement must be cleared exactly once, got %d", items.cleared)
	}
}

// An empty (but present) list is how the panel says "снять все": the venue goes
// back to the derived rail.
func TestReplaceTopPicksWithAnEmptyListClearsTheRail(t *testing.T) {
	rid := uuid.New()
	a := dish(rid, "a", true, intp(1))
	items := seed(a)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})

	if err := f.ReplaceTopPicks(context.Background(), rid, nil); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if items.store[a.ID].TopPickPosition != nil {
		t.Fatal("the rail was not cleared")
	}
}

func TestReplaceTopPicksRejectsOverfillAndDuplicates(t *testing.T) {
	rid := uuid.New()
	a := dish(rid, "a", true, nil)
	items := seed(a)
	f := NewFacade(items, &fakeCategories{}, &inlineTx{})
	ctx := context.Background()

	tooMany := make([]uuid.UUID, domain.MenuTopPickLimit+1)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	err := f.ReplaceTopPicks(ctx, rid, tooMany)
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeMenuTopPicksLimit {
		t.Fatalf("overfill: want code %q, got %q (err %v)", domain.CodeMenuTopPicksLimit, code, err)
	}

	if err := f.ReplaceTopPicks(ctx, rid, []uuid.UUID{a.ID, a.ID}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("duplicate: want ErrValidation, got %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

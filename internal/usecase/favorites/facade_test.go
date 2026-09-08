package favorites

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeFavoriteRepo struct {
	addCalls    []domain.Favorite
	removeCalls []domain.Favorite
	addErr      error
	listItems   []domain.RestaurantListItem
	set         map[uuid.UUID]bool

	restaurantItems []domain.FavoriteRestaurantItem
	eventItems      []domain.FavoriteEventItem
	promoItems      []domain.FavoritePromoItem
	// listNow records the instant ListAll evaluated visibility windows at.
	listNow      time.Time
	addEvents    []uuid.UUID
	removeEvents []uuid.UUID
	addPromos    []uuid.UUID
	removePromos []uuid.UUID
	addEventErr  error
}

func (f *fakeFavoriteRepo) Add(_ context.Context, userID, restaurantID uuid.UUID) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.addCalls = append(f.addCalls, domain.Favorite{UserID: userID, RestaurantID: restaurantID})
	return nil
}

func (f *fakeFavoriteRepo) Remove(_ context.Context, userID, restaurantID uuid.UUID) error {
	f.removeCalls = append(f.removeCalls, domain.Favorite{UserID: userID, RestaurantID: restaurantID})
	return nil
}

func (f *fakeFavoriteRepo) ListByUser(_ context.Context, _ uuid.UUID) ([]domain.RestaurantListItem, error) {
	return f.listItems, nil
}

func (f *fakeFavoriteRepo) FavoriteSet(_ context.Context, _ uuid.UUID, _ []uuid.UUID) (map[uuid.UUID]bool, error) {
	return f.set, nil
}

func (f *fakeFavoriteRepo) ListRestaurantsByUser(_ context.Context, _ uuid.UUID) ([]domain.FavoriteRestaurantItem, error) {
	return f.restaurantItems, nil
}

func (f *fakeFavoriteRepo) AddEvent(_ context.Context, _, eventID uuid.UUID) error {
	if f.addEventErr != nil {
		return f.addEventErr
	}
	f.addEvents = append(f.addEvents, eventID)
	return nil
}

func (f *fakeFavoriteRepo) RemoveEvent(_ context.Context, _, eventID uuid.UUID) error {
	f.removeEvents = append(f.removeEvents, eventID)
	return nil
}

func (f *fakeFavoriteRepo) ListEventsByUser(_ context.Context, _ uuid.UUID, now time.Time) ([]domain.FavoriteEventItem, error) {
	f.listNow = now
	return f.eventItems, nil
}

func (f *fakeFavoriteRepo) AddPromo(_ context.Context, _, promoID uuid.UUID) error {
	f.addPromos = append(f.addPromos, promoID)
	return nil
}

func (f *fakeFavoriteRepo) RemovePromo(_ context.Context, _, promoID uuid.UUID) error {
	f.removePromos = append(f.removePromos, promoID)
	return nil
}

func (f *fakeFavoriteRepo) ListPromosByUser(_ context.Context, _ uuid.UUID, _ time.Time) ([]domain.FavoritePromoItem, error) {
	return f.promoItems, nil
}

// The facade must pass ITS clock down to the visibility windows, not
// time.Now(): a test that pins the clock and a production caller that does not
// must both read the same code path.
func TestListAll_UsesTheInjectedClock(t *testing.T) {
	pinned := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	repo := &fakeFavoriteRepo{}
	f := NewFacade(repo, WithClock(func() time.Time { return pinned }))

	if _, err := f.ListAll(context.Background(), uuid.New()); err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if !repo.listNow.Equal(pinned) {
		t.Fatalf("events read at %s, want the pinned %s", repo.listNow, pinned)
	}
}

func TestAddEvent_PropagatesNotFound(t *testing.T) {
	repo := &fakeFavoriteRepo{addEventErr: domain.ErrNotFound}
	f := NewFacade(repo)

	if err := f.AddEvent(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("AddEvent error = %v, want ErrNotFound", err)
	}
}

func TestAdd(t *testing.T) {
	repo := &fakeFavoriteRepo{}
	f := NewFacade(repo)
	uid, rid := uuid.New(), uuid.New()

	if err := f.Add(context.Background(), uid, rid); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(repo.addCalls) != 1 || repo.addCalls[0].RestaurantID != rid {
		t.Fatalf("expected one Add call for %s, got %+v", rid, repo.addCalls)
	}
}

func TestAdd_PropagatesNotFound(t *testing.T) {
	repo := &fakeFavoriteRepo{addErr: domain.ErrNotFound}
	f := NewFacade(repo)

	err := f.Add(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemove_Idempotent(t *testing.T) {
	repo := &fakeFavoriteRepo{}
	f := NewFacade(repo)
	uid, rid := uuid.New(), uuid.New()

	// Calling Remove twice on something never favorited must not error either
	// time — the repo fake never returns an error from Remove, matching the
	// real repository's idempotent DELETE.
	if err := f.Remove(context.Background(), uid, rid); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := f.Remove(context.Background(), uid, rid); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if len(repo.removeCalls) != 2 {
		t.Fatalf("expected 2 Remove calls, got %d", len(repo.removeCalls))
	}
}

func TestList(t *testing.T) {
	rid := uuid.New()
	repo := &fakeFavoriteRepo{listItems: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{ID: rid, Name: "Test"}},
	}}
	f := NewFacade(repo)

	items, err := f.List(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Restaurant.ID != rid {
		t.Fatalf("unexpected list result: %+v", items)
	}
}

func TestFavoriteSet(t *testing.T) {
	rid := uuid.New()
	repo := &fakeFavoriteRepo{set: map[uuid.UUID]bool{rid: true}}
	f := NewFacade(repo)

	set, err := f.FavoriteSet(context.Background(), uuid.New(), []uuid.UUID{rid})
	if err != nil {
		t.Fatalf("FavoriteSet: %v", err)
	}
	if !set[rid] {
		t.Fatalf("expected %s to be favorited", rid)
	}
}

// fakeAttacher stands in for the shared usecase/restaurants.VenueState.
type fakeAttacher struct {
	calls int
	state domain.PublicVenueState
}

func (f *fakeAttacher) AttachList(_ context.Context, items []domain.RestaurantListItem) {
	f.calls++
	for i := range items {
		st := f.state
		items[i].VenueState = &st
	}
}

// Favorites renders the same public shape as the catalog listing, so it must go
// through the same enrichment. Without this the same venue shows a full
// schedule on the home screen and none in favorites — a difference with no
// cause in the data. See transport/rest/favorites' cross-screen integration
// test for the end-to-end version of this contract.
func TestList_AttachesVenueState(t *testing.T) {
	rid := uuid.New()
	repo := &fakeFavoriteRepo{listItems: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{ID: rid}},
	}}
	att := &fakeAttacher{state: domain.PublicVenueState{
		AcceptsOnlineBookings: true,
		Schedule:              &domain.WeeklySchedule{Timezone: "Asia/Almaty"},
	}}
	f := NewFacade(repo, WithVenueState(att))

	items, err := f.List(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if att.calls != 1 {
		t.Errorf("enricher called %d times, want exactly 1 (one batch for the page)", att.calls)
	}
	if len(items) != 1 || items[0].VenueState == nil {
		t.Fatalf("favorited venue must carry the venue state, got %+v", items)
	}
	if !items[0].VenueState.AcceptsOnlineBookings || items[0].VenueState.Schedule == nil {
		t.Errorf("venue state = %+v, want the enricher's value verbatim", items[0].VenueState)
	}
}

// Unwired, favorites behaves exactly as before: fields absent, never guessed.
func TestList_WithoutEnricherLeavesVenueStateAbsent(t *testing.T) {
	repo := &fakeFavoriteRepo{listItems: []domain.RestaurantListItem{
		{Restaurant: domain.Restaurant{ID: uuid.New()}},
	}}
	f := NewFacade(repo)

	items, err := f.List(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if items[0].VenueState != nil {
		t.Error("venue state must stay absent when the enrichment is not wired")
	}
}

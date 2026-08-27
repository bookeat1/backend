package stories

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// superadmin is the actor these tests act as: RBAC is not what they are about
// (facade_test covers it), and RoleAdmin short-circuits the permission check, so
// the fake checker never has to be populated.
func superadmin() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

// The public read must hand the repository the FACADE's clock. Filtering only
// in the mobile app was the alternative, and it is the one this asserts against:
// whoever calls the endpoint — the app, the cabinet, a partner — gets the same
// answer because the same instant reaches the same SQL predicate.
func TestListPassesTheFacadeClockToTheRepository(t *testing.T) {
	repo := newFakeRepo()
	rid := uuid.New()
	fixed := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	alive := &domain.Story{ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/alive.jpg", IsActive: true}
	past := fixed.Add(-time.Hour)
	stale := &domain.Story{ID: uuid.New(), RestaurantID: rid, ImageURL: "https://cdn/stale.jpg", IsActive: true, ExpiresAt: &past}
	repo.byID[alive.ID] = alive
	repo.byID[stale.ID] = stale

	f := NewFacade(repo, &fakePerms{})
	f.(*facade).clock = func() time.Time { return fixed }

	got, err := f.List(context.Background(), rid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !repo.publicNow.Equal(fixed) {
		t.Errorf("repository received now = %v, want the facade clock %v", repo.publicNow, fixed)
	}
	if len(got) != 1 || got[0].ID != alive.ID {
		t.Fatalf("guest list = %+v, want only the unexpired card", got)
	}

	// The cabinet read of the SAME data still shows both, expired included.
	admin, err := f.ListForAdmin(context.Background(), superadmin(), rid)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(admin) != 2 {
		t.Fatalf("cabinet list len = %d, want 2 (the expired card stays visible to the venue)", len(admin))
	}
}

// expires_at over the wire is a *string with THREE states, the convention this
// package already uses for caption and action_url, because JSON cannot otherwise
// tell "leave it alone" from "clear it".
func TestUpdateExpiresAtThreeStates(t *testing.T) {
	rid := uuid.New()
	deadline := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	later := "2026-09-01T12:00:00Z"

	newStory := func(repo *fakeStoryRepo) *domain.Story {
		exp := deadline
		s := &domain.Story{
			ID: uuid.New(), RestaurantID: rid,
			ImageURL: "https://cdn/a.jpg", IsActive: true, ExpiresAt: &exp,
		}
		repo.byID[s.ID] = s
		return s
	}

	t.Run("omitted leaves the stored expiry untouched", func(t *testing.T) {
		repo := newFakeRepo()
		s := newStory(repo)
		f := NewFacade(repo, &fakePerms{})
		caption := "Устрицы"
		got, err := f.UpdateStory(context.Background(), superadmin(), s.ID, UpdateInput{Caption: &caption})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(deadline) {
			t.Fatalf("expires_at = %v, want it unchanged at %v", got.ExpiresAt, deadline)
		}
	})

	t.Run("a new instant extends it", func(t *testing.T) {
		repo := newFakeRepo()
		s := newStory(repo)
		f := NewFacade(repo, &fakePerms{})
		got, err := f.UpdateStory(context.Background(), superadmin(), s.ID, UpdateInput{ExpiresAt: &later})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		want, _ := time.Parse(time.RFC3339, later)
		if got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) {
			t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, want)
		}
	})

	t.Run("an empty string clears it — the story becomes permanent again", func(t *testing.T) {
		repo := newFakeRepo()
		s := newStory(repo)
		f := NewFacade(repo, &fakePerms{})
		blank := "  "
		got, err := f.UpdateStory(context.Background(), superadmin(), s.ID, UpdateInput{ExpiresAt: &blank})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if got.ExpiresAt != nil {
			t.Fatalf("expires_at = %v, want nil (cleared)", *got.ExpiresAt)
		}
	})

	t.Run("garbage is a 422, not a silently ignored field", func(t *testing.T) {
		repo := newFakeRepo()
		s := newStory(repo)
		f := NewFacade(repo, &fakePerms{})
		bad := "завтра в обед"
		_, err := f.UpdateStory(context.Background(), superadmin(), s.ID, UpdateInput{ExpiresAt: &bad})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
	})

	t.Run("a past instant is accepted: it is how an expired card is re-saved", func(t *testing.T) {
		repo := newFakeRepo()
		s := newStory(repo)
		f := NewFacade(repo, &fakePerms{})
		past := "2020-01-01T00:00:00Z"
		got, err := f.UpdateStory(context.Background(), superadmin(), s.ID, UpdateInput{ExpiresAt: &past})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if got.ExpiresAt == nil || !got.IsExpired(time.Now()) {
			t.Fatalf("expires_at = %v, want a stored past instant", got.ExpiresAt)
		}
	})
}

// Creating without expires_at must keep producing a PERMANENT story. The 24-hour
// default is the cabinet's suggestion in its form, deliberately not a rule of
// the API: an integration that has never heard of expiry must not start writing
// stories that vanish a day later.
func TestCreateWithoutExpiresAtIsPermanent(t *testing.T) {
	repo := newFakeRepo()
	f := NewFacade(repo, &fakePerms{})
	got, err := f.CreateStory(context.Background(), superadmin(), CreateInput{
		RestaurantID: uuid.New(),
		ImageURL:     "https://cdn/a.jpg",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Fatalf("expires_at = %v, want nil (no expiry unless asked for)", *got.ExpiresAt)
	}

	iso := "2026-08-28T12:00:00Z"
	withExpiry, err := f.CreateStory(context.Background(), superadmin(), CreateInput{
		RestaurantID: uuid.New(),
		ImageURL:     "https://cdn/b.jpg",
		ExpiresAt:    &iso,
	})
	if err != nil {
		t.Fatalf("create with expiry: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, iso)
	if withExpiry.ExpiresAt == nil || !withExpiry.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", withExpiry.ExpiresAt, want)
	}
}

// The nil expiry is the state every pre-0088 row carries, so IsExpired must
// never call one expired — that single wrong answer would empty the platform's
// story rails on deploy day.
func TestStoryIsExpired(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Second)
	cases := []struct {
		name string
		exp  *time.Time
		want bool
	}{
		{"no expiry is never expired", nil, false},
		{"a past instant is expired", &past, true},
		{"a future instant is not", &future, false},
		{"exactly now is expired (the SQL predicate is strictly >)", &now, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := domain.Story{ExpiresAt: tc.exp}
			if got := s.IsExpired(now); got != tc.want {
				t.Errorf("IsExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

package campaign

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- fake repository ---------------------------------------------------------

type fakeLinks struct {
	bySlug  map[string]*domain.CampaignLink
	findErr error

	hits   []hit
	hitErr error
}

type hit struct {
	linkID   uuid.UUID
	platform string
}

func (f *fakeLinks) FindBySlug(_ context.Context, slug string) (*domain.CampaignLink, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.bySlug[slug], nil
}

func (f *fakeLinks) RecordHit(_ context.Context, linkID uuid.UUID, platform string) error {
	if f.hitErr != nil {
		return f.hitErr
	}
	f.hits = append(f.hits, hit{linkID, platform})
	return nil
}

func TestResolveUnknownSlugIsNotFoundAndNoHit(t *testing.T) {
	repo := &fakeLinks{bySlug: map[string]*domain.CampaignLink{}}
	f := NewFacade(repo)

	outcome, link, err := f.Resolve(context.Background(), "nope", "some-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != OutcomeNotFound {
		t.Fatalf("outcome = %v, want OutcomeNotFound", outcome)
	}
	if link != nil {
		t.Fatalf("link = %+v, want nil", link)
	}
	if len(repo.hits) != 0 {
		t.Fatalf("hits recorded = %d, want 0 for an unknown slug", len(repo.hits))
	}
}

func TestResolveDisabledLinkIsDisabledAndNoHit(t *testing.T) {
	linkID := uuid.New()
	repo := &fakeLinks{bySlug: map[string]*domain.CampaignLink{
		"am26-flyer": {ID: linkID, Slug: "am26-flyer", Status: domain.CampaignLinkDisabled},
	}}
	f := NewFacade(repo)

	outcome, link, err := f.Resolve(context.Background(), "am26-flyer", "some-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != OutcomeDisabled {
		t.Fatalf("outcome = %v, want OutcomeDisabled", outcome)
	}
	if link == nil || link.ID != linkID {
		t.Fatalf("link = %+v, want the disabled link", link)
	}
	if len(repo.hits) != 0 {
		t.Fatalf("hits recorded = %d, want 0 for a disabled slug", len(repo.hits))
	}
}

func TestResolveActiveLinkRecordsHitWithPlatform(t *testing.T) {
	linkID := uuid.New()
	repo := &fakeLinks{bySlug: map[string]*domain.CampaignLink{
		"am26-flyer": {ID: linkID, Slug: "am26-flyer", Status: domain.CampaignLinkActive,
			TargetURL: "https://bookeat.godetour.link/lQ9BPpUvJc"},
	}}
	f := NewFacade(repo)

	outcome, link, err := f.Resolve(context.Background(), "am26-flyer",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != OutcomeActive {
		t.Fatalf("outcome = %v, want OutcomeActive", outcome)
	}
	if link == nil || link.ID != linkID {
		t.Fatalf("link = %+v, want the active link", link)
	}
	if len(repo.hits) != 1 {
		t.Fatalf("hits recorded = %d, want 1", len(repo.hits))
	}
	if repo.hits[0].linkID != linkID || repo.hits[0].platform != "android" {
		t.Fatalf("hit = %+v, want {%s android}", repo.hits[0], linkID)
	}
}

func TestResolvePropagatesFindError(t *testing.T) {
	repo := &fakeLinks{findErr: errors.New("db down")}
	f := NewFacade(repo)

	if _, _, err := f.Resolve(context.Background(), "am26-flyer", "ua"); err == nil {
		t.Fatal("expected an error to propagate")
	}
}

func TestPlatformFromUserAgent(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)", "ios"},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X)", "ios"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit", "android"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "other"},
		{"", "other"},
	}
	for _, c := range cases {
		if got := PlatformFromUserAgent(c.ua); got != c.want {
			t.Errorf("PlatformFromUserAgent(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}

package venuedashboard

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeTodayRepo struct {
	gotVenue                    uuid.UUID
	gotNow                      time.Time
	gotAwaitingLim, gotTodayLim int
	calls                       int
}

func (f *fakeTodayRepo) Today(_ context.Context, rid uuid.UUID, now time.Time,
	awaitingLimit, todayLimit int) (domain.VenueToday, error) {
	f.gotVenue, f.gotNow = rid, now
	f.gotAwaitingLim, f.gotTodayLim = awaitingLimit, todayLimit
	f.calls++
	return domain.VenueToday{}, nil
}

func newTodayUC(repo TodayRepo) *TodayUseCase {
	u := NewTodayUseCase(repo)
	u.now = fixedNow
	return u
}

// A panel that opens with no query string must still get a screenful, not
// everything the venue ever booked.
func TestOmittedLimitsBecomeTheDefaults(t *testing.T) {
	repo := &fakeTodayRepo{}
	venue := uuid.New()

	if _, err := newTodayUC(repo).Today(context.Background(), venue, 0, 0); err != nil {
		t.Fatalf("today: %v", err)
	}
	if repo.gotAwaitingLim != defaultAwaitingLimit || repo.gotTodayLim != defaultTodayLimit {
		t.Fatalf("limits = %d/%d, want the defaults %d/%d",
			repo.gotAwaitingLim, repo.gotTodayLim, defaultAwaitingLimit, defaultTodayLimit)
	}
	if repo.gotVenue != venue {
		t.Fatal("the venue must be passed through untouched")
	}
	if repo.gotNow != fixedNow() {
		t.Fatalf("now = %v, want the usecase clock", repo.gotNow)
	}
}

// A negative limit is a caller mistake that must not reach SQL as `LIMIT -1`,
// and an oversized one must not turn a dashboard tile into a full export.
func TestLimitsAreClampedNotObeyed(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		awaiting, today      int
		wantAwait, wantToday int
	}{
		{"negative falls back to the default", -5, -1, defaultAwaitingLimit, defaultTodayLimit},
		{"a sane request is honoured", 5, 7, 5, 7},
		{"an oversized request is capped", 10_000, 10_000, maxAwaitingLimit, maxTodayLimit},
		{"exactly the ceiling is allowed", maxAwaitingLimit, maxTodayLimit, maxAwaitingLimit, maxTodayLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeTodayRepo{}
			if _, err := newTodayUC(repo).Today(context.Background(), uuid.New(), tc.awaiting, tc.today); err != nil {
				t.Fatalf("today: %v", err)
			}
			if repo.gotAwaitingLim != tc.wantAwait || repo.gotTodayLim != tc.wantToday {
				t.Fatalf("limits = %d/%d, want %d/%d",
					repo.gotAwaitingLim, repo.gotTodayLim, tc.wantAwait, tc.wantToday)
			}
		})
	}
}

// Both lists and every waiting time must be measured against ONE instant. Read
// twice, a booking could be "today" in one half of the answer and not in the
// other across a midnight, and the screen would contradict itself.
func TestTheClockIsReadOnceForTheWholeScreen(t *testing.T) {
	repo := &fakeTodayRepo{}
	u := NewTodayUseCase(repo)
	var reads int
	u.now = func() time.Time {
		reads++
		return fixedNow().Add(time.Duration(reads) * time.Hour)
	}

	if _, err := u.Today(context.Background(), uuid.New(), 0, 0); err != nil {
		t.Fatalf("today: %v", err)
	}
	if reads != 1 {
		t.Fatalf("the clock was read %d times, want exactly 1", reads)
	}
	if repo.calls != 1 {
		t.Fatalf("the read model was called %d times, want exactly 1", repo.calls)
	}
}

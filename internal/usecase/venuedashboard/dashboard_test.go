package venuedashboard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

type fakeRepo struct {
	gotVenue    uuid.UUID
	gotFrom     time.Time
	gotTo       time.Time
	summaryCall int
}

func (f *fakeRepo) Summary(_ context.Context, rid uuid.UUID, from, to time.Time) (domain.VenueDashboard, error) {
	f.gotVenue, f.gotFrom, f.gotTo = rid, from, to
	f.summaryCall++
	return domain.VenueDashboard{From: from, To: to}, nil
}

func (f *fakeRepo) Load(_ context.Context, rid uuid.UUID, from, to time.Time) ([]domain.VenueLoadSlot, error) {
	f.gotVenue, f.gotFrom, f.gotTo = rid, from, to
	return nil, nil
}

func fixedNow() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }

func newUC(repo Repo) *UseCase {
	u := NewUseCase(repo)
	u.now = fixedNow
	return u
}

// A screen that opens with no filters must still show something sensible, so an
// omitted period becomes the default look-back rather than "everything" (which
// is a table scan) or "nothing" (which is an empty dashboard).
func TestOmittedPeriodBecomesTheDefaultLookBack(t *testing.T) {
	repo := &fakeRepo{}
	venue := uuid.New()

	if _, err := newUC(repo).Summary(context.Background(), venue, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if repo.gotTo != fixedNow() {
		t.Fatalf("to = %v, want now", repo.gotTo)
	}
	if want := fixedNow().Add(-defaultLookback); repo.gotFrom != want {
		t.Fatalf("from = %v, want %v", repo.gotFrom, want)
	}
	if repo.gotVenue != venue {
		t.Fatal("the venue must be passed through untouched")
	}
}

// Only `from` given: `to` still defaults to now, so "since the first of the
// month" works without the caller having to name today's date.
func TestOnlyFromGivenStillEndsNow(t *testing.T) {
	repo := &fakeRepo{}
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if _, err := newUC(repo).Summary(context.Background(), uuid.New(), from, time.Time{}); err != nil {
		t.Fatalf("summary: %v", err)
	}
	if repo.gotFrom != from || repo.gotTo != fixedNow() {
		t.Fatalf("period = %v..%v", repo.gotFrom, repo.gotTo)
	}
}

// A backwards or empty window is a caller mistake, and it must not reach SQL:
// the query would return an empty result and the screen would silently show
// zeros as if the venue had no bookings.
func TestBackwardsPeriodIsRefused(t *testing.T) {
	repo := &fakeRepo{}
	to := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	from := to.Add(24 * time.Hour)

	_, err := newUC(repo).Summary(context.Background(), uuid.New(), from, to)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if repo.summaryCall != 0 {
		t.Fatal("an invalid period must never reach the database")
	}
}

func TestEqualBoundsAreRefused(t *testing.T) {
	repo := &fakeRepo{}
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	if _, err := newUC(repo).Summary(context.Background(), uuid.New(), at, at); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an empty window must be refused, got %v", err)
	}
}

// The ceiling is what keeps a dashboard from turning into a full-history scan
// because someone typed 2015 into a date field.
func TestPeriodLongerThanAYearIsRefused(t *testing.T) {
	repo := &fakeRepo{}
	to := fixedNow()
	from := to.Add(-maxLookback - time.Hour)

	_, err := newUC(repo).Summary(context.Background(), uuid.New(), from, to)

	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
	if repo.summaryCall != 0 {
		t.Fatal("an oversized period must never reach the database")
	}
}

// Load shares the period rules with Summary; if they ever drift, one half of
// the screen would show a different window than the other.
func TestLoadSharesThePeriodRules(t *testing.T) {
	repo := &fakeRepo{}

	if _, err := newUC(repo).Load(context.Background(), uuid.New(), time.Time{}, time.Time{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := fixedNow().Add(-defaultLookback); repo.gotFrom != want {
		t.Fatalf("from = %v, want %v", repo.gotFrom, want)
	}

	to := fixedNow()
	if _, err := newUC(repo).Load(context.Background(), uuid.New(), to.Add(time.Hour), to); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("load must refuse a backwards period, got %v", err)
	}
}

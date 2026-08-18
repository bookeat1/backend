package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeSkips records the tombstones the facade writes for a generated
// occurrence, so a test can prove the generator will never refill that slot.
type fakeSkips struct {
	recorded []struct {
		recurrenceID uuid.UUID
		slot         time.Time
	}
	err error
}

func (f *fakeSkips) RecordSkip(_ context.Context, recurrenceID uuid.UUID, slot time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.recorded = append(f.recorded, struct {
		recurrenceID uuid.UUID
		slot         time.Time
	}{recurrenceID, slot})
	return nil
}

func seedOccurrence(repo *fakeEventRepo, rid uuid.UUID, recurrenceID *uuid.UUID, startsAt time.Time) *domain.Event {
	e := &domain.Event{
		ID:           uuid.New(),
		RestaurantID: rid,
		Title:        "Караоке-битва",
		StartsAt:     startsAt,
		EndsAt:       startsAt.Add(3 * time.Hour),
		Status:       domain.EventPublished,
		RecurrenceID: recurrenceID,
	}
	repo.byID[e.ID] = e
	return e
}

// Deleting a GENERATED occurrence must tombstone its slot: the row disappears,
// the slot becomes free, and without the tombstone the next generator pass
// recreates the date the venue just removed.
func TestDeleteGeneratedOccurrenceRecordsSkip(t *testing.T) {
	rid, actorID, recurrenceID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	skips := &fakeSkips{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{}, WithOccurrenceSkips(skips))
	starts := time.Date(2026, time.August, 26, 19, 0, 0, 0, time.UTC)
	e := seedOccurrence(repo, rid, &recurrenceID, starts)

	if err := f.Delete(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(skips.recorded) != 1 {
		t.Fatalf("want exactly one tombstone, got %d", len(skips.recorded))
	}
	if skips.recorded[0].recurrenceID != recurrenceID || !skips.recorded[0].slot.Equal(starts) {
		t.Fatalf("tombstone points at the wrong slot: %+v", skips.recorded[0])
	}
}

// An ordinary one-off event has no rule behind it, so there is nothing to
// tombstone — and writing a skip for a nil recurrence would be a nil deref.
func TestDeleteOneOffEventRecordsNoSkip(t *testing.T) {
	rid, actorID := uuid.New(), uuid.New()
	repo := newFakeRepo()
	skips := &fakeSkips{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{}, WithOccurrenceSkips(skips))
	e := seedOccurrence(repo, rid, nil, time.Now().Add(48*time.Hour))

	if err := f.Delete(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(skips.recorded) != 0 {
		t.Fatalf("a one-off event must leave no tombstone, got %+v", skips.recorded)
	}
}

// If the tombstone cannot be written, the delete must NOT proceed: an
// occurrence that is gone without a tombstone comes back on the next pass, and
// the venue would have to delete it again every few minutes.
func TestDeleteGeneratedOccurrenceFailsWhenSkipFails(t *testing.T) {
	rid, actorID, recurrenceID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	boom := errors.New("db down")
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{},
		WithOccurrenceSkips(&fakeSkips{err: boom}))
	e := seedOccurrence(repo, rid, &recurrenceID, time.Now().Add(48*time.Hour))

	if err := f.Delete(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID); !errors.Is(err, boom) {
		t.Fatalf("want the skip error to propagate, got %v", err)
	}
	if len(repo.deleted) != 0 {
		t.Fatal("the event must survive a failed tombstone write")
	}
}

// MOVING a generated occurrence frees its original slot just as a delete does,
// so the original slot needs the same tombstone — otherwise the venue ends up
// with the moved event AND a regenerated one at the old time.
func TestMovingGeneratedOccurrenceRecordsSkipForOldSlot(t *testing.T) {
	rid, actorID, recurrenceID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	skips := &fakeSkips{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{}, WithOccurrenceSkips(skips))
	oldStart := time.Date(2026, time.August, 26, 19, 0, 0, 0, time.UTC)
	e := seedOccurrence(repo, rid, &recurrenceID, oldStart)

	newStart := oldStart.Add(2 * time.Hour)
	in := UpdateInput{
		Title:    "Караоке-битва",
		StartsAt: newStart,
		EndsAt:   newStart.Add(3 * time.Hour),
		Status:   domain.EventPublished,
	}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(skips.recorded) != 1 || !skips.recorded[0].slot.Equal(oldStart) {
		t.Fatalf("moving an occurrence must tombstone the ORIGINAL slot, got %+v", skips.recorded)
	}
}

// Editing anything BUT the time leaves the slot occupied by this very row, so
// the unique index already protects it: no tombstone, or the venue could never
// restore a date it merely retitled.
func TestEditingGeneratedOccurrenceInPlaceRecordsNoSkip(t *testing.T) {
	rid, actorID, recurrenceID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	skips := &fakeSkips{}
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{}, WithOccurrenceSkips(skips))
	starts := time.Date(2026, time.August, 26, 19, 0, 0, 0, time.UTC)
	e := seedOccurrence(repo, rid, &recurrenceID, starts)

	in := UpdateInput{
		Title:    "Караоке-битва: финал",
		StartsAt: starts,
		EndsAt:   starts.Add(3 * time.Hour),
		Status:   domain.EventPublished,
	}
	if _, err := f.Update(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(skips.recorded) != 0 {
		t.Fatalf("an in-place edit must leave no tombstone, got %+v", skips.recorded)
	}
}

// A facade built WITHOUT the tombstone port (older tests, and any caller that
// never touches a generated event) must keep working exactly as before.
func TestDeleteWithoutSkipPortStillWorks(t *testing.T) {
	rid, actorID, recurrenceID := uuid.New(), uuid.New(), uuid.New()
	repo := newFakeRepo()
	f := NewFacade(repo, permsWith(actorID, rid, domain.StaffRoleManager), &fakeFeed{})
	e := seedOccurrence(repo, rid, &recurrenceID, time.Now().Add(48*time.Hour))

	if err := f.Delete(context.Background(), Actor{UserID: actorID, Role: domain.RoleRestaurant}, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(repo.deleted) != 1 {
		t.Fatal("the event must be deleted")
	}
}

package consent

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// fakeConsentRepo is an in-memory append-only log.
type fakeConsentRepo struct{ appended []domain.ConsentRecord }

func (f *fakeConsentRepo) Append(_ context.Context, rec *domain.ConsentRecord) error {
	f.appended = append(f.appended, *rec)
	return nil
}
func (f *fakeConsentRepo) CurrentState(context.Context, uuid.UUID) ([]domain.ConsentRecord, error) {
	return nil, nil
}

type fakePrefRepo struct {
	stored map[uuid.UUID]domain.NotificationPreference
}

func (f *fakePrefRepo) Get(_ context.Context, id uuid.UUID) (domain.NotificationPreference, error) {
	if p, ok := f.stored[id]; ok {
		return p, nil
	}
	return domain.DefaultNotificationPreference(id), nil
}
func (f *fakePrefRepo) Upsert(_ context.Context, p domain.NotificationPreference) error {
	if f.stored == nil {
		f.stored = map[uuid.UUID]domain.NotificationPreference{}
	}
	f.stored[p.UserID] = p
	return nil
}

func newFacade() (*fakeConsentRepo, *fakePrefRepo, Facade) {
	cr := &fakeConsentRepo{}
	pr := &fakePrefRepo{stored: map[uuid.UUID]domain.NotificationPreference{}}
	return cr, pr, NewFacade(cr, pr)
}

func TestRecordValidatesInput(t *testing.T) {
	_, _, f := newFacade()
	uid := uuid.New()
	ctx := context.Background()

	cases := []struct {
		name string
		in   RecordInput
	}{
		{"empty type", RecordInput{ConsentType: "  ", Version: "v1", Source: domain.ConsentSourceApp}},
		{"empty version", RecordInput{ConsentType: "marketing", Version: " ", Source: domain.ConsentSourceApp}},
		{"bad source", RecordInput{ConsentType: "marketing", Version: "v1", Source: "sms"}},
	}
	for _, tc := range cases {
		if _, err := f.Record(ctx, uid, tc.in); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: expected ErrValidation, got %v", tc.name, err)
		}
	}
}

func TestRecordAppendsTrimmed(t *testing.T) {
	cr, _, f := newFacade()
	uid := uuid.New()

	rec, err := f.Record(context.Background(), uid, RecordInput{
		ConsentType: "  privacy_policy ", Version: " v2 ", Granted: true, Source: domain.ConsentSourceWeb,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.ConsentType != "privacy_policy" || rec.Version != "v2" {
		t.Fatalf("expected trimmed values, got type=%q version=%q", rec.ConsentType, rec.Version)
	}
	if rec.UserID != uid {
		t.Fatalf("record scoped to wrong user: %v", rec.UserID)
	}
	if len(cr.appended) != 1 {
		t.Fatalf("expected exactly one append, got %d", len(cr.appended))
	}
}

func TestSetPreferencesRoundTrips(t *testing.T) {
	_, _, f := newFacade()
	uid := uuid.New()
	ctx := context.Background()

	got, err := f.SetPreferences(ctx, uid, PreferenceInput{
		NotificationsEnabled: true, PushEnabled: false, EmailEnabled: true,
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got.PushEnabled {
		t.Fatalf("push should be off")
	}
	read, err := f.Preferences(ctx, uid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.PushEnabled || !read.NotificationsEnabled || !read.EmailEnabled {
		t.Fatalf("preference did not round-trip: %+v", read)
	}
}

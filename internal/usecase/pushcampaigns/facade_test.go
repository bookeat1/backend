package pushcampaigns

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func newTestFacade(subjects *fakeSubjects, audienceRows []domain.PushAudienceRow, perms *fakePerms, channelConfigured bool, clock func() time.Time) (*facade, *fakeCampaigns, *fakeRecipients) {
	campaigns := newFakeCampaigns()
	recipients := newFakeRecipients()
	f := &facade{
		subjects: subjects, audience: &fakeAudience{rows: audienceRows}, campaigns: campaigns, recipients: recipients,
		perms: perms, loc: mustLoc("Asia/Almaty"), dailyCap: 1, weeklyCap: 3,
		pushChannelConfigured: channelConfigured, now: clock,
	}
	return f, campaigns, recipients
}

func adminActor() Actor { return Actor{UserID: uuid.New(), Role: domain.RoleAdmin} }

func at(hour int) func() time.Time {
	loc := mustLoc("Asia/Almaty")
	return func() time.Time { return time.Date(2026, 9, 17, hour, 30, 0, 0, loc) }
}

// fixedNoon is the reference instant every fixture below anchors its
// StartsAt/EndsAt to — using real time.Now() alongside an injected facade
// clock (at(hour)) would make "already started" tests flaky depending on
// when the suite actually runs.
var fixedNoon = at(12)()

// TestEstimateBreakdownFiveGuests pins criterion 3: five guests, one per
// category, and Eligible/breakdown match exactly.
func TestEstimateBreakdownFiveGuests(t *testing.T) {
	subjects := newFakeSubjects()
	almaty := uuid.New()
	subjectID := uuid.New()
	subjects.put(domain.PushCampaignSubject{
		Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, CityID: &almaty,
		Status: "published", StartsAt: fixedNoon.Add(24 * time.Hour), EndsAt: fixedNoon.Add(27 * time.Hour),
		Title: "T",
	})
	rows := []domain.PushAudienceRow{
		{UserID: uuid.New(), Allowed: false, HasDevice: true},
		{UserID: uuid.New(), Allowed: true, Capped: true, HasDevice: true},
		{UserID: uuid.New(), Allowed: true, Duplicate: true, HasDevice: true},
		{UserID: uuid.New(), Allowed: true, HasDevice: false},
		{UserID: uuid.New(), Allowed: true, HasDevice: true},
	}
	f, _, _ := newTestFacade(subjects, rows, &fakePerms{}, true, at(12))

	res, err := f.Estimate(context.Background(), adminActor(), domain.PushCampaignKindEvent, subjectID)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if res.InCity != 5 || res.OptedOut != 1 || res.Capped != 1 || res.AlreadyReceived != 1 || res.NoDevice != 1 || res.Eligible != 1 {
		t.Fatalf("estimate = %+v, want one guest per category", res)
	}
	if res.QuietHoursNow {
		t.Fatalf("12:00 must not be quiet hours")
	}
	if len(res.Preview) != 3 {
		t.Fatalf("preview langs = %d, want 3", len(res.Preview))
	}
}

// TestEstimateForbiddenForNonAdmin pins §0.1: only RoleAdmin may estimate.
func TestEstimateForbiddenForNonAdmin(t *testing.T) {
	subjects := newFakeSubjects()
	f, _, _ := newTestFacade(subjects, nil, &fakePerms{}, true, at(12))
	_, err := f.Estimate(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser}, domain.PushCampaignKindEvent, uuid.New())
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func publishedEventSubject(id uuid.UUID, cityID *uuid.UUID, restaurantID *uuid.UUID, active bool) domain.PushCampaignSubject {
	return domain.PushCampaignSubject{
		Kind: domain.PushCampaignKindEvent, SubjectID: id, CityID: cityID, RestaurantID: restaurantID,
		RestaurantIsActive: active, Status: "published",
		StartsAt: fixedNoon.Add(24 * time.Hour), EndsAt: fixedNoon.Add(27 * time.Hour), Title: "T",
	}
}

// TestCreateHappyPath pins criterion 4's 201 branch.
func TestCreateHappyPath(t *testing.T) {
	subjects := newFakeSubjects()
	almaty := uuid.New()
	subjectID := uuid.New()
	subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
	rows := []domain.PushAudienceRow{{UserID: uuid.New(), Allowed: true, HasDevice: true}}
	f, campaigns, _ := newTestFacade(subjects, rows, &fakePerms{}, true, at(12))

	c, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != domain.PushCampaignQueued {
		t.Fatalf("status = %s, want queued", c.Status)
	}
	if c.EstimatedRecipients != 1 {
		t.Fatalf("estimated_recipients = %d, want 1", c.EstimatedRecipients)
	}
	if _, err := campaigns.GetByID(context.Background(), c.ID); err != nil {
		t.Fatalf("campaign not persisted: %v", err)
	}
}

// TestCreateSubjectNotFound pins criterion 4's 404 branch.
func TestCreateSubjectNotFound(t *testing.T) {
	f, _, _ := newTestFacade(newFakeSubjects(), nil, &fakePerms{}, true, at(12))
	_, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New()})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestCreateForbiddenForNonAdmin pins criterion 4's 403 branch.
func TestCreateForbiddenForNonAdmin(t *testing.T) {
	f, _, _ := newTestFacade(newFakeSubjects(), nil, &fakePerms{}, true, at(12))
	_, err := f.Create(context.Background(), Actor{UserID: uuid.New(), Role: domain.RoleUser},
		CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New()})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// TestCreateDoubleClickIsConflict pins criterion 2/3.3: a second POST while
// the first is still active answers 409 campaign_in_progress.
func TestCreateDoubleClickIsConflict(t *testing.T) {
	subjects := newFakeSubjects()
	almaty := uuid.New()
	subjectID := uuid.New()
	subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
	rows := []domain.PushAudienceRow{{UserID: uuid.New(), Allowed: true, HasDevice: true}}
	f, _, _ := newTestFacade(subjects, rows, &fakePerms{}, true, at(12))
	ctx := context.Background()
	in := CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID}

	if _, err := f.Create(ctx, adminActor(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := f.Create(ctx, adminActor(), in)
	if !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("second create err = %v, want ErrAlreadyExists", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodeCampaignInProgress {
		t.Fatalf("code = %v, ok=%v, want CodeCampaignInProgress", code, ok)
	}
}

// TestCreateValidationReasons pins criterion 8: subject_not_published,
// subject_expired, venue_inactive, each its own 422 reason.
func TestCreateValidationReasons(t *testing.T) {
	almaty := uuid.New()
	rid := uuid.New()

	cases := []struct {
		name    string
		subject domain.PushCampaignSubject
		want    domain.ErrorCode
	}{
		{
			name: "draft is not published",
			subject: domain.PushCampaignSubject{
				Kind: domain.PushCampaignKindEvent, RestaurantID: nil, CityID: &almaty,
				Status: "draft", StartsAt: fixedNoon.Add(time.Hour), EndsAt: fixedNoon.Add(2 * time.Hour), Title: "T",
			},
			want: domain.CodeSubjectNotPublished,
		},
		{
			name: "event already started",
			subject: domain.PushCampaignSubject{
				Kind: domain.PushCampaignKindEvent, RestaurantID: nil, CityID: &almaty,
				Status: "published", StartsAt: fixedNoon.Add(-time.Hour), EndsAt: fixedNoon.Add(time.Hour), Title: "T",
			},
			want: domain.CodeSubjectExpired,
		},
		{
			name: "venue inactive",
			subject: domain.PushCampaignSubject{
				Kind: domain.PushCampaignKindEvent, RestaurantID: &rid, RestaurantIsActive: false, CityID: &almaty,
				Status: "published", StartsAt: fixedNoon.Add(time.Hour), EndsAt: fixedNoon.Add(2 * time.Hour), Title: "T",
			},
			want: domain.CodeVenueInactive,
		},
		{
			name: "city does not resolve for a venue subject",
			subject: domain.PushCampaignSubject{
				Kind: domain.PushCampaignKindEvent, RestaurantID: &rid, RestaurantIsActive: true, CityID: nil,
				Status: "published", StartsAt: fixedNoon.Add(time.Hour), EndsAt: fixedNoon.Add(2 * time.Hour), Title: "T",
			},
			want: domain.CodeCityUnresolved,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subjects := newFakeSubjects()
			subjectID := uuid.New()
			c.subject.SubjectID = subjectID
			subjects.put(c.subject)
			f, _, _ := newTestFacade(subjects, nil, &fakePerms{}, true, at(12))
			_, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if code, ok := domain.CodeOf(err); !ok || code != c.want {
				t.Fatalf("code = %v, ok=%v, want %v", code, ok, c.want)
			}
		})
	}
}

// TestCreateQuietHours pins criterion 7: 22:30/03:00 need force_quiet_hours,
// 12:00 does not.
func TestCreateQuietHours(t *testing.T) {
	almaty := uuid.New()
	for _, hour := range []int{22, 3} {
		t.Run("blocked without flag", func(t *testing.T) {
			subjects := newFakeSubjects()
			subjectID := uuid.New()
			subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
			f, _, _ := newTestFacade(subjects, []domain.PushAudienceRow{}, &fakePerms{}, true, at(hour))
			_, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if code, ok := domain.CodeOf(err); !ok || code != domain.CodeQuietHours {
				t.Fatalf("code = %v, ok=%v, want CodeQuietHours", code, ok)
			}
		})
		t.Run("allowed with force flag", func(t *testing.T) {
			subjects := newFakeSubjects()
			subjectID := uuid.New()
			subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
			f, _, _ := newTestFacade(subjects, []domain.PushAudienceRow{}, &fakePerms{}, true, at(hour))
			_, err := f.Create(context.Background(), adminActor(),
				CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, ForceQuietHours: true})
			if err != nil {
				t.Fatalf("create with force flag at hour %d: %v", hour, err)
			}
		})
	}
	t.Run("noon needs no flag", func(t *testing.T) {
		subjects := newFakeSubjects()
		subjectID := uuid.New()
		subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
		f, _, _ := newTestFacade(subjects, []domain.PushAudienceRow{}, &fakePerms{}, true, at(12))
		if _, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID}); err != nil {
			t.Fatalf("create at noon: %v", err)
		}
	})
}

// TestCreateChannelDisabled pins criterion 9: 503 when no provider is
// configured, and the campaign is NOT created.
func TestCreateChannelDisabled(t *testing.T) {
	subjects := newFakeSubjects()
	almaty := uuid.New()
	subjectID := uuid.New()
	subjects.put(publishedEventSubject(subjectID, &almaty, nil, true))
	f, campaigns, _ := newTestFacade(subjects, []domain.PushAudienceRow{}, &fakePerms{}, false, at(12))

	_, err := f.Create(context.Background(), adminActor(), CreateInput{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if code, ok := domain.CodeOf(err); !ok || code != domain.CodePushChannelDisabled {
		t.Fatalf("code = %v, ok=%v, want CodePushChannelDisabled", code, ok)
	}
	if len(campaigns.rows) != 0 {
		t.Fatalf("a campaign was created despite the channel being disabled")
	}

	// Estimate must still work (criterion 9's second half).
	if _, err := f.Estimate(context.Background(), adminActor(), domain.PushCampaignKindEvent, subjectID); err != nil {
		t.Fatalf("estimate with channel disabled: %v", err)
	}
}

// TestListLatestBySubjectsAuthorization pins criterion 5: RoleAdmin or
// PermRestaurantManage may list a restaurant's campaigns; a manager of
// another restaurant is forbidden; platform=true is admin-only.
func TestListLatestBySubjectsAuthorization(t *testing.T) {
	subjects := newFakeSubjects()
	rid := uuid.New()
	eventID := uuid.New()
	subjects.put(domain.PushCampaignSubject{Kind: domain.PushCampaignKindEvent, SubjectID: eventID, RestaurantID: &rid, Status: "published", StartsAt: fixedNoon, EndsAt: fixedNoon})

	perms := &fakePerms{allowed: map[uuid.UUID]bool{rid: true}}
	f, campaigns, _ := newTestFacade(subjects, nil, perms, true, at(12))
	if err := campaigns.Create(context.Background(), &domain.PushCampaign{
		Kind: domain.PushCampaignKindEvent, SubjectID: eventID, RestaurantID: &rid, EstimatedRecipients: 1,
	}); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}

	manager := Actor{UserID: uuid.New(), Role: domain.RoleUser}
	list, err := f.ListLatestBySubjects(context.Background(), manager, domain.PushCampaignKindEvent, &rid, false)
	if err != nil {
		t.Fatalf("list as manager with permission: %v", err)
	}
	if len(list) != 1 || list[0].SubjectID != eventID {
		t.Fatalf("list = %+v, want the one seeded campaign", list)
	}

	otherRid := uuid.New()
	_, err = f.ListLatestBySubjects(context.Background(), manager, domain.PushCampaignKindEvent, &otherRid, false)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("manager of another restaurant: err = %v, want ErrForbidden", err)
	}

	_, err = f.ListLatestBySubjects(context.Background(), manager, domain.PushCampaignKindEvent, nil, true)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("platform=true as non-admin: err = %v, want ErrForbidden", err)
	}
}

// TestGetIncludesSkipCounts pins criterion 6.
func TestGetIncludesSkipCounts(t *testing.T) {
	subjects := newFakeSubjects()
	f, campaigns, recipients := newTestFacade(subjects, nil, &fakePerms{}, true, at(12))
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 2}
	if err := campaigns.Create(context.Background(), c); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := recipients.RecordSkipped(context.Background(), c.ID, []uuid.UUID{uuid.New()}, domain.RecipientSkippedOptOut, time.Now()); err != nil {
		t.Fatalf("seed skip: %v", err)
	}

	got, err := f.Get(context.Background(), adminActor(), c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SkipCounts[domain.RecipientSkippedOptOut] != 1 {
		t.Fatalf("skip counts = %+v, want 1 optout", got.SkipCounts)
	}
}

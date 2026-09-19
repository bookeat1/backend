package pushcampaigns

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestSender(subjects *fakeSubjects, audienceRows []domain.PushAudienceRow, send BatchSender, clock func() time.Time) (*Sender, *fakeCampaigns, *fakeRecipients, *fakeDeviceTokensPC, *fakeFeedPC, *fakeTicketsPC) {
	campaigns := newFakeCampaigns()
	recipients := newFakeRecipients()
	tokens := newFakeDeviceTokensPC()
	feed := &fakeFeedPC{}
	tickets := &fakeTicketsPC{}
	s := NewSender(subjects, &fakeAudience{rows: audienceRows}, campaigns, recipients, tokens,
		&fakeLanguages{}, feed, tickets, send, fakeTx{}, mustLoc("Asia/Almaty"), SenderConfig{DailyCap: 1, WeeklyCap: 3}, testLog())
	s.now = clock
	return s, campaigns, recipients, tokens, feed, tickets
}

// TestSenderCancelsWhenSubjectMissing pins criterion 11: a deleted subject
// cancels the campaign with subject_missing, no push sent.
func TestSenderCancelsWhenSubjectMissing(t *testing.T) {
	subjects := newFakeSubjects()
	s, campaigns, _, _, _, _ := newTestSender(subjects, nil, neverCalledSender(t), at(12))
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _ := campaigns.GetByID(context.Background(), c.ID)
	if got.Status != domain.PushCampaignCancelled || got.CancelReason == nil || *got.CancelReason != domain.CancelReasonSubjectMissing {
		t.Fatalf("campaign = %+v, want cancelled/subject_missing", got)
	}
}

// TestSenderCancelsWhenSubjectUnpublishedOrExpiredOrVenueInactive pins the
// other three re-read reasons of criterion 11.
func TestSenderCancelsWhenSubjectUnpublishedOrExpiredOrVenueInactive(t *testing.T) {
	rid := uuid.New()
	cases := []struct {
		name    string
		subject domain.PushCampaignSubject
		want    domain.PushCampaignCancelReason
	}{
		{"unpublished", domain.PushCampaignSubject{Status: "hidden", StartsAt: fixedNoon.Add(time.Hour), EndsAt: fixedNoon.Add(2 * time.Hour)}, domain.CancelReasonSubjectUnpublished},
		{"expired", domain.PushCampaignSubject{Status: "published", StartsAt: fixedNoon.Add(-time.Hour), EndsAt: fixedNoon.Add(time.Hour)}, domain.CancelReasonSubjectExpired},
		{"venue inactive", domain.PushCampaignSubject{Status: "published", RestaurantID: &rid, RestaurantIsActive: false, StartsAt: fixedNoon.Add(time.Hour), EndsAt: fixedNoon.Add(2 * time.Hour)}, domain.CancelReasonVenueInactive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subjects := newFakeSubjects()
			subjectID := uuid.New()
			c.subject.Kind = domain.PushCampaignKindEvent
			c.subject.SubjectID = subjectID
			subjects.put(c.subject)
			s, campaigns, _, _, _, _ := newTestSender(subjects, nil, neverCalledSender(t), at(12))
			camp := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
			mustSeedCampaign(t, campaigns, camp, domain.PushCampaignQueued)

			if _, err := s.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			got, _ := campaigns.GetByID(context.Background(), camp.ID)
			if got.Status != domain.PushCampaignCancelled || got.CancelReason == nil || *got.CancelReason != c.want {
				t.Fatalf("campaign = %+v, want cancelled/%s", got, c.want)
			}
		})
	}
}

func publishedSubjectForSender(id uuid.UUID) domain.PushCampaignSubject {
	return domain.PushCampaignSubject{
		Kind: domain.PushCampaignKindEvent, SubjectID: id, Status: "published",
		StartsAt: fixedNoon.Add(24 * time.Hour), EndsAt: fixedNoon.Add(27 * time.Hour), Title: "T",
	}
}

// TestSenderRecordsSkipsAndSendsEligible pins criteria 13/18/19/20/21: the
// skip buckets land as the right status, an eligible guest gets a message
// with the right channel/payload, a durable feed row, and Finish's counters
// add up.
func TestSenderRecordsSkipsAndSendsEligible(t *testing.T) {
	subjects := newFakeSubjects()
	subjectID := uuid.New()
	subjects.put(publishedSubjectForSender(subjectID))

	optOut, capped, dup, noDevice, eligible := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rows := []domain.PushAudienceRow{
		{UserID: optOut, Allowed: false, HasDevice: true},
		{UserID: capped, Allowed: true, Capped: true, HasDevice: true},
		{UserID: dup, Allowed: true, Duplicate: true, HasDevice: true},
		{UserID: noDevice, Allowed: true, HasDevice: false},
		{UserID: eligible, Allowed: true, HasDevice: true},
	}

	var sentMsgs []SendMessage
	send := func(_ context.Context, msgs []SendMessage) ([]SendResult, error) {
		sentMsgs = msgs
		out := make([]SendResult, len(msgs))
		for i, m := range msgs {
			out[i] = SendResult{Verdict: SendDelivered, TicketID: "tk-" + m.Token}
		}
		return out, nil
	}
	s, campaigns, recipients, tokens, feed, tickets := newTestSender(subjects, rows, send, at(12))
	tokens.add(eligible, "tok-eligible")

	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)

	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Done != 1 {
		t.Fatalf("tick result = %+v, want Done=1", res)
	}

	counts, _ := recipients.CountByStatus(context.Background(), c.ID)
	want := map[domain.PushCampaignRecipientStatus]int{
		domain.RecipientSkippedOptOut: 1, domain.RecipientSkippedCap: 1,
		domain.RecipientSkippedDuplicate: 1, domain.RecipientSkippedNoDevice: 1, domain.RecipientSent: 1,
	}
	for status, n := range want {
		if counts[status] != n {
			t.Errorf("counts[%s] = %d, want %d (all counts: %+v)", status, counts[status], n, counts)
		}
	}

	got, _ := campaigns.GetByID(context.Background(), c.ID)
	if got.Status != domain.PushCampaignDone || got.SentCount != 1 || got.SkippedCount != 4 || got.FailedCount != 0 {
		t.Fatalf("campaign = %+v, want done/1 sent/4 skipped/0 failed", got)
	}

	if len(sentMsgs) != 1 {
		t.Fatalf("messages sent = %d, want 1 (only the eligible guest)", len(sentMsgs))
	}
	if sentMsgs[0].ChannelID != "offers" {
		t.Fatalf("channel id = %q, want offers (criterion 21)", sentMsgs[0].ChannelID)
	}
	if sentMsgs[0].Data["event"] != "content.event" || sentMsgs[0].Data["campaign_id"] != c.ID.String() || sentMsgs[0].Data["event_id"] != subjectID.String() {
		t.Fatalf("payload = %+v, want content.event + campaign_id + event_id", sentMsgs[0].Data)
	}

	if len(feed.rows) != 1 || feed.rows[0].UserID != eligible || feed.rows[0].Type != domain.FeedTypeEvent {
		t.Fatalf("feed rows = %+v, want one event-type row for the eligible guest", feed.rows)
	}
	if feed.rows[0].CampaignID == nil || *feed.rows[0].CampaignID != c.ID {
		t.Fatalf("feed row campaign_id = %v, want %v", feed.rows[0].CampaignID, c.ID)
	}

	if len(tickets.rows) != 1 || tickets.rows[0].CampaignID == nil || *tickets.rows[0].CampaignID != c.ID {
		t.Fatalf("tickets = %+v, want one ticket tagged with the campaign", tickets.rows)
	}
}

// TestSenderAtMostOnceNeverResendsAlreadyDecidedGuest pins criterion 14: a
// guest already `sending` from a previous crashed attempt is excluded from a
// later pass — never sent to twice.
func TestSenderAtMostOnceNeverResendsAlreadyDecidedGuest(t *testing.T) {
	subjects := newFakeSubjects()
	subjectID := uuid.New()
	subjects.put(publishedSubjectForSender(subjectID))
	guest := uuid.New()
	rows := []domain.PushAudienceRow{{UserID: guest, Allowed: true, HasDevice: true}}

	s, campaigns, recipients, tokens, _, _ := newTestSender(subjects, rows, neverCalledSender(t), at(12))
	tokens.add(guest, "tok")
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)

	// Simulate a crashed previous attempt: the guest already has a `sending`
	// row for this campaign.
	if _, err := recipients.ClaimSending(context.Background(), c.ID, []uuid.UUID{guest}, time.Now()); err != nil {
		t.Fatalf("seed sending row: %v", err)
	}

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// neverCalledSender fails the test if `send` is invoked — reaching here
	// proves the guest was never re-sent to.
	counts, _ := recipients.CountByStatus(context.Background(), c.ID)
	if counts[domain.RecipientSending] != 1 {
		t.Fatalf("counts = %+v, want the guest to stay in sending (never retried)", counts)
	}
}

// TestSenderReschedulesOnTransientFailure pins criterion 16: a transient send
// error bumps attempts and sets next_attempt_at with exponential backoff, and
// after MaxAttempts the campaign is failed.
func TestSenderReschedulesOnTransientFailure(t *testing.T) {
	subjects := newFakeSubjects()
	subjectID := uuid.New()
	subjects.put(publishedSubjectForSender(subjectID))
	guest := uuid.New()
	rows := []domain.PushAudienceRow{{UserID: guest, Allowed: true, HasDevice: true}}
	failing := func(context.Context, []SendMessage) ([]SendResult, error) {
		return nil, errors.New("expo: 502")
	}
	s, campaigns, _, tokens, _, _ := newTestSender(subjects, rows, failing, at(12))
	s.cfg.MaxAttempts = 2
	tokens.add(guest, "tok")
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	got, _ := campaigns.GetByID(context.Background(), c.ID)
	if got.Status != domain.PushCampaignSending || got.Attempts != 1 || got.NextAttemptAt == nil {
		t.Fatalf("after 1 failure: %+v, want sending/attempts=1/next_attempt_at set", got)
	}

	// Advance past the lease AND the backoff so the campaign is claimable again.
	s.now = func() time.Time { return at(12)().Add(2 * time.Hour) }
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	got, _ = campaigns.GetByID(context.Background(), c.ID)
	if got.Status != domain.PushCampaignFailed || got.Attempts != 2 {
		t.Fatalf("after MaxAttempts=2 failures: %+v, want failed/attempts=2", got)
	}
}

// TestSenderDoesNotRetryUnknownOutcome pins the bug-review fix for #141: a
// send failure whose outcome the provider genuinely could not confirm (a
// client timeout, a connection reset after the request left this process, or
// an unreadable/malformed response) must NEVER be retried — retrying could
// duplicate a push the provider already accepted (spec
// push-campaigns-manual-spec §7 п.7 / §3.11: "лучше недослать, чем прислать
// дважды"). Contrast with TestSenderReschedulesOnTransientFailure, whose
// plain "expo: 502" error is a DEFINITE non-delivery (the provider itself
// answered) and must keep retrying.
func TestSenderDoesNotRetryUnknownOutcome(t *testing.T) {
	subjects := newFakeSubjects()
	subjectID := uuid.New()
	subjects.put(publishedSubjectForSender(subjectID))
	guest := uuid.New()
	rows := []domain.PushAudienceRow{{UserID: guest, Allowed: true, HasDevice: true}}
	var calls int
	unknownOutcome := func(context.Context, []SendMessage) ([]SendResult, error) {
		calls++
		// Simulates a client timeout/connection reset AFTER the request may
		// have already reached the provider — see expopush.SendBatchError's
		// Definite flag, translated here via MarkUnknownRange the same way
		// bootstrap.expoBatchSender does.
		return nil, MarkUnknownRange(context.DeadlineExceeded, 0, 1)
	}
	s, campaigns, recipients, tokens, _, _ := newTestSender(subjects, rows, unknownOutcome, at(12))
	s.cfg.MaxAttempts = 5
	tokens.add(guest, "tok")
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: subjectID, EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	counts, _ := recipients.CountByStatus(context.Background(), c.ID)
	if counts[domain.RecipientSending] != 1 {
		t.Fatalf("after tick 1: counts = %+v, want the guest left `sending` (unknown outcome, not unclaimed)", counts)
	}

	// Advance past the lease AND the backoff so the campaign is claimable
	// again, and tick a second time.
	s.now = func() time.Time { return at(12)().Add(2 * time.Hour) }
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("BatchSender called %d times, want exactly 1 — an ambiguous-outcome recipient must never be resent to", calls)
	}
	counts, _ = recipients.CountByStatus(context.Background(), c.ID)
	if counts[domain.RecipientSending] != 1 {
		t.Fatalf("after tick 2: counts = %+v, want the guest still `sending` (never retried, never resolved)", counts)
	}
}

// TestSenderExpiresStaleQueuedCampaign pins criterion 17 at the Sender level
// (the repository-level test already covers the SQL; this proves Tick reports
// it and never calls the guest audience for an expired campaign).
func TestSenderExpiresStaleQueuedCampaign(t *testing.T) {
	subjects := newFakeSubjects()
	s, campaigns, _, _, _, _ := newTestSender(subjects, nil, neverCalledSender(t), at(12))
	c := &domain.PushCampaign{Kind: domain.PushCampaignKindEvent, SubjectID: uuid.New(), EstimatedRecipients: 1}
	mustSeedCampaign(t, campaigns, c, domain.PushCampaignQueued)
	campaigns.rows[c.ID].CreatedAt = at(12)().Add(-7 * time.Hour)

	res, err := s.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Expired != 1 {
		t.Fatalf("tick result = %+v, want Expired=1", res)
	}
	got, _ := campaigns.GetByID(context.Background(), c.ID)
	if got.Status != domain.PushCampaignExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
}

func neverCalledSender(t *testing.T) BatchSender {
	return func(context.Context, []SendMessage) ([]SendResult, error) {
		t.Fatal("send must not be called")
		return nil, nil
	}
}

func mustSeedCampaign(t *testing.T, campaigns *fakeCampaigns, c *domain.PushCampaign, status domain.PushCampaignStatus) {
	t.Helper()
	if err := campaigns.Create(context.Background(), c); err != nil {
		t.Fatalf("seed campaign: %v", err)
	}
	campaigns.rows[c.ID].Status = status
}

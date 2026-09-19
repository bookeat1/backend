package pushcampaigns

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// --- subjects -----------------------------------------------------------

type fakeSubjects struct {
	byKind map[domain.PushCampaignKind]map[uuid.UUID]domain.PushCampaignSubject
	// platformIDs / restaurantIDs let list tests control ListPlatformIDs /
	// ListIDsByRestaurant independently of the subject map above.
	platformIDs   map[domain.PushCampaignKind][]uuid.UUID
	restaurantIDs map[uuid.UUID][]uuid.UUID // restaurantID -> subject ids (any kind, filtered by caller-tracked kind if needed)
}

func newFakeSubjects() *fakeSubjects {
	return &fakeSubjects{
		byKind:        map[domain.PushCampaignKind]map[uuid.UUID]domain.PushCampaignSubject{},
		platformIDs:   map[domain.PushCampaignKind][]uuid.UUID{},
		restaurantIDs: map[uuid.UUID][]uuid.UUID{},
	}
}

func (f *fakeSubjects) put(s domain.PushCampaignSubject) {
	if f.byKind[s.Kind] == nil {
		f.byKind[s.Kind] = map[uuid.UUID]domain.PushCampaignSubject{}
	}
	f.byKind[s.Kind][s.SubjectID] = s
	if s.RestaurantID == nil {
		f.platformIDs[s.Kind] = append(f.platformIDs[s.Kind], s.SubjectID)
	} else {
		f.restaurantIDs[*s.RestaurantID] = append(f.restaurantIDs[*s.RestaurantID], s.SubjectID)
	}
}

func (f *fakeSubjects) Resolve(_ context.Context, kind domain.PushCampaignKind, id uuid.UUID) (*domain.PushCampaignSubject, error) {
	s, ok := f.byKind[kind][id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := s
	return &cp, nil
}

func (f *fakeSubjects) ListIDsByRestaurant(_ context.Context, kind domain.PushCampaignKind, restaurantID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, id := range f.restaurantIDs[restaurantID] {
		if s, ok := f.byKind[kind][id]; ok && s.RestaurantID != nil && *s.RestaurantID == restaurantID {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeSubjects) ListPlatformIDs(_ context.Context, kind domain.PushCampaignKind) ([]uuid.UUID, error) {
	return f.platformIDs[kind], nil
}

// --- audience -------------------------------------------------------------

type fakeAudience struct {
	rows []domain.PushAudienceRow
}

func (f *fakeAudience) Classify(context.Context, *uuid.UUID, domain.PushCampaignKind, uuid.UUID, time.Time, int, int) ([]domain.PushAudienceRow, error) {
	return f.rows, nil
}

// --- campaigns --------------------------------------------------------------

type fakeCampaigns struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.PushCampaign
}

func newFakeCampaigns() *fakeCampaigns {
	return &fakeCampaigns{rows: map[uuid.UUID]*domain.PushCampaign{}}
}

func (f *fakeCampaigns) Create(_ context.Context, c *domain.PushCampaign) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Kind == c.Kind && r.SubjectID == c.SubjectID && (r.Status == domain.PushCampaignQueued || r.Status == domain.PushCampaignSending) {
			return domain.ErrAlreadyExists
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Status == "" {
		c.Status = domain.PushCampaignQueued
	}
	c.CreatedAt = time.Now()
	cp := *c
	f.rows[c.ID] = &cp
	return nil
}

func (f *fakeCampaigns) GetByID(_ context.Context, id uuid.UUID) (*domain.PushCampaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeCampaigns) LatestBySubjects(_ context.Context, kind domain.PushCampaignKind, subjectIDs []uuid.UUID) (map[uuid.UUID]domain.PushCampaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := map[uuid.UUID]bool{}
	for _, id := range subjectIDs {
		want[id] = true
	}
	out := map[uuid.UUID]domain.PushCampaign{}
	for _, r := range f.rows {
		if r.Kind != kind || !want[r.SubjectID] {
			continue
		}
		if cur, ok := out[r.SubjectID]; !ok || r.CreatedAt.After(cur.CreatedAt) {
			out[r.SubjectID] = *r
		}
	}
	return out, nil
}

func (f *fakeCampaigns) CountToday(_ context.Context, cityID *uuid.UUID, since time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if !r.CreatedAt.Before(since) && uuidPtrEq(r.CityID, cityID) {
			n++
		}
	}
	return n, nil
}

func uuidPtrEq(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (f *fakeCampaigns) Claim(_ context.Context, now time.Time, leaseFor, maxQueueAge time.Duration, limit int) ([]domain.PushCampaign, []uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var candidates []*domain.PushCampaign
	for _, r := range f.rows {
		if r.Status == domain.PushCampaignQueued {
			candidates = append(candidates, r)
		} else if r.Status == domain.PushCampaignSending && r.LeaseUntil != nil && !r.LeaseUntil.After(now) &&
			(r.NextAttemptAt == nil || !r.NextAttemptAt.After(now)) {
			candidates = append(candidates, r)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	var claimed []domain.PushCampaign
	var expired []uuid.UUID
	for _, r := range candidates {
		if r.Status == domain.PushCampaignQueued && now.Sub(r.CreatedAt) > maxQueueAge {
			r.Status = domain.PushCampaignExpired
			expired = append(expired, r.ID)
			continue
		}
		lease := now.Add(leaseFor)
		r.Status = domain.PushCampaignSending
		r.LeaseUntil = &lease
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
		claimed = append(claimed, *r)
	}
	return claimed, expired, nil
}

func (f *fakeCampaigns) Cancel(_ context.Context, id uuid.UUID, reason domain.PushCampaignCancelReason, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || (r.Status != domain.PushCampaignQueued && r.Status != domain.PushCampaignSending) {
		return nil
	}
	r.Status = domain.PushCampaignCancelled
	r.CancelReason = &reason
	r.FinishedAt = &at
	return nil
}

func (f *fakeCampaigns) Reschedule(_ context.Context, id uuid.UUID, lastError string, nextAttemptAt time.Time, maxAttempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.Status != domain.PushCampaignSending {
		return nil
	}
	r.Attempts++
	r.LastError = lastError
	r.NextAttemptAt = &nextAttemptAt
	if r.Attempts >= maxAttempts {
		r.Status = domain.PushCampaignFailed
		r.FinishedAt = &nextAttemptAt
	}
	return nil
}

func (f *fakeCampaigns) Finish(_ context.Context, id uuid.UUID, sent, skipped, failed int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.Status != domain.PushCampaignSending {
		return nil
	}
	r.Status = domain.PushCampaignDone
	r.SentCount, r.SkippedCount, r.FailedCount = sent, skipped, failed
	r.FinishedAt = &at
	return nil
}

// --- recipients -------------------------------------------------------------

type recipientKey struct {
	campaign, user uuid.UUID
}

type fakeRecipients struct {
	mu   sync.Mutex
	rows map[recipientKey]domain.PushCampaignRecipientStatus
}

func newFakeRecipients() *fakeRecipients {
	return &fakeRecipients{rows: map[recipientKey]domain.PushCampaignRecipientStatus{}}
}

func (f *fakeRecipients) ClaimSending(_ context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, _ time.Time) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, uid := range userIDs {
		k := recipientKey{campaignID, uid}
		if _, ok := f.rows[k]; ok {
			continue
		}
		f.rows[k] = domain.RecipientSending
		out = append(out, uid)
	}
	return out, nil
}

func (f *fakeRecipients) UnclaimSending(_ context.Context, campaignID uuid.UUID, userIDs []uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, uid := range userIDs {
		k := recipientKey{campaignID, uid}
		if f.rows[k] == domain.RecipientSending {
			delete(f.rows, k)
		}
	}
	return nil
}

func (f *fakeRecipients) Resolve(_ context.Context, campaignID, userID uuid.UUID, status domain.PushCampaignRecipientStatus, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[recipientKey{campaignID, userID}] = status
	return nil
}

func (f *fakeRecipients) RecordSkipped(_ context.Context, campaignID uuid.UUID, userIDs []uuid.UUID, status domain.PushCampaignRecipientStatus, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, uid := range userIDs {
		k := recipientKey{campaignID, uid}
		if _, ok := f.rows[k]; ok {
			continue
		}
		f.rows[k] = status
	}
	return nil
}

func (f *fakeRecipients) CountByStatus(_ context.Context, campaignID uuid.UUID) (map[domain.PushCampaignRecipientStatus]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.PushCampaignRecipientStatus]int{}
	for k, status := range f.rows {
		if k.campaign == campaignID {
			out[status]++
		}
	}
	return out, nil
}

// --- permissions ------------------------------------------------------------

type fakePerms struct {
	allowed map[uuid.UUID]bool // restaurantID -> allowed
}

func (f *fakePerms) HasPermission(_ context.Context, _ uuid.UUID, restaurantID uuid.UUID, _ domain.Permission) (bool, error) {
	return f.allowed[restaurantID], nil
}

// --- device tokens ------------------------------------------------------------

type fakeDeviceTokensPC struct {
	mu     sync.Mutex
	byUser map[uuid.UUID][]domain.DevicePushToken
	dead   map[uuid.UUID]bool
}

func newFakeDeviceTokensPC() *fakeDeviceTokensPC {
	return &fakeDeviceTokensPC{byUser: map[uuid.UUID][]domain.DevicePushToken{}, dead: map[uuid.UUID]bool{}}
}

func (f *fakeDeviceTokensPC) add(userID uuid.UUID, token string) uuid.UUID {
	id := uuid.New()
	f.byUser[userID] = append(f.byUser[userID], domain.DevicePushToken{ID: id, UserID: userID, Token: token, Platform: domain.PlatformIOS, IsActive: true})
	return id
}

func (f *fakeDeviceTokensPC) Upsert(context.Context, *domain.DevicePushToken) error { return nil }
func (f *fakeDeviceTokensPC) ListActiveByUser(_ context.Context, userID uuid.UUID) ([]domain.DevicePushToken, error) {
	return f.ListActiveByUsers(context.Background(), []uuid.UUID{userID})
}
func (f *fakeDeviceTokensPC) ListActiveByUsers(_ context.Context, userIDs []uuid.UUID) ([]domain.DevicePushToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.DevicePushToken
	for _, uid := range userIDs {
		for _, t := range f.byUser[uid] {
			if !f.dead[t.ID] {
				out = append(out, t)
			}
		}
	}
	return out, nil
}
func (f *fakeDeviceTokensPC) DeactivateByID(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead[id] = true
	return nil
}
func (f *fakeDeviceTokensPC) DeactivateForUser(context.Context, uuid.UUID, string) error { return nil }

// --- languages ----------------------------------------------------------------

type fakeLanguages struct{ byUser map[uuid.UUID]string }

func (f *fakeLanguages) PreferredLanguages(_ context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, uid := range userIDs {
		if lang, ok := f.byUser[uid]; ok {
			out[uid] = lang
		}
	}
	return out, nil
}

// --- feed -----------------------------------------------------------------

type fakeFeedPC struct {
	mu   sync.Mutex
	rows []domain.Notification
}

func (f *fakeFeedPC) Insert(_ context.Context, n *domain.Notification) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.UserID == n.UserID && uuidPtrEq(r.CampaignID, n.CampaignID) && n.CampaignID != nil {
			return false, nil
		}
	}
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	f.rows = append(f.rows, *n)
	return true, nil
}
func (f *fakeFeedPC) ListByUser(context.Context, uuid.UUID, *domain.NotificationCursor, int) ([]domain.Notification, error) {
	return f.rows, nil
}
func (f *fakeFeedPC) CountUnread(context.Context, uuid.UUID) (int, error)  { return 0, nil }
func (f *fakeFeedPC) MarkRead(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeFeedPC) MarkAllRead(context.Context, uuid.UUID) error         { return nil }

// --- tickets ----------------------------------------------------------------

type fakeTicketsPC struct {
	mu   sync.Mutex
	rows []domain.PushTicket
}

func (f *fakeTicketsPC) Record(_ context.Context, t domain.PushTicket) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, t)
	return nil
}
func (f *fakeTicketsPC) ListUnresolved(context.Context, time.Time, int) ([]domain.PushTicket, error) {
	return nil, nil
}
func (f *fakeTicketsPC) Resolve(context.Context, []string, time.Time) error { return nil }
func (f *fakeTicketsPC) ExpireOlderThan(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

// --- tx manager (no real transaction, just runs fn) --------------------------

type fakeTx struct{}

func (fakeTx) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }
func (fakeTx) Detach(ctx context.Context) context.Context                             { return ctx }

// --- batch sender -------------------------------------------------------------

// scriptedSender answers each call with the next scripted response, so a test
// can simulate a mid-batch transient failure precisely.
type scriptedSender struct {
	mu    sync.Mutex
	calls [][]SendMessage
	// results[i] is what call i returns (both slots ignored if err != nil is
	// requested by resultForCall returning ok=false to fall back to default).
	resultFn func(call int, msgs []SendMessage) ([]SendResult, error)
}

func (s *scriptedSender) send(ctx context.Context, msgs []SendMessage) ([]SendResult, error) {
	s.mu.Lock()
	call := len(s.calls)
	s.calls = append(s.calls, msgs)
	s.mu.Unlock()
	if s.resultFn != nil {
		return s.resultFn(call, msgs)
	}
	out := make([]SendResult, len(msgs))
	for i, m := range msgs {
		out[i] = SendResult{Verdict: SendDelivered, TicketID: "ticket-" + m.Token}
	}
	return out, nil
}

func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(fmt.Sprintf("load location %q: %v", name, err))
	}
	return loc
}

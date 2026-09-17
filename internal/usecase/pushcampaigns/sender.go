package pushcampaigns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// SenderConfig is the background sender's schedule and safety knobs — bound
// to bootstrap.PushCampaignsConfig's env vars.
type SenderConfig struct {
	Tick        time.Duration
	BatchSize   int // campaigns claimed per tick
	LeaseFor    time.Duration
	MaxQueueAge time.Duration
	MaxAttempts int
	// SendBatchSize is Expo's own per-request ceiling, passed straight through
	// to the BatchSender (expopush.Sender.SendBatch's own chunking) rather than
	// applied here — Sender itself never re-chunks. See NewPushCampaignsSender.
	SendBatchSize int
	DailyCap      int
	WeeklyCap     int
}

func (c SenderConfig) withDefaults() SenderConfig {
	if c.Tick <= 0 {
		c.Tick = 10 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 5
	}
	if c.LeaseFor <= 0 {
		c.LeaseFor = 10 * time.Minute
	}
	if c.MaxQueueAge <= 0 {
		c.MaxQueueAge = 6 * time.Hour
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 12
	}
	if c.SendBatchSize <= 0 {
		c.SendBatchSize = 100
	}
	if c.DailyCap <= 0 {
		c.DailyCap = 1
	}
	if c.WeeklyCap <= 0 {
		c.WeeklyCap = 3
	}
	return c
}

// backoff mirrors notifications.DispatcherConfig.backoff: base, doubling,
// capped at one hour — the exact schedule spec criterion 16 pins (1, 2, 4…
// minutes, ceiling 1h).
func (c SenderConfig) backoff(attempts int) time.Duration {
	const (
		base    = time.Minute
		ceiling = time.Hour
	)
	d := base
	for i := 1; i < attempts; i++ {
		if d >= ceiling/2 {
			return ceiling
		}
		d *= 2
	}
	return d
}

// Sender is the manual push-campaign background worker (cmd/worker). Once per
// tick it claims campaigns ready to send (claim+lease, criterion 10),
// re-validates each subject fresh (criterion 11), fans the eligible audience
// out in provider batches (criterion 15), and records the outcome durably
// before moving on — never inferring a campaign's fate from what is already
// in memory.
//
// It shares its guest classification (Classify) and text rendering
// (renderText) with Facade's Estimate, and is built ONLY when a guest push
// provider is configured (bootstrap.NewDeps mirrors NewPushReceiptWorker's
// posture here, not the reconcilers' safe-idle one: with no provider nothing
// could ever be sent, so there is no reason to tick at all).
type Sender struct {
	subjects   domain.PushCampaignSubjectRepository
	audience   domain.PushCampaignAudienceRepository
	campaigns  domain.PushCampaignRepository
	recipients domain.PushCampaignRecipientRepository
	tokens     domain.DevicePushTokenRepository
	languages  languageReader
	feed       domain.NotificationFeedRepository
	tickets    domain.PushTicketRepository
	send       BatchSender
	tx         domain.TxManager
	loc        *time.Location
	cfg        SenderConfig
	log        *slog.Logger
	now        func() time.Time
}

// NewSender builds the push-campaign background sender.
func NewSender(
	subjects domain.PushCampaignSubjectRepository,
	audience domain.PushCampaignAudienceRepository,
	campaigns domain.PushCampaignRepository,
	recipients domain.PushCampaignRecipientRepository,
	tokens domain.DevicePushTokenRepository,
	languages languageReader,
	feed domain.NotificationFeedRepository,
	tickets domain.PushTicketRepository,
	send BatchSender,
	tx domain.TxManager,
	loc *time.Location,
	cfg SenderConfig,
	log *slog.Logger,
) *Sender {
	return &Sender{
		subjects: subjects, audience: audience, campaigns: campaigns, recipients: recipients,
		tokens: tokens, languages: languages, feed: feed, tickets: tickets, send: send, tx: tx,
		loc: loc, cfg: cfg.withDefaults(), log: log, now: time.Now,
	}
}

// SenderTickResult counts what one pass did. Zero values are the steady state.
type SenderTickResult struct {
	Claimed   int
	Expired   int
	Cancelled int
	Done      int
	Retried   int
	Failed    int
}

func (r SenderTickResult) attrs() []any {
	return []any{
		slog.Int("claimed", r.Claimed), slog.Int("expired", r.Expired),
		slog.Int("cancelled", r.Cancelled), slog.Int("done", r.Done),
		slog.Int("retried", r.Retried), slog.Int("failed", r.Failed),
	}
}

// Run ticks until ctx is cancelled. A failing pass is logged and retried on
// the next tick — a transient database error must not kill the process.
func (s *Sender) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	s.log.Info("push campaign sender started", slog.Duration("tick", s.cfg.Tick))
	for {
		select {
		case <-ctx.Done():
			s.log.Info("push campaign sender stopped")
			return nil
		case <-t.C:
			res, err := s.Tick(ctx)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					continue
				}
				s.log.Error("push campaign sender tick failed", slog.String("error", err.Error()))
				continue
			}
			if res != (SenderTickResult{}) {
				s.log.Info("push campaign sender tick", res.attrs()...)
			}
		}
	}
}

// Tick runs one pass. Exported so it can be driven directly from tests.
func (s *Sender) Tick(ctx context.Context) (SenderTickResult, error) {
	now := s.now()
	var res SenderTickResult

	var claimed []domain.PushCampaign
	var expired []uuid.UUID
	if err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		var e error
		claimed, expired, e = s.campaigns.Claim(ctx, now, s.cfg.LeaseFor, s.cfg.MaxQueueAge, s.cfg.BatchSize)
		return e
	}); err != nil {
		return res, fmt.Errorf("claim push campaigns: %w", err)
	}
	res.Expired = len(expired)
	res.Claimed = len(claimed)

	for i := range claimed {
		if err := s.processCampaign(ctx, &claimed[i], now, &res); err != nil {
			return res, fmt.Errorf("process push campaign %s: %w", claimed[i].ID, err)
		}
	}
	return res, nil
}

// processCampaign re-validates the subject, fans the eligible audience out,
// and persists the outcome. It never returns an error for a business-level
// refusal (cancelled/failed/rescheduled are all recorded, not propagated) —
// only an infrastructure failure (a DB write that itself failed) aborts the
// tick.
func (s *Sender) processCampaign(ctx context.Context, c *domain.PushCampaign, now time.Time, res *SenderTickResult) error {
	subject, err := s.subjects.Resolve(ctx, c.Kind, c.SubjectID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			res.Cancelled++
			return s.campaigns.Cancel(ctx, c.ID, domain.CancelReasonSubjectMissing, now)
		}
		return err
	}
	if subject.Status != "published" {
		res.Cancelled++
		return s.campaigns.Cancel(ctx, c.ID, domain.CancelReasonSubjectUnpublished, now)
	}
	if subject.Expired(now) {
		res.Cancelled++
		return s.campaigns.Cancel(ctx, c.ID, domain.CancelReasonSubjectExpired, now)
	}
	if subject.RestaurantID != nil && !subject.RestaurantIsActive {
		res.Cancelled++
		return s.campaigns.Cancel(ctx, c.ID, domain.CancelReasonVenueInactive, now)
	}

	rows, err := s.audience.Classify(ctx, subject.CityID, c.Kind, c.SubjectID, now, s.cfg.DailyCap, s.cfg.WeeklyCap)
	if err != nil {
		return err
	}
	b := classifyAll(rows)

	// Skipped guests are recorded directly — they never pass through `sending`.
	for status, ids := range map[domain.PushCampaignRecipientStatus][]uuid.UUID{
		domain.RecipientSkippedOptOut:    b.SkippedOptOut,
		domain.RecipientSkippedCap:       b.SkippedCap,
		domain.RecipientSkippedDuplicate: b.SkippedDuplicate,
		domain.RecipientSkippedNoDevice:  b.SkippedNoDevice,
	} {
		if err := s.recipients.RecordSkipped(ctx, c.ID, ids, status, now); err != nil {
			return err
		}
	}

	// At-most-once (criterion 14): only guests with NO existing row for this
	// campaign are claimed into `sending`. A guest already decided by an
	// earlier, crashed attempt is silently excluded here — see Sender's doc
	// comment and spec 3.11.
	toSend, err := s.recipients.ClaimSending(ctx, c.ID, b.Eligible, now)
	if err != nil {
		return err
	}

	sendErr := s.fanOut(ctx, c, subject, toSend, now, res)

	counts, err := s.recipients.CountByStatus(ctx, c.ID)
	if err != nil {
		return err
	}
	sent := counts[domain.RecipientSent]
	skipped := counts[domain.RecipientSkippedOptOut] + counts[domain.RecipientSkippedCap] +
		counts[domain.RecipientSkippedDuplicate] + counts[domain.RecipientSkippedNoDevice]
	// A guest still `sending` (send never resolved it — a crash, or a batch
	// error mid-way) is counted failed for reporting: at-most-once means it
	// will never be retried, so "unknown forever" is honestly "failed", not
	// "sent" and not "skipped".
	failed := counts[domain.RecipientFailed] + counts[domain.RecipientSending]

	if sendErr != nil {
		res.Retried++
		next := now.Add(s.cfg.backoff(c.Attempts + 1))
		s.log.Warn("push campaign send failed transiently, rescheduling",
			slog.String("campaign_id", c.ID.String()), slog.Int("attempts", c.Attempts+1),
			slog.Time("next_attempt_at", next), slog.String("error", sendErr.Error()))
		if c.Attempts+1 >= s.cfg.MaxAttempts {
			res.Failed++
			s.log.Error("push campaign abandoned after the attempt budget ran out",
				slog.String("campaign_id", c.ID.String()), slog.Int("attempts", c.Attempts+1),
				slog.String("error", sendErr.Error()))
		}
		return s.campaigns.Reschedule(ctx, c.ID, sendErr.Error(), next, s.cfg.MaxAttempts)
	}

	res.Done++
	return s.campaigns.Finish(ctx, c.ID, sent, skipped, failed, now)
}

// fanOut sends to every claimed guest's active devices, records the durable
// feed entry for each successfully-sent guest, and resolves each guest's
// recipient row. A guest with multiple devices gets exactly one feed entry
// and counts as `sent` once at least one of their devices succeeds (spec
// 3.10 — "оба получают пуш, в ленте одна запись").
func (s *Sender) fanOut(ctx context.Context, c *domain.PushCampaign, subject *domain.PushCampaignSubject, userIDs []uuid.UUID, now time.Time, res *SenderTickResult) error {
	if len(userIDs) == 0 {
		return nil
	}
	tokens, err := s.tokens.ListActiveByUsers(ctx, userIDs)
	if err != nil {
		return fmt.Errorf("list device tokens: %w", err)
	}
	languages, err := s.languages.PreferredLanguages(ctx, userIDs)
	if err != nil {
		return fmt.Errorf("read preferred languages: %w", err)
	}

	type target struct {
		userID  uuid.UUID
		tokenID uuid.UUID
	}
	var msgs []SendMessage
	var targets []target
	byUser := map[uuid.UUID][]domain.DevicePushToken{}
	for _, t := range tokens {
		byUser[t.UserID] = append(byUser[t.UserID], t)
	}
	for _, uid := range userIDs {
		lang := languages[uid]
		text := renderText(*subject, lang, s.loc)
		data := map[string]string{"event": "content." + string(c.Kind), "campaign_id": c.ID.String()}
		switch c.Kind {
		case domain.PushCampaignKindEvent:
			data["event_id"] = c.SubjectID.String()
		case domain.PushCampaignKindPromo:
			data["promo_id"] = c.SubjectID.String()
		}
		if c.RestaurantID != nil {
			data["restaurant_id"] = c.RestaurantID.String()
		}
		for _, dt := range byUser[uid] {
			msgs = append(msgs, SendMessage{Token: dt.Token, Title: text.Title, Body: text.Body, Data: data, ChannelID: "offers"})
			targets = append(targets, target{userID: uid, tokenID: dt.ID})
		}
	}
	if len(msgs) == 0 {
		// Every claimed guest lost their only device between Classify and
		// here (a race with the guest signing out). Nothing to send, but the
		// recipient rows must still resolve — otherwise they would sit
		// `sending` forever and never be retried.
		for _, uid := range userIDs {
			if err := s.recipients.Resolve(ctx, c.ID, uid, domain.RecipientFailed, now); err != nil {
				return err
			}
		}
		return nil
	}

	results, sendErr := s.send(ctx, msgs)
	// Every message that came back Delivered is FINAL and must never be
	// retried — that guest's send worked, resending would duplicate a real
	// push on their phone. Everything else is provisional until we know
	// whether sendErr is nil (see below).
	sentUsers := map[uuid.UUID]bool{}
	for i, r := range results {
		if i >= len(targets) {
			break
		}
		tg := targets[i]
		switch r.Verdict {
		case SendDelivered:
			sentUsers[tg.userID] = true
			s.recordTicket(ctx, r.TicketID, tg.tokenID, c.ID)
		case SendDeviceGone:
			if err := s.tokens.DeactivateByID(ctx, tg.tokenID); err != nil {
				s.log.Error("push campaign: deactivate gone device token failed",
					slog.String("device_token_id", tg.tokenID.String()), slog.String("error", err.Error()))
			}
		}
	}

	var retry []uuid.UUID
	for _, uid := range userIDs {
		if sentUsers[uid] {
			if err := s.recipients.Resolve(ctx, c.ID, uid, domain.RecipientSent, now); err != nil {
				return err
			}
			if err := s.writeFeedEntry(ctx, c, subject, uid); err != nil {
				s.log.Error("push campaign: write feed entry failed",
					slog.String("campaign_id", c.ID.String()), slog.String("user_id", uid.String()),
					slog.String("error", err.Error()))
			}
			continue
		}
		if sendErr != nil {
			// The call itself failed transiently: we KNOW (we are still in
			// this same process, past the call, not recovering from a crash)
			// that nothing was delivered to this guest, so it is safe to
			// unclaim them for a retry on the next attempt — the exact
			// "continues with guests who have no row yet" behaviour spec 3.11
			// describes, without weakening the crash-safety ClaimSending's
			// write-before-send gives a guest whose fate we genuinely do not
			// know (a process crash, not an error THIS call observed).
			retry = append(retry, uid)
			continue
		}
		// sendErr == nil: this guest has a DEFINITIVE non-delivered answer
		// (rejected by the provider, or every device gone) — terminal,
		// never retried.
		if err := s.recipients.Resolve(ctx, c.ID, uid, domain.RecipientFailed, now); err != nil {
			return err
		}
	}
	if len(retry) > 0 {
		if err := s.recipients.UnclaimSending(ctx, c.ID, retry); err != nil {
			return err
		}
	}
	return sendErr
}

// writeFeedEntry appends the durable «Уведомления» row for one sent guest
// (criterion 19). Type/EventID/PromoID are set from the campaign's own kind —
// never both, matching notifications' one-producer CHECK.
func (s *Sender) writeFeedEntry(ctx context.Context, c *domain.PushCampaign, subject *domain.PushCampaignSubject, userID uuid.UUID) error {
	n := &domain.Notification{
		UserID: userID, RestaurantID: c.RestaurantID, CampaignID: &c.ID,
	}
	switch c.Kind {
	case domain.PushCampaignKindEvent:
		n.Type = domain.FeedTypeEvent
		id := c.SubjectID
		n.EventID = &id
		n.Title, n.Body = "Новое событие", subject.Title
	case domain.PushCampaignKindPromo:
		n.Type = domain.FeedTypePromo
		id := c.SubjectID
		n.PromoID = &id
		n.Title, n.Body = "Акция", subject.Title
	}
	_, err := s.feed.Insert(ctx, n)
	return err
}

// recordTicket enqueues an accepted push for receipt polling. A failure here
// is logged, never returned — the push has already left, and the only cost of
// a lost ticket is one missed chance to notice a dead token (mirrors
// GuestPushNotifier.recordTicket).
func (s *Sender) recordTicket(ctx context.Context, ticketID string, deviceTokenID, campaignID uuid.UUID) {
	if s.tickets == nil || ticketID == "" {
		return
	}
	cid := campaignID
	if err := s.tickets.Record(ctx, domain.PushTicket{ID: ticketID, DeviceTokenID: deviceTokenID, CampaignID: &cid}); err != nil {
		s.log.Error("push campaign: record receipt ticket failed",
			slog.String("device_token_id", deviceTokenID.String()), slog.String("error", err.Error()))
	}
}

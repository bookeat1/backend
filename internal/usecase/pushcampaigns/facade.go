package pushcampaigns

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// EstimateResult is the confirmation modal's whole data source (criterion 3).
// Categories are computed by the SAME Classify chain Sender uses, in the SAME
// priority order, so they never overlap: InCity == NoDevice+OptedOut+Capped+
// AlreadyReceived+Eligible always holds.
type EstimateResult struct {
	City                 string
	InCity               int
	NoDevice             int
	OptedOut             int
	Capped               int
	AlreadyReceived      int
	Eligible             int
	QuietHoursNow        bool
	CampaignsTodayInCity int
	// LastCampaign is nil when the subject has never had a campaign.
	LastCampaign *domain.PushCampaign
	// Preview is always ru+kk+en, independent of who will actually receive it
	// (criterion 3) — keyed by domain.LocaleRU/KK/EN.
	Preview map[string]PushText
}

// CreateInput is what the confirmation modal submits.
type CreateInput struct {
	Kind            domain.PushCampaignKind
	SubjectID       uuid.UUID
	ForceQuietHours bool
}

// GetResult is one campaign plus its per-guest skip-reason breakdown
// (criterion 6 — "для разбора").
type GetResult struct {
	Campaign   domain.PushCampaign
	SkipCounts map[domain.PushCampaignRecipientStatus]int
}

// Facade exposes the superadmin-facing push-campaign operations.
type Facade interface {
	// Estimate previews a campaign's reach without creating anything.
	// RoleAdmin only. ErrNotFound if the subject does not exist.
	Estimate(ctx context.Context, actor Actor, kind domain.PushCampaignKind, subjectID uuid.UUID) (*EstimateResult, error)
	// Create queues a new campaign. RoleAdmin only. See errors.go-style
	// sentinels: ErrNotFound (404), ErrValidation with a CodeSubject*/
	// CodeCityUnresolved/CodeQuietHours code (422), ErrUnavailable with
	// CodePushChannelDisabled (503), ErrAlreadyExists with
	// CodeCampaignInProgress (409).
	Create(ctx context.Context, actor Actor, in CreateInput) (*domain.PushCampaign, error)
	// ListLatestBySubjects returns the latest campaign per subject id, for
	// subjects belonging to restaurantID (RoleAdmin or PermRestaurantManage
	// there) or, when platform is true, the platform's own subjects
	// (RoleAdmin only). Subjects with no campaign are simply absent.
	ListLatestBySubjects(ctx context.Context, actor Actor, kind domain.PushCampaignKind, restaurantID *uuid.UUID, platform bool) ([]domain.PushCampaign, error)
	// Get returns one campaign plus its skip-reason breakdown. Same
	// authorization as ListLatestBySubjects, resolved from the campaign's own
	// restaurant (nil restaurant = platform subject = RoleAdmin only).
	Get(ctx context.Context, actor Actor, id uuid.UUID) (*GetResult, error)
}

type facade struct {
	subjects   domain.PushCampaignSubjectRepository
	audience   domain.PushCampaignAudienceRepository
	campaigns  domain.PushCampaignRepository
	recipients domain.PushCampaignRecipientRepository
	perms      permissionChecker
	loc        *time.Location
	dailyCap   int
	weeklyCap  int
	// pushChannelConfigured mirrors bootstrap.PushConfig.GuestPushConfigured():
	// whether GUEST_PUSH_PROVIDER is set. Estimate ignores it entirely
	// (criterion 9 — reach can be inspected before the channel is ready);
	// Create refuses with 503 when it is false.
	pushChannelConfigured bool
	now                   func() time.Time
}

// NewFacade builds the admin-facing push-campaign Facade.
func NewFacade(
	subjects domain.PushCampaignSubjectRepository,
	audience domain.PushCampaignAudienceRepository,
	campaigns domain.PushCampaignRepository,
	recipients domain.PushCampaignRecipientRepository,
	perms permissionChecker,
	loc *time.Location,
	dailyCap, weeklyCap int,
	pushChannelConfigured bool,
) Facade {
	return &facade{
		subjects: subjects, audience: audience, campaigns: campaigns, recipients: recipients,
		perms: perms, loc: loc, dailyCap: dailyCap, weeklyCap: weeklyCap,
		pushChannelConfigured: pushChannelConfigured, now: time.Now,
	}
}

func (f *facade) Estimate(ctx context.Context, actor Actor, kind domain.PushCampaignKind, subjectID uuid.UUID) (*EstimateResult, error) {
	if actor.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("%w: only the platform may estimate a push campaign's reach", domain.ErrForbidden)
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: kind must be event or promo", domain.ErrValidation)
	}
	subject, err := f.subjects.Resolve(ctx, kind, subjectID)
	if err != nil {
		return nil, err
	}
	now := f.now()

	rows, err := f.audience.Classify(ctx, subject.CityID, kind, subjectID, now, f.dailyCap, f.weeklyCap)
	if err != nil {
		return nil, err
	}
	b := classifyAll(rows)

	latest, err := f.campaigns.LatestBySubjects(ctx, kind, []uuid.UUID{subjectID})
	if err != nil {
		return nil, err
	}
	var last *domain.PushCampaign
	if c, ok := latest[subjectID]; ok {
		cc := c
		last = &cc
	}

	campaignsToday, err := f.campaigns.CountToday(ctx, subject.CityID, startOfDay(now, f.loc))
	if err != nil {
		return nil, err
	}

	city := ""
	if subject.CityName != nil {
		city = *subject.CityName
	}
	return &EstimateResult{
		City: city, InCity: b.InCity, NoDevice: b.NoDevice(), OptedOut: b.OptedOut(),
		Capped: b.Capped(), AlreadyReceived: b.AlreadyReceived(), Eligible: b.EligibleCount(),
		QuietHoursNow:        isQuietHours(now, f.loc),
		CampaignsTodayInCity: campaignsToday,
		LastCampaign:         last,
		Preview:              renderPreview(*subject, f.loc),
	}, nil
}

func (f *facade) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.PushCampaign, error) {
	if actor.Role != domain.RoleAdmin {
		return nil, fmt.Errorf("%w: only the platform may send a push campaign", domain.ErrForbidden)
	}
	if !in.Kind.Valid() {
		return nil, fmt.Errorf("%w: kind must be event or promo", domain.ErrValidation)
	}
	if !f.pushChannelConfigured {
		return nil, domain.WithCode(domain.CodePushChannelDisabled,
			fmt.Errorf("%w: no guest push provider is configured", domain.ErrUnavailable))
	}
	subject, err := f.subjects.Resolve(ctx, in.Kind, in.SubjectID)
	if err != nil {
		return nil, err
	}
	if subject.Status != "published" {
		return nil, domain.WithCode(domain.CodeSubjectNotPublished,
			fmt.Errorf("%w: subject is not published", domain.ErrValidation))
	}
	now := f.now()
	if subject.Expired(now) {
		return nil, domain.WithCode(domain.CodeSubjectExpired,
			fmt.Errorf("%w: the subject's own window has already ended", domain.ErrValidation))
	}
	if subject.RestaurantID != nil && !subject.RestaurantIsActive {
		return nil, domain.WithCode(domain.CodeVenueInactive,
			fmt.Errorf("%w: the subject's venue is inactive", domain.ErrValidation))
	}
	if subject.RestaurantID != nil && subject.CityID == nil {
		return nil, domain.WithCode(domain.CodeCityUnresolved,
			fmt.Errorf("%w: the subject's city does not resolve in the dictionary", domain.ErrValidation))
	}
	if !in.ForceQuietHours && isQuietHours(now, f.loc) {
		return nil, domain.WithCode(domain.CodeQuietHours,
			fmt.Errorf("%w: it is quiet hours (21:00-10:00); confirm to send anyway", domain.ErrValidation))
	}

	rows, err := f.audience.Classify(ctx, subject.CityID, in.Kind, in.SubjectID, now, f.dailyCap, f.weeklyCap)
	if err != nil {
		return nil, err
	}
	b := classifyAll(rows)

	createdBy := actor.UserID
	c := &domain.PushCampaign{
		Kind: in.Kind, SubjectID: in.SubjectID, RestaurantID: subject.RestaurantID,
		CityID: subject.CityID, City: subject.CityName,
		ForceQuietHours: in.ForceQuietHours, EstimatedRecipients: b.EligibleCount(),
		CreatedBy: &createdBy,
	}
	if err := f.campaigns.Create(ctx, c); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return nil, domain.WithCode(domain.CodeCampaignInProgress, err)
		}
		return nil, err
	}
	return c, nil
}

func (f *facade) ListLatestBySubjects(ctx context.Context, actor Actor, kind domain.PushCampaignKind, restaurantID *uuid.UUID, platform bool) ([]domain.PushCampaign, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: kind must be event or promo", domain.ErrValidation)
	}
	var ids []uuid.UUID
	var err error
	if platform {
		if actor.Role != domain.RoleAdmin {
			return nil, fmt.Errorf("%w: only the platform may list its own push campaigns", domain.ErrForbidden)
		}
		ids, err = f.subjects.ListPlatformIDs(ctx, kind)
	} else {
		if restaurantID == nil {
			return nil, fmt.Errorf("%w: restaurant_id is required unless platform=true", domain.ErrValidation)
		}
		if err := f.authorizeRestaurant(ctx, actor, *restaurantID); err != nil {
			return nil, err
		}
		ids, err = f.subjects.ListIDsByRestaurant(ctx, kind, *restaurantID)
	}
	if err != nil {
		return nil, err
	}
	latest, err := f.campaigns.LatestBySubjects(ctx, kind, ids)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PushCampaign, 0, len(latest))
	for _, id := range ids {
		if c, ok := latest[id]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *facade) Get(ctx context.Context, actor Actor, id uuid.UUID) (*GetResult, error) {
	c, err := f.campaigns.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := f.authorizeRead(ctx, actor, c.RestaurantID); err != nil {
		return nil, err
	}
	counts, err := f.recipients.CountByStatus(ctx, id)
	if err != nil {
		return nil, err
	}
	return &GetResult{Campaign: *c, SkipCounts: counts}, nil
}

// authorizeRead gates GET .../push-campaigns[/:id]: RoleAdmin bypasses;
// otherwise a platform subject (nil restaurantID) is admin-only, and a
// venue-bound one needs PermRestaurantManage there — same shape as
// usecase/events.authorize, but read-only.
func (f *facade) authorizeRead(ctx context.Context, actor Actor, restaurantID *uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	if restaurantID == nil {
		return fmt.Errorf("%w: only the platform may read a platform campaign", domain.ErrForbidden)
	}
	return f.authorizeRestaurant(ctx, actor, *restaurantID)
}

func (f *facade) authorizeRestaurant(ctx context.Context, actor Actor, restaurantID uuid.UUID) error {
	if actor.Role == domain.RoleAdmin {
		return nil
	}
	ok, err := f.perms.HasPermission(ctx, actor.UserID, restaurantID, domain.PermRestaurantManage)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: restaurant.manage required to read this restaurant's push campaigns", domain.ErrForbidden)
	}
	return nil
}

// isQuietHours reports whether `now`, rendered in loc, falls in
// 21:00-10:00 — the window spec §3.8 requires an explicit confirmation for.
func isQuietHours(now time.Time, loc *time.Location) bool {
	h := now.In(loc).Hour()
	return h >= 21 || h < 10
}

// startOfDay is midnight of `now`'s calendar day in loc — the boundary
// CountToday's "campaigns today" counts from.
func startOfDay(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

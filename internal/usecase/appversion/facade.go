// Package appversion is the mobile update gate: the public "must this build
// update?" question every app launch asks, and the platform-only screen that
// sets the thresholds and the wording behind it.
//
// The rule the package exists to enforce: THE MODE IS THE SERVER'S DECISION.
// An over-the-air update cannot carry a native change, so "ask nicely" and "do
// not let them continue" have to be switchable without a store release — and
// therefore without a deploy, which is why the policy lives in a table and not
// in the environment.
//
// The second rule, which every uncertain path in here obeys: uncertainty means
// "do nothing". A missing row, an empty threshold, a threshold nobody can parse
// and a client sending nonsense all resolve to domain.AppUpdateNone. This
// endpoint can lock a paying guest out of a working app, so every ambiguity is
// resolved in the direction that cannot.
package appversion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// Actor is the authenticated caller of the management routes. The public check
// has no actor at all — it is called before anyone signs in.
type Actor struct {
	UserID uuid.UUID
	Role   domain.Role
}

// Decision is the answer to one launching client.
type Decision struct {
	Platform domain.DevicePlatform
	Action   domain.AppUpdateAction
	// StoreURL is where the "Update" button goes. Carried even when Action is
	// none: a client may want it for an "about" screen, and it is not a secret.
	StoreURL string
	// The thresholds as configured, so a support engineer looking at a guest's
	// HAR log can see WHY they were told what they were told.
	MinSupportedVersion   string
	MinRecommendedVersion string
	// Title / Message are full {ru,kk,en} objects, nil when Action is none.
	// Whole maps rather than one resolved string on purpose: the client picks
	// the language, so the answer does not depend on a request header and the
	// response can be cached by URL alone.
	Title   domain.I18n
	Message domain.I18n
}

// UseCase is the mobile update gate.
type UseCase interface {
	// Check answers one client. It never fails because of the client's input:
	// an unconfigured platform or an unparsable version come back as a normal
	// Decision with Action = none. It DOES return an error when the database
	// is unreachable — a caller that cannot read the policy must not be able
	// to invent one, and the app treats any non-200 as "do nothing".
	Check(ctx context.Context, platform domain.DevicePlatform, clientVersion string) (Decision, error)
	// List returns every platform's policy for the admin screen.
	List(ctx context.Context, actor Actor) ([]domain.MobileAppPolicy, error)
	// Save merges a partial update onto one platform's policy.
	Save(ctx context.Context, actor Actor, platform domain.DevicePlatform, in SaveInput) (*domain.MobileAppPolicy, error)
}

// SaveInput is a PATCH: every field is a pointer, so "absent from the request"
// (preserve) is distinguishable from "explicitly set". The *I18n fields are
// PARTIAL translation patches (domain.I18nPatch) for the same reason — a panel
// editing the Kazakh wording must not have to resend English to keep it. Both
// conventions match the promos/events/cities admin writes.
type SaveInput struct {
	MinSupportedVersion   *string
	MinRecommendedVersion *string
	StoreURL              *string

	RecommendedTitle       *string
	RecommendedTitleI18n   domain.I18nPatch
	RecommendedMessage     *string
	RecommendedMessageI18n domain.I18nPatch
	RequiredTitle          *string
	RequiredTitleI18n      domain.I18nPatch
	RequiredMessage        *string
	RequiredMessageI18n    domain.I18nPatch
}

// Column widths from migration 0103. Checked here so an over-long text comes
// back as a 422 naming the field instead of a Postgres 22001 rendered as a 500.
const (
	maxVersionLen  = 32
	maxStoreURLLen = 512
	maxTitleLen    = 256
	maxMessageLen  = 1024
)

type useCase struct {
	repo domain.MobileAppPolicyRepository
	log  *slog.Logger
}

// NewUseCase builds the gate. A nil logger falls back to slog.Default().
func NewUseCase(repo domain.MobileAppPolicyRepository, log *slog.Logger) UseCase {
	if log == nil {
		log = slog.Default()
	}
	return &useCase{repo: repo, log: log}
}

func (u *useCase) Check(ctx context.Context, platform domain.DevicePlatform, clientVersion string) (Decision, error) {
	d := Decision{Platform: platform, Action: domain.AppUpdateNone}
	p, err := u.repo.Get(ctx, platform)
	if errors.Is(err, domain.ErrNotFound) {
		// The platform has no policy at all. Not an error: the feature is
		// simply not configured for it.
		return d, nil
	}
	if err != nil {
		return Decision{}, err
	}

	d.StoreURL = p.StoreURL
	d.MinSupportedVersion = p.MinSupportedVersion
	d.MinRecommendedVersion = p.MinRecommendedVersion
	d.Action = p.Decide(clientVersion)
	d.Title = p.TitleFor(d.Action)
	d.Message = p.MessageFor(d.Action)
	return d, nil
}

func (u *useCase) List(ctx context.Context, actor Actor) ([]domain.MobileAppPolicy, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	return u.repo.List(ctx)
}

func (u *useCase) Save(ctx context.Context, actor Actor, platform domain.DevicePlatform, in SaveInput) (*domain.MobileAppPolicy, error) {
	if err := requirePlatform(actor); err != nil {
		return nil, err
	}
	if platform != domain.PlatformIOS && platform != domain.PlatformAndroid {
		return nil, fmt.Errorf("%w: unknown platform %q", domain.ErrValidation, string(platform))
	}
	if err := validatePatches(in); err != nil {
		return nil, err
	}

	p, err := u.repo.Get(ctx, platform)
	if errors.Is(err, domain.ErrNotFound) {
		// First write for a platform the seed never created. Start from an
		// empty policy rather than 404: an unconfigured platform must be
		// configurable, and an empty policy forces nobody.
		p = &domain.MobileAppPolicy{Platform: platform}
	} else if err != nil {
		return nil, err
	}
	before := *p

	applyInput(p, in)
	if err := validatePolicy(*p); err != nil {
		return nil, err
	}
	if err := u.repo.Upsert(ctx, p); err != nil {
		return nil, err
	}

	// The forcing threshold is the one setting in this repository that can lock
	// every guest out of the app at once. Its changes are logged with who did
	// it and what it was before, so an incident review does not depend on
	// somebody remembering.
	if before.MinSupportedVersion != p.MinSupportedVersion {
		u.log.Warn("mobile forced-update threshold changed",
			slog.String("platform", string(platform)),
			slog.String("actor_id", actor.UserID.String()),
			slog.String("from", before.MinSupportedVersion),
			slog.String("to", p.MinSupportedVersion))
	}
	return p, nil
}

// requirePlatform is the defense-in-depth role re-check. The transport already
// mounts these routes behind RequireRole(RoleAdmin); this makes sure a future
// re-mount on a wider group cannot hand the forced-update switch to venue
// staff — the same reasoning as usecase/cities.requirePlatform.
func requirePlatform(actor Actor) error {
	if actor.Role != domain.RoleAdmin {
		return fmt.Errorf("%w: only the platform may manage the mobile update policy", domain.ErrForbidden)
	}
	return nil
}

func validatePatches(in SaveInput) error {
	for field, patch := range map[string]domain.I18nPatch{
		"recommended_title_i18n":   in.RecommendedTitleI18n,
		"recommended_message_i18n": in.RecommendedMessageI18n,
		"required_title_i18n":      in.RequiredTitleI18n,
		"required_message_i18n":    in.RequiredMessageI18n,
	} {
		if err := patch.Validate(field); err != nil {
			return err
		}
	}
	return nil
}

// applyInput merges the patch onto the stored policy. Text fields go through
// domain.ApplyTranslations, which re-establishes the invariant the whole i18n
// scheme rests on: the plain column and i18n["ru"] are the same Russian text.
func applyInput(p *domain.MobileAppPolicy, in SaveInput) {
	if in.MinSupportedVersion != nil {
		p.MinSupportedVersion = strings.TrimSpace(*in.MinSupportedVersion)
	}
	if in.MinRecommendedVersion != nil {
		p.MinRecommendedVersion = strings.TrimSpace(*in.MinRecommendedVersion)
	}
	if in.StoreURL != nil {
		p.StoreURL = strings.TrimSpace(*in.StoreURL)
	}
	if in.RecommendedTitle != nil {
		p.RecommendedTitle = strings.TrimSpace(*in.RecommendedTitle)
	}
	if in.RecommendedMessage != nil {
		p.RecommendedMessage = strings.TrimSpace(*in.RecommendedMessage)
	}
	if in.RequiredTitle != nil {
		p.RequiredTitle = strings.TrimSpace(*in.RequiredTitle)
	}
	if in.RequiredMessage != nil {
		p.RequiredMessage = strings.TrimSpace(*in.RequiredMessage)
	}
	p.RecommendedTitleI18n = domain.ApplyTranslations(p.RecommendedTitleI18n, in.RecommendedTitleI18n, p.RecommendedTitle)
	p.RecommendedMessageI18n = domain.ApplyTranslations(p.RecommendedMessageI18n, in.RecommendedMessageI18n, p.RecommendedMessage)
	p.RequiredTitleI18n = domain.ApplyTranslations(p.RequiredTitleI18n, in.RequiredTitleI18n, p.RequiredTitle)
	p.RequiredMessageI18n = domain.ApplyTranslations(p.RequiredMessageI18n, in.RequiredMessageI18n, p.RequiredMessage)
}

// validatePolicy refuses a policy that would be dishonest at read time:
// a threshold nothing can parse (it would silently mean "no threshold"), a mode
// switched on with no wording or no store link (a blank modal with a dead
// button), and a forcing threshold ABOVE the recommending one (the recommended
// tier would then be unreachable — Decide checks required first).
func validatePolicy(p domain.MobileAppPolicy) error {
	if err := checkLen("store_url", p.StoreURL, maxStoreURLLen); err != nil {
		return err
	}
	if p.StoreURL != "" {
		u, err := url.Parse(p.StoreURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: store_url must be an absolute http(s) URL", domain.ErrValidation)
		}
	}
	for field, text := range map[string]string{
		"recommended_title": p.RecommendedTitle,
		"required_title":    p.RequiredTitle,
	} {
		if err := checkLen(field, text, maxTitleLen); err != nil {
			return err
		}
	}
	for field, text := range map[string]string{
		"recommended_message": p.RecommendedMessage,
		"required_message":    p.RequiredMessage,
	} {
		if err := checkLen(field, text, maxMessageLen); err != nil {
			return err
		}
	}
	for field, i18n := range map[string]domain.I18n{
		"recommended_title_i18n":   p.RecommendedTitleI18n,
		"required_title_i18n":      p.RequiredTitleI18n,
		"recommended_message_i18n": p.RecommendedMessageI18n,
		"required_message_i18n":    p.RequiredMessageI18n,
	} {
		limit := maxMessageLen
		if strings.Contains(field, "title") {
			limit = maxTitleLen
		}
		for lang, v := range i18n {
			if err := checkLen(field+"."+lang, v, limit); err != nil {
				return err
			}
		}
	}

	supported, hasSupported, err := parseThreshold("min_supported_version", p.MinSupportedVersion)
	if err != nil {
		return err
	}
	recommended, hasRecommended, err := parseThreshold("min_recommended_version", p.MinRecommendedVersion)
	if err != nil {
		return err
	}
	if hasSupported && hasRecommended && recommended.Less(supported) {
		return fmt.Errorf("%w: min_recommended_version must not be older than min_supported_version", domain.ErrValidation)
	}
	if hasSupported {
		if err := requireWording("required", p.RequiredTitle, p.RequiredMessage, p.StoreURL); err != nil {
			return err
		}
	}
	if hasRecommended {
		if err := requireWording("recommended", p.RecommendedTitle, p.RecommendedMessage, p.StoreURL); err != nil {
			return err
		}
	}
	return nil
}

// parseThreshold reads one configured threshold. An empty value is a legitimate
// "no threshold"; a non-empty value that cannot be parsed is refused HERE,
// because at read time it would silently degrade to the same "no threshold" and
// nobody would learn that the switch they flipped does nothing.
func parseThreshold(field, raw string) (domain.AppVersion, bool, error) {
	if raw == "" {
		return domain.AppVersion{}, false, nil
	}
	if err := checkLen(field, raw, maxVersionLen); err != nil {
		return domain.AppVersion{}, false, err
	}
	v, ok := domain.ParseAppVersion(raw)
	if !ok {
		return domain.AppVersion{}, false, fmt.Errorf(
			"%w: %s must look like 1, 1.5 or 1.5.1, got %q", domain.ErrValidation, field, raw)
	}
	return v, true, nil
}

func requireWording(mode, title, message, storeURL string) error {
	if title == "" || message == "" {
		return fmt.Errorf("%w: %s_title and %s_message must be set before the %s mode can be switched on",
			domain.ErrValidation, mode, mode, mode)
	}
	if storeURL == "" {
		return fmt.Errorf("%w: store_url must be set before any update prompt can be switched on",
			domain.ErrValidation)
	}
	return nil
}

func checkLen(field, v string, limit int) error {
	if len([]rune(v)) > limit {
		return fmt.Errorf("%w: %s is longer than %d characters", domain.ErrValidation, field, limit)
	}
	return nil
}

// Package consent is the application logic for a guest's data-processing consent
// and notification opt-out. Every operation is scoped by the caller's own user
// id — there is no cross-user read/write surface, so a caller can never reach
// another user's consent records or preferences. Routes must be registered on a
// group already protected by middleware.Auth (see transport/rest/consent).
//
// What is infrastructure (built here) vs. an owner/legal decision (data, not
// code): this package records WHATEVER consent_type/version the client sends
// (validated only as non-empty and within length), and exposes the current
// state. The canonical list of consent types, which are mandatory at signup,
// and the policy versions/text are deliberately NOT encoded here.
package consent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"backend-core/internal/domain"
)

// maxConsentTypeLen / maxVersionLen mirror the VARCHAR bounds in migration 0040.
const (
	maxConsentTypeLen = 64
	maxVersionLen     = 64
)

// RecordInput is a single grant/revoke decision to append to the log.
type RecordInput struct {
	ConsentType string
	Version     string
	Granted     bool
	Source      domain.ConsentSource
}

// PreferenceInput is the guest's desired notification opt-out state.
type PreferenceInput struct {
	NotificationsEnabled bool
	PushEnabled          bool
	EmailEnabled         bool
}

// Facade exposes the current user's consent and notification-preference operations.
type Facade interface {
	// Record appends one immutable grant/revoke record for the caller and
	// returns the stored record. It never mutates prior records — the log is
	// append-only.
	Record(ctx context.Context, userID uuid.UUID, in RecordInput) (*domain.ConsentRecord, error)
	// CurrentState returns the caller's effective consent state: the latest
	// record per consent_type.
	CurrentState(ctx context.Context, userID uuid.UUID) ([]domain.ConsentRecord, error)
	// Preferences returns the caller's notification opt-out (defaults when unset).
	Preferences(ctx context.Context, userID uuid.UUID) (domain.NotificationPreference, error)
	// SetPreferences replaces the caller's notification opt-out.
	SetPreferences(ctx context.Context, userID uuid.UUID, in PreferenceInput) (domain.NotificationPreference, error)
}

type facade struct {
	consents domain.UserConsentRepository
	prefs    domain.UserNotificationPreferenceRepository
}

// NewFacade constructs the consent Facade.
func NewFacade(
	consents domain.UserConsentRepository,
	prefs domain.UserNotificationPreferenceRepository,
) Facade {
	return &facade{consents: consents, prefs: prefs}
}

func (f *facade) Record(ctx context.Context, userID uuid.UUID, in RecordInput) (*domain.ConsentRecord, error) {
	consentType := strings.TrimSpace(in.ConsentType)
	version := strings.TrimSpace(in.Version)
	if consentType == "" {
		return nil, fmt.Errorf("%w: consent_type must not be empty", domain.ErrValidation)
	}
	if len(consentType) > maxConsentTypeLen {
		return nil, fmt.Errorf("%w: consent_type too long (max %d)", domain.ErrValidation, maxConsentTypeLen)
	}
	if version == "" {
		return nil, fmt.Errorf("%w: version must not be empty", domain.ErrValidation)
	}
	if len(version) > maxVersionLen {
		return nil, fmt.Errorf("%w: version too long (max %d)", domain.ErrValidation, maxVersionLen)
	}
	if !domain.ValidConsentSource(in.Source) {
		return nil, fmt.Errorf("%w: source must be one of app, web", domain.ErrValidation)
	}

	rec := &domain.ConsentRecord{
		ID:          uuid.New(),
		UserID:      userID,
		ConsentType: consentType,
		Version:     version,
		Granted:     in.Granted,
		Source:      in.Source,
	}
	if err := f.consents.Append(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (f *facade) CurrentState(ctx context.Context, userID uuid.UUID) ([]domain.ConsentRecord, error) {
	return f.consents.CurrentState(ctx, userID)
}

func (f *facade) Preferences(ctx context.Context, userID uuid.UUID) (domain.NotificationPreference, error) {
	return f.prefs.Get(ctx, userID)
}

func (f *facade) SetPreferences(ctx context.Context, userID uuid.UUID, in PreferenceInput) (domain.NotificationPreference, error) {
	pref := domain.NotificationPreference{
		UserID:               userID,
		NotificationsEnabled: in.NotificationsEnabled,
		PushEnabled:          in.PushEnabled,
		EmailEnabled:         in.EmailEnabled,
	}
	if err := f.prefs.Upsert(ctx, pref); err != nil {
		return domain.NotificationPreference{}, err
	}
	return f.prefs.Get(ctx, userID)
}

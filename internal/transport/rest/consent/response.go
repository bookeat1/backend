package consent

import (
	"time"

	"backend-core/internal/domain"
)

type consentResponse struct {
	ID          string    `json:"id" example:"550e8400-e29b-41d4-a716-446655440000"`
	ConsentType string    `json:"consent_type" example:"privacy_policy"`
	Version     string    `json:"version" example:"2026-07-01"`
	Granted     bool      `json:"granted" example:"true"`
	Source      string    `json:"source" example:"app"`
	CreatedAt   time.Time `json:"created_at" example:"2026-07-24T09:00:00Z"`
}

func fromConsent(r domain.ConsentRecord) consentResponse {
	return consentResponse{
		ID:          r.ID.String(),
		ConsentType: r.ConsentType,
		Version:     r.Version,
		Granted:     r.Granted,
		Source:      string(r.Source),
		CreatedAt:   r.CreatedAt,
	}
}

type preferenceResponse struct {
	NotificationsEnabled bool      `json:"notifications_enabled" example:"true"`
	PushEnabled          bool      `json:"push_enabled" example:"true"`
	EmailEnabled         bool      `json:"email_enabled" example:"true"`
	UpdatedAt            time.Time `json:"updated_at" example:"2026-07-24T09:00:00Z"`
}

func fromPreference(p domain.NotificationPreference) preferenceResponse {
	return preferenceResponse{
		NotificationsEnabled: p.NotificationsEnabled,
		PushEnabled:          p.PushEnabled,
		EmailEnabled:         p.EmailEnabled,
		UpdatedAt:            p.UpdatedAt,
	}
}

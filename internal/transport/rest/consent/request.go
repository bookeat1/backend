package consent

import (
	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/consent"
)

// recordRequest is one grant/revoke decision. consent_type and version are
// free-form strings (validated non-empty in the usecase) — the canonical list
// and versioning policy are an owner/legal decision, not fixed by this API.
type recordRequest struct {
	ConsentType string `json:"consent_type" example:"privacy_policy"`
	Version     string `json:"version" example:"2026-07-01"`
	Granted     bool   `json:"granted" example:"true"`
	// Source is where the decision was captured: "app" or "web".
	Source string `json:"source" example:"app"`
}

func (r recordRequest) toInput() uc.RecordInput {
	return uc.RecordInput{
		ConsentType: r.ConsentType,
		Version:     r.Version,
		Granted:     r.Granted,
		Source:      domain.ConsentSource(r.Source),
	}
}

// preferenceRequest is a full replacement of the caller's notification opt-out.
// Each flag is a *bool so an omitted field defaults to true (enabled) rather
// than to Go's false zero-value — a client sending only notifications_enabled
// does not accidentally silence every channel.
type preferenceRequest struct {
	NotificationsEnabled *bool `json:"notifications_enabled" example:"true"`
	PushEnabled          *bool `json:"push_enabled" example:"true"`
	EmailEnabled         *bool `json:"email_enabled" example:"true"`
}

func (r preferenceRequest) toInput() uc.PreferenceInput {
	return uc.PreferenceInput{
		NotificationsEnabled: boolOrTrue(r.NotificationsEnabled),
		PushEnabled:          boolOrTrue(r.PushEnabled),
		EmailEnabled:         boolOrTrue(r.EmailEnabled),
	}
}

func boolOrTrue(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

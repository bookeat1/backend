package appversion

import (
	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/appversion"
)

// checkResponse is what a launching app reads.
//
// Action is the ONLY field a client must branch on: "none" | "recommended" |
// "required". Title/Message are whole {ru,kk,en} objects and are omitted for
// "none" — the client selects the language itself, so the answer stays
// independent of Accept-Language and cacheable by URL.
type checkResponse struct {
	Platform string `json:"platform"`
	Action   string `json:"action"`
	StoreURL string `json:"store_url,omitempty"`
	// The configured thresholds, echoed back so a support engineer can tell
	// from a single response WHY the guest was told what they were told.
	MinSupportedVersion   string            `json:"min_supported_version,omitempty"`
	MinRecommendedVersion string            `json:"min_recommended_version,omitempty"`
	Title                 map[string]string `json:"title,omitempty"`
	Message               map[string]string `json:"message,omitempty"`
}

func toCheckResponse(d uc.Decision) checkResponse {
	return checkResponse{
		Platform:              string(d.Platform),
		Action:                string(d.Action),
		StoreURL:              d.StoreURL,
		MinSupportedVersion:   d.MinSupportedVersion,
		MinRecommendedVersion: d.MinRecommendedVersion,
		Title:                 d.Title,
		Message:               d.Message,
	}
}

// policyResponse is the row as its OWNER sees it: both thresholds, both modes'
// wording, every translation. Nothing is resolved to a single language here —
// an editor has to see what is actually stored.
type policyResponse struct {
	Platform               string            `json:"platform"`
	MinSupportedVersion    string            `json:"min_supported_version"`
	MinRecommendedVersion  string            `json:"min_recommended_version"`
	StoreURL               string            `json:"store_url"`
	RecommendedTitle       string            `json:"recommended_title"`
	RecommendedTitleI18n   map[string]string `json:"recommended_title_i18n,omitempty"`
	RecommendedMessage     string            `json:"recommended_message"`
	RecommendedMessageI18n map[string]string `json:"recommended_message_i18n,omitempty"`
	RequiredTitle          string            `json:"required_title"`
	RequiredTitleI18n      map[string]string `json:"required_title_i18n,omitempty"`
	RequiredMessage        string            `json:"required_message"`
	RequiredMessageI18n    map[string]string `json:"required_message_i18n,omitempty"`
	UpdatedAt              string            `json:"updated_at,omitempty"`
}

func toPolicyResponse(p domain.MobileAppPolicy) policyResponse {
	r := policyResponse{
		Platform:               string(p.Platform),
		MinSupportedVersion:    p.MinSupportedVersion,
		MinRecommendedVersion:  p.MinRecommendedVersion,
		StoreURL:               p.StoreURL,
		RecommendedTitle:       p.RecommendedTitle,
		RecommendedTitleI18n:   p.RecommendedTitleI18n,
		RecommendedMessage:     p.RecommendedMessage,
		RecommendedMessageI18n: p.RecommendedMessageI18n,
		RequiredTitle:          p.RequiredTitle,
		RequiredTitleI18n:      p.RequiredTitleI18n,
		RequiredMessage:        p.RequiredMessage,
		RequiredMessageI18n:    p.RequiredMessageI18n,
	}
	if !p.UpdatedAt.IsZero() {
		r.UpdatedAt = p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return r
}

func toPolicyResponses(items []domain.MobileAppPolicy) []policyResponse {
	out := make([]policyResponse, 0, len(items))
	for _, p := range items {
		out = append(out, toPolicyResponse(p))
	}
	return out
}

// saveRequest is the admin write body. Every scalar is a pointer so an absent
// field is preserved rather than blanked (the full-replace shape is exactly how
// admin writes have wiped fields in this codebase before), and every *_i18n is
// a PARTIAL patch: a key absent leaves that language alone, a key with null or
// "" removes it.
//
// Switching a mode OFF is `"min_supported_version": ""` — an empty threshold
// means "no threshold", never "no change".
type saveRequest struct {
	MinSupportedVersion   *string `json:"min_supported_version"`
	MinRecommendedVersion *string `json:"min_recommended_version"`
	StoreURL              *string `json:"store_url"`

	RecommendedTitle       *string            `json:"recommended_title"`
	RecommendedTitleI18n   map[string]*string `json:"recommended_title_i18n"`
	RecommendedMessage     *string            `json:"recommended_message"`
	RecommendedMessageI18n map[string]*string `json:"recommended_message_i18n"`
	RequiredTitle          *string            `json:"required_title"`
	RequiredTitleI18n      map[string]*string `json:"required_title_i18n"`
	RequiredMessage        *string            `json:"required_message"`
	RequiredMessageI18n    map[string]*string `json:"required_message_i18n"`
}

func (r saveRequest) toInput() uc.SaveInput {
	return uc.SaveInput{
		MinSupportedVersion:    r.MinSupportedVersion,
		MinRecommendedVersion:  r.MinRecommendedVersion,
		StoreURL:               r.StoreURL,
		RecommendedTitle:       r.RecommendedTitle,
		RecommendedTitleI18n:   domain.I18nPatch(r.RecommendedTitleI18n),
		RecommendedMessage:     r.RecommendedMessage,
		RecommendedMessageI18n: domain.I18nPatch(r.RecommendedMessageI18n),
		RequiredTitle:          r.RequiredTitle,
		RequiredTitleI18n:      domain.I18nPatch(r.RequiredTitleI18n),
		RequiredMessage:        r.RequiredMessage,
		RequiredMessageI18n:    domain.I18nPatch(r.RequiredMessageI18n),
	}
}

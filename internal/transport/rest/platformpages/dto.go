package platformpages

import (
	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/platformpages"
)

const timeLayout = "2006-01-02T15:04:05Z07:00"

// publicResponse is what the guest-facing site reads: GET /pages/:slug.
// Title/Body are already resolved to the caller's language (reqlocale +
// I18n.Resolve, done by the handler) — the raw *_i18n maps never reach an
// unauthenticated caller.
type publicResponse struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Format    string `json:"format"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func toPublicResponse(p domain.PlatformPage, lang string) publicResponse {
	r := publicResponse{
		Slug:   string(p.Slug),
		Title:  p.TitleI18n.Resolve(lang, p.Title),
		Body:   p.BodyI18n.Resolve(lang, p.Body),
		Format: p.Format,
	}
	if !p.UpdatedAt.IsZero() {
		r.UpdatedAt = p.UpdatedAt.UTC().Format(timeLayout)
	}
	return r
}

// adminResponse is what the cabinet reads: every field as actually stored,
// published or not, translations included — an editor has to see what is
// really there, not a resolved-to-one-language view.
type adminResponse struct {
	Slug      string            `json:"slug"`
	Title     string            `json:"title"`
	TitleI18n map[string]string `json:"title_i18n,omitempty"`
	Body      string            `json:"body"`
	BodyI18n  map[string]string `json:"body_i18n,omitempty"`
	Format    string            `json:"format"`
	Published bool              `json:"published"`
	UpdatedBy *string           `json:"updated_by,omitempty"`
	CreatedAt string            `json:"created_at,omitempty"`
	UpdatedAt string            `json:"updated_at,omitempty"`
}

func toAdminResponse(p domain.PlatformPage) adminResponse {
	r := adminResponse{
		Slug:      string(p.Slug),
		Title:     p.Title,
		TitleI18n: p.TitleI18n,
		Body:      p.Body,
		BodyI18n:  p.BodyI18n,
		Format:    p.Format,
		Published: p.Published(),
	}
	if p.UpdatedBy != nil {
		s := p.UpdatedBy.String()
		r.UpdatedBy = &s
	}
	if !p.CreatedAt.IsZero() {
		r.CreatedAt = p.CreatedAt.UTC().Format(timeLayout)
	}
	if !p.UpdatedAt.IsZero() {
		r.UpdatedAt = p.UpdatedAt.UTC().Format(timeLayout)
	}
	return r
}

func toAdminResponses(items []domain.PlatformPage) []adminResponse {
	out := make([]adminResponse, 0, len(items))
	for _, p := range items {
		out = append(out, toAdminResponse(p))
	}
	return out
}

// updateRequest is the admin write body: PATCH semantics on PUT, same
// convention as usecase/appversion.SaveInput. Every scalar is a pointer so an
// absent field is preserved rather than blanked, and the *_i18n objects are
// PARTIAL translation patches: a key absent leaves that language alone, a key
// with null or "" removes it.
type updateRequest struct {
	Title     *string            `json:"title"`
	TitleI18n map[string]*string `json:"title_i18n"`
	Body      *string            `json:"body"`
	BodyI18n  map[string]*string `json:"body_i18n"`
	Published *bool              `json:"published"`
}

func (r updateRequest) toInput() uc.UpdateInput {
	return uc.UpdateInput{
		Title:     r.Title,
		TitleI18n: domain.I18nPatch(r.TitleI18n),
		Body:      r.Body,
		BodyI18n:  domain.I18nPatch(r.BodyI18n),
		Published: r.Published,
	}
}

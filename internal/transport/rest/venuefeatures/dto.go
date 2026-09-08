package venuefeatures

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/venuefeatures"
)

// saveRequest is the create/update body. Every field is a pointer so PATCH can
// tell "not mentioned" (preserve) from "explicitly set", matching the venue
// PATCH convention in the restaurants package.
type saveRequest struct {
	Code         *string           `json:"code"`
	Name         *string           `json:"name"`
	NameI18n     map[string]string `json:"name_i18n"`
	DisplayOrder *int              `json:"display_order"`
	IsActive     *bool             `json:"is_active"`
}

func (r saveRequest) toInput() uc.SaveInput {
	in := uc.SaveInput{
		Code:         r.Code,
		Name:         r.Name,
		DisplayOrder: r.DisplayOrder,
		IsActive:     r.IsActive,
	}
	if r.NameI18n != nil {
		in.NameI18n = domain.I18n(r.NameI18n)
	}
	return in
}

// setVenueRequest is the venue's whole feature set.
type setVenueRequest struct {
	FeatureIDs []string `json:"feature_ids"`
}

// ids parses the request's ids, failing the WHOLE call on the first bad one.
// Skipping an unparseable id would answer "saved" for a set the venue never
// asked for — the same rule ResolveIDs applies to ids that simply do not exist.
func (r setVenueRequest) ids() ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(r.FeatureIDs))
	for _, s := range r.FeatureIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid venue feature id %q", domain.ErrValidation, s)
		}
		out = append(out, id)
	}
	return out, nil
}

// featureResponse is the dictionary entry as clients read it. It matches the
// `features[]` element embedded in the restaurant payload field for field, so
// a client writes one mapper — plus display_order / is_active / venue_count,
// which the platform's own management screen needs.
//
// venue_count is NOT omitempty: zero is the interesting value here (a feature
// no venue carries yet), and omitting it would make "нет данных" look like
// "поле не поддерживается".
type featureResponse struct {
	ID           string            `json:"id"`
	Code         string            `json:"code"`
	Name         string            `json:"name"`
	NameI18n     map[string]string `json:"name_i18n,omitempty"`
	DisplayOrder int               `json:"display_order"`
	IsActive     bool              `json:"is_active"`
	VenueCount   int               `json:"venue_count"`
}

func toResponse(vf domain.VenueFeature, lang string) featureResponse {
	return featureResponse{
		ID:           vf.ID.String(),
		Code:         vf.Code,
		Name:         vf.NameI18n.Resolve(lang, vf.Name),
		NameI18n:     vf.NameI18n,
		DisplayOrder: vf.DisplayOrder,
		IsActive:     vf.IsActive,
		VenueCount:   vf.VenueCount,
	}
}

func toResponses(items []domain.VenueFeature, lang string) []featureResponse {
	out := make([]featureResponse, 0, len(items))
	for _, vf := range items {
		out = append(out, toResponse(vf, lang))
	}
	return out
}

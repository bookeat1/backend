package cuisines

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/cuisines"
)

// saveRequest is the create/update body. Every field is a pointer so PATCH can
// tell "not mentioned" (preserve) from "explicitly set", matching the venue
// PATCH convention in the restaurants package.
type saveRequest struct {
	Code         *string           `json:"code"`
	Name         *string           `json:"name"`
	NameI18n     map[string]string `json:"name_i18n"`
	ImageURL     *string           `json:"image_url"`
	DisplayOrder *int              `json:"display_order"`
	IsActive     *bool             `json:"is_active"`
}

func (r saveRequest) toInput() uc.SaveInput {
	in := uc.SaveInput{
		Code:         r.Code,
		Name:         r.Name,
		ImageURL:     r.ImageURL,
		DisplayOrder: r.DisplayOrder,
		IsActive:     r.IsActive,
	}
	if r.NameI18n != nil {
		in.NameI18n = domain.I18n(r.NameI18n)
	}
	return in
}

// setVenueRequest is the venue's whole cuisine set. Order is meaningful: the
// first id is the venue's main cuisine (what a card with room for one shows).
type setVenueRequest struct {
	CuisineIDs []string `json:"cuisine_ids"`
}

// ids parses the request's ids, failing the WHOLE call on the first bad one.
// Skipping an unparseable id would answer "saved" for a set the venue never
// asked for — the same rule ResolveIDs applies to ids that simply do not exist.
func (r setVenueRequest) ids() ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(r.CuisineIDs))
	for _, s := range r.CuisineIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cuisine id %q", domain.ErrValidation, s)
		}
		out = append(out, id)
	}
	return out, nil
}

// cuisineResponse is the dictionary entry as clients read it. It matches the
// `cuisines[]` element embedded in the restaurant payload field for field, so
// a client writes one mapper — plus is_active/display_order, which only the
// platform's own management screen has any use for.
type cuisineResponse struct {
	ID           string            `json:"id"`
	Code         string            `json:"code"`
	Name         string            `json:"name"`
	NameI18n     map[string]string `json:"name_i18n,omitempty"`
	ImageURL     *string           `json:"image_url,omitempty"`
	DisplayOrder int               `json:"display_order"`
	IsActive     bool              `json:"is_active"`
}

func toResponse(c domain.Cuisine, lang string) cuisineResponse {
	return cuisineResponse{
		ID: c.ID.String(), Code: c.Code,
		Name:         c.NameI18n.Resolve(lang, c.Name),
		NameI18n:     c.NameI18n,
		ImageURL:     c.ImageURL,
		DisplayOrder: c.DisplayOrder,
		IsActive:     c.IsActive,
	}
}

func toResponses(items []domain.Cuisine, lang string) []cuisineResponse {
	out := make([]cuisineResponse, 0, len(items))
	for _, c := range items {
		out = append(out, toResponse(c, lang))
	}
	return out
}

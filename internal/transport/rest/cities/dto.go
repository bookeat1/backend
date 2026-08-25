package cities

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/cities"
)

// saveRequest is the create/update body. Every field is a pointer so PATCH can
// tell "not mentioned" (preserve) from "explicitly set", matching the venue and
// cuisine PATCH convention.
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

// reorderRequest is the whole display order, as a sequence of ids. A full
// sequence rather than a "move id to position n": two admins dragging rows at
// the same time would otherwise interleave into an order neither of them chose.
type reorderRequest struct {
	CityIDs []string `json:"city_ids"`
}

// ids parses the request's ids, failing the WHOLE call on the first bad one —
// answering 200 for a partially understood order would show the caller a list
// they never asked for.
func (r reorderRequest) ids() ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(r.CityIDs))
	for _, s := range r.CityIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid city id %q", domain.ErrValidation, s)
		}
		out = append(out, id)
	}
	return out, nil
}

// aliasRequest registers an extra spelling for a city.
type aliasRequest struct {
	Alias string `json:"alias"`
}

// cityResponse is the dictionary entry as clients read it. Shape mirrors
// cuisineResponse field for field (minus the image), so a client writes one
// mapper for both dictionaries.
type cityResponse struct {
	ID       string            `json:"id"`
	Code     string            `json:"code"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
	// Value is what a client must send back as ?city=. It is the BASE Russian
	// name, never the translated one: the catalog filter compares it to the
	// stored restaurants.city string, and a client that echoed the localized
	// name would silently find nothing. Making it an explicit field means a
	// client never has to know that rule.
	Value        string `json:"value"`
	DisplayOrder int    `json:"display_order"`
	IsActive     bool   `json:"is_active"`
}

func toResponse(c domain.CityEntry, lang string) cityResponse {
	return cityResponse{
		ID:           c.ID.String(),
		Code:         c.Code,
		Name:         c.NameI18n.Resolve(lang, c.Name),
		NameI18n:     c.NameI18n,
		Value:        c.Name,
		DisplayOrder: c.DisplayOrder,
		IsActive:     c.IsActive,
	}
}

func toResponses(items []domain.CityEntry, lang string) []cityResponse {
	out := make([]cityResponse, 0, len(items))
	for _, c := range items {
		out = append(out, toResponse(c, lang))
	}
	return out
}

// toNames renders the LEGACY response body: a bare array of Russian city names
// in dictionary order. This is the exact shape GET /cities has always returned
// (`{"data":["Астана","Алматы"]}`) and the build currently in the store parses
// it as such — it is a frozen contract, not a default worth improving.
//
// Base names on purpose, never translated: the same string comes back as
// ?city= and is compared to restaurants.city.
func toNames(items []domain.CityEntry) []string {
	out := make([]string, 0, len(items))
	for _, c := range items {
		out = append(out, c.Name)
	}
	return out
}

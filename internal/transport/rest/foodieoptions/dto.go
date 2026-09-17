package foodieoptions

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/foodieoptions"
)

// saveRequest is the create/update body. Every field is a pointer so PATCH
// can tell "not mentioned" (preserve) from "explicitly set" — same
// convention as cuisines.saveRequest, extended with the fields §5 of the
// spec adds for this dictionary (kind, budget's three extra fields, the
// cuisine-tile link).
type saveRequest struct {
	Kind            *string           `json:"kind" example:"cuisine"`
	Code            *string           `json:"code" example:"korean"`
	Name            *string           `json:"name" example:"Корейская"`
	NameI18n        map[string]string `json:"name_i18n"`
	ImageURL        *string           `json:"image_url"`
	Description     *string           `json:"description"`
	DescriptionI18n map[string]string `json:"description_i18n"`
	PriceLabel      *string           `json:"price_label"`
	PriceLabelI18n  map[string]string `json:"price_label_i18n"`
	// PriceCategory: omitted = unchanged, "" = explicitly cleared (a budget
	// tier with no ₸-equivalent, spec 3.10), "₸"/"₸₸"/"₸₸₸" = set. Only
	// meaningful for kind=budget — the usecase 422s it on any other kind.
	PriceCategory *string `json:"price_category" example:"₸₸"`
	// CuisineIDs is the cuisine-tile link (🔴1 = A), only meaningful for
	// kind=cuisine. Omitted = unchanged; present (including an empty array)
	// = replace the link set wholesale.
	CuisineIDs   *[]string `json:"cuisine_ids"`
	DisplayOrder *int      `json:"display_order"`
	IsActive     *bool     `json:"is_active"`
}

func (r saveRequest) toInput() (uc.SaveInput, error) {
	in := uc.SaveInput{
		Code: r.Code, Name: r.Name, ImageURL: r.ImageURL,
		Description: r.Description, PriceLabel: r.PriceLabel, PriceCategory: r.PriceCategory,
		DisplayOrder: r.DisplayOrder, IsActive: r.IsActive,
	}
	if r.Kind != nil {
		k := domain.FoodieOptionKind(*r.Kind)
		in.Kind = &k
	}
	if r.NameI18n != nil {
		in.NameI18n = domain.I18n(r.NameI18n)
	}
	if r.DescriptionI18n != nil {
		in.DescriptionI18n = domain.I18n(r.DescriptionI18n)
	}
	if r.PriceLabelI18n != nil {
		in.PriceLabelI18n = domain.I18n(r.PriceLabelI18n)
	}
	if r.CuisineIDs != nil {
		ids := make([]uuid.UUID, 0, len(*r.CuisineIDs))
		for _, s := range *r.CuisineIDs {
			id, err := uuid.Parse(s)
			if err != nil {
				return uc.SaveInput{}, fmt.Errorf("%w: invalid cuisine_ids entry %q", domain.ErrValidation, s)
			}
			ids = append(ids, id)
		}
		in.CuisineIDs = &ids
	}
	return in, nil
}

// publicOptionResponse is one entry as the wizard reads it (spec §5's public
// contract): the four base fields every kind has, plus the three budget-only
// fields (nil/omitted for cuisine/diet/allergy).
type publicOptionResponse struct {
	ID              string            `json:"id"`
	Code            string            `json:"code"`
	Name            string            `json:"name"`
	NameI18n        map[string]string `json:"name_i18n,omitempty"`
	ImageURL        *string           `json:"image_url,omitempty"`
	DisplayOrder    int               `json:"display_order"`
	Description     *string           `json:"description,omitempty"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	PriceLabel      *string           `json:"price_label,omitempty"`
	PriceLabelI18n  map[string]string `json:"price_label_i18n,omitempty"`
	PriceCategory   *string           `json:"price_category,omitempty"`
}

// publicOptionsResponse is GET /foodie-profile/options' whole body: the four
// buckets the wizard's four steps read one-to-one.
type publicOptionsResponse struct {
	Cuisines  []publicOptionResponse `json:"cuisines"`
	Diets     []publicOptionResponse `json:"diets"`
	Allergies []publicOptionResponse `json:"allergies"`
	Budgets   []publicOptionResponse `json:"budgets"`
}

func toPublicOption(o domain.FoodieOption, lang string) publicOptionResponse {
	out := publicOptionResponse{
		ID: o.ID.String(), Code: o.Code,
		Name:         o.NameI18n.Resolve(lang, o.Name),
		NameI18n:     o.NameI18n,
		ImageURL:     o.ImageURL,
		DisplayOrder: o.DisplayOrder,
	}
	if o.Kind == domain.FoodieOptionKindBudget {
		out.Description = o.Description
		out.DescriptionI18n = o.DescriptionI18n
		out.PriceLabel = o.PriceLabel
		out.PriceLabelI18n = o.PriceLabelI18n
		if o.PriceCategory != nil {
			s := string(*o.PriceCategory)
			out.PriceCategory = &s
		}
	}
	return out
}

func toPublicOptions(items []domain.FoodieOption, lang string) publicOptionsResponse {
	out := publicOptionsResponse{
		Cuisines: []publicOptionResponse{}, Diets: []publicOptionResponse{},
		Allergies: []publicOptionResponse{}, Budgets: []publicOptionResponse{},
	}
	for _, o := range items {
		item := toPublicOption(o, lang)
		switch o.Kind {
		case domain.FoodieOptionKindCuisine:
			out.Cuisines = append(out.Cuisines, item)
		case domain.FoodieOptionKindDiet:
			out.Diets = append(out.Diets, item)
		case domain.FoodieOptionKindAllergy:
			out.Allergies = append(out.Allergies, item)
		case domain.FoodieOptionKindBudget:
			out.Budgets = append(out.Budgets, item)
		}
	}
	return out
}

// adminOptionResponse is one entry as the platform's own management screen
// reads it: everything publicOptionResponse has, plus Kind/IsActive/the
// cuisine-tile link/AffectsMatching/timestamps (spec §5's admin contract:
// "то же + kind, is_active, cuisine_ids, cuisine_codes, affects_matching,
// created_at, updated_at").
type adminOptionResponse struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Code            string            `json:"code"`
	Name            string            `json:"name"`
	NameI18n        map[string]string `json:"name_i18n,omitempty"`
	ImageURL        *string           `json:"image_url,omitempty"`
	Description     *string           `json:"description,omitempty"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	PriceLabel      *string           `json:"price_label,omitempty"`
	PriceLabelI18n  map[string]string `json:"price_label_i18n,omitempty"`
	PriceCategory   *string           `json:"price_category,omitempty"`
	// CuisineIDs/CuisineCodes are non-nil only for kind=cuisine (spec §5:
	// "у кухонь"); always [] rather than omitted for the other three kinds,
	// so the admin table's column renders "—" from an empty array uniformly
	// rather than branching on a missing key.
	CuisineIDs      []string  `json:"cuisine_ids"`
	CuisineCodes    []string  `json:"cuisine_codes"`
	DisplayOrder    int       `json:"display_order"`
	IsActive        bool      `json:"is_active"`
	AffectsMatching bool      `json:"affects_matching"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func toAdminOption(o domain.FoodieOption, lang string) adminOptionResponse {
	pub := toPublicOption(o, lang)
	out := adminOptionResponse{
		ID: pub.ID, Kind: string(o.Kind), Code: pub.Code, Name: pub.Name, NameI18n: pub.NameI18n,
		ImageURL: pub.ImageURL, Description: pub.Description, DescriptionI18n: pub.DescriptionI18n,
		PriceLabel: pub.PriceLabel, PriceLabelI18n: pub.PriceLabelI18n, PriceCategory: pub.PriceCategory,
		CuisineIDs: []string{}, CuisineCodes: []string{},
		DisplayOrder: o.DisplayOrder, IsActive: o.IsActive, AffectsMatching: o.AffectsMatching(),
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
	for _, c := range o.Cuisines {
		out.CuisineIDs = append(out.CuisineIDs, c.ID.String())
		out.CuisineCodes = append(out.CuisineCodes, c.Code)
	}
	return out
}

// adminOptionsResponse is GET /admin/foodie-profile/options' whole body —
// the same four-bucket shape as the public route (spec §5), so the admin
// screen's four tabs read it directly without a client-side kind filter.
type adminOptionsResponse struct {
	Cuisines  []adminOptionResponse `json:"cuisines"`
	Diets     []adminOptionResponse `json:"diets"`
	Allergies []adminOptionResponse `json:"allergies"`
	Budgets   []adminOptionResponse `json:"budgets"`
}

func toAdminOptions(items []domain.FoodieOption, lang string) adminOptionsResponse {
	out := adminOptionsResponse{
		Cuisines: []adminOptionResponse{}, Diets: []adminOptionResponse{},
		Allergies: []adminOptionResponse{}, Budgets: []adminOptionResponse{},
	}
	for _, o := range items {
		item := toAdminOption(o, lang)
		switch o.Kind {
		case domain.FoodieOptionKindCuisine:
			out.Cuisines = append(out.Cuisines, item)
		case domain.FoodieOptionKindDiet:
			out.Diets = append(out.Diets, item)
		case domain.FoodieOptionKindAllergy:
			out.Allergies = append(out.Allergies, item)
		case domain.FoodieOptionKindBudget:
			out.Budgets = append(out.Budgets, item)
		}
	}
	return out
}

package restaurants

import (
	"fmt"

	"github.com/google/uuid"

	"backend-core/internal/domain"
	uc "backend-core/internal/usecase/restaurants"
)

// saveRestaurantRequest is the PATCH/POST body. Scalar fields are pointers so an
// absent JSON key (nil) is distinguishable from an explicit empty value: on
// Update the facade preserves omitted fields (read-modify-write) instead of
// wiping them.
type saveRestaurantRequest struct {
	CategoryID    *string           `json:"category_id"`
	Name          *string           `json:"name"`
	NameI18n      map[string]string `json:"name_i18n"`
	Description   *string           `json:"description"`
	CuisineType   *string           `json:"cuisine_type"`
	Address       *string           `json:"address"`
	OpeningHours  *string           `json:"opening_hours"`
	City          *string           `json:"city"`
	PriceCategory *string           `json:"price_category"`
	Email         *string           `json:"email"`
	Phone         *string           `json:"phone"`
	Latitude      *float64          `json:"latitude"`
	Longitude     *float64          `json:"longitude"`
	IsActive      *bool             `json:"is_active"`
	IsNew         *bool             `json:"is_new"`
	IsPopular     *bool             `json:"is_popular"`
	IsPremium     *bool             `json:"is_premium"`
	DisplayOrder  *int              `json:"display_order"`
	Images        []imageInput      `json:"images"`
	Features      []featureInput    `json:"features"`
	Tags          []tagInput        `json:"tags"`
	SocialLinks   []socialInput     `json:"social_links"`
}

type imageInput struct {
	ImageURL  string `json:"image_url"`
	IsPrimary bool   `json:"is_primary"`
}
type featureInput struct {
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n"`
}
type tagInput struct {
	TagName     string            `json:"tag_name"`
	TagNameI18n map[string]string `json:"tag_name_i18n"`
}
type socialInput struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// toInput maps the request to uc.SaveInput with opt-in semantics: a field or
// collection that is absent from the JSON body (nil) leaves the existing value
// untouched on Update; only fields/collections explicitly present are applied.
// A malformed category_id is a hard error (mapped to 422) rather than being
// silently dropped, so a typo can't slip through as "field omitted".
func (r saveRestaurantRequest) toInput() (uc.SaveInput, error) {
	in := uc.SaveInput{
		Name: r.Name, NameI18n: domain.I18n(r.NameI18n), Description: r.Description,
		CuisineType: r.CuisineType, Address: r.Address, OpeningHours: r.OpeningHours,
		City: r.City, PriceCategory: r.PriceCategory, Email: r.Email, Phone: r.Phone,
		Latitude: r.Latitude, Longitude: r.Longitude, IsActive: r.IsActive,
		IsNew: r.IsNew, IsPopular: r.IsPopular, IsPremium: r.IsPremium, DisplayOrder: r.DisplayOrder,
	}
	if r.CategoryID != nil {
		id, err := uuid.Parse(*r.CategoryID)
		if err != nil {
			return uc.SaveInput{}, fmt.Errorf("invalid category_id: %w", domain.ErrValidation)
		}
		in.CategoryID = &id
	}
	if r.Images != nil {
		imgs := make([]domain.Image, 0, len(r.Images))
		for _, i := range r.Images {
			imgs = append(imgs, domain.Image{ImageURL: i.ImageURL, IsPrimary: i.IsPrimary})
		}
		in.Images = &imgs
	}
	if r.Features != nil {
		feats := make([]domain.Feature, 0, len(r.Features))
		for _, f := range r.Features {
			feats = append(feats, domain.Feature{Name: f.Name, NameI18n: f.NameI18n})
		}
		in.Features = &feats
	}
	if r.Tags != nil {
		tags := make([]domain.Tag, 0, len(r.Tags))
		for _, t := range r.Tags {
			tags = append(tags, domain.Tag{TagName: t.TagName, TagNameI18n: t.TagNameI18n})
		}
		in.Tags = &tags
	}
	if r.SocialLinks != nil {
		socials := make([]domain.SocialLink, 0, len(r.SocialLinks))
		for _, s := range r.SocialLinks {
			socials = append(socials, domain.SocialLink{Type: s.Type, URL: s.URL})
		}
		in.SocialLinks = &socials
	}
	return in, nil
}

type partnershipRequest struct {
	RestaurantName string  `json:"restaurant_name"`
	ContactName    string  `json:"contact_name"`
	Email          string  `json:"email"`
	Phone          string  `json:"phone"`
	Address        string  `json:"address"`
	CuisineType    *string `json:"cuisine_type"`
	Description    *string `json:"description"`
	AdditionalInfo *string `json:"additional_info"`
}

func (r partnershipRequest) toInput() uc.PartnershipInput {
	return uc.PartnershipInput{
		RestaurantName: r.RestaurantName, ContactName: r.ContactName, Email: r.Email,
		Phone: r.Phone, Address: r.Address, CuisineType: r.CuisineType,
		Description: r.Description, AdditionalInfo: r.AdditionalInfo,
	}
}

type assignManagerRequest struct {
	UserID        string  `json:"user_id"`
	Role          string  `json:"role"` // one of: owner, manager, hostess
	WhatsappOptIn bool    `json:"whatsapp_opt_in"`
	WhatsappPhone *string `json:"whatsapp_phone"`
}

type setManagerRoleRequest struct {
	Role string `json:"role"` // one of: owner, manager, hostess
}

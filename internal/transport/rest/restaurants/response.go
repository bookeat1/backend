package restaurants

import (
	"time"

	"backend-core/internal/domain"
)

type restaurantResponse struct {
	ID            string            `json:"id"`
	CategoryID    *string           `json:"category_id"`
	Name          string            `json:"name"`
	NameI18n      map[string]string `json:"name_i18n,omitempty"`
	Description   string            `json:"description"`
	CuisineType   string            `json:"cuisine_type"`
	Address       string            `json:"address"`
	OpeningHours  string            `json:"opening_hours"`
	City          string            `json:"city"`
	PriceCategory string            `json:"price_category"`
	Email         string            `json:"email"`
	Phone         string            `json:"phone"`
	Latitude      *float64          `json:"latitude"`
	Longitude     *float64          `json:"longitude"`
	IsActive      bool              `json:"is_active"`
	IsNew         *bool             `json:"is_new"`
	IsPopular     *bool             `json:"is_popular"`
	IsPremium     *bool             `json:"is_premium"`
	DisplayOrder  *int              `json:"display_order"`
	PrimaryImage  *string           `json:"primary_image,omitempty"`
	Images        []imageResponse   `json:"images,omitempty"`
	Features      []featureResponse `json:"features,omitempty"`
	Tags          []tagResponse     `json:"tags,omitempty"`
	SocialLinks   []socialResponse  `json:"social_links,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	// IsFavorite is nil for an anonymous caller (omitted from the JSON
	// entirely) and an explicit true/false for an authenticated one — a
	// pointer so "not favorited" and "we don't know because you're not
	// logged in" are never confused with each other.
	IsFavorite *bool `json:"is_favorite,omitempty"`
}

type imageResponse struct {
	ID        string `json:"id"`
	ImageURL  string `json:"image_url"`
	IsPrimary bool   `json:"is_primary"`
}
type featureResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}
type tagResponse struct {
	ID          string            `json:"id"`
	TagName     string            `json:"tag_name"`
	TagNameI18n map[string]string `json:"tag_name_i18n,omitempty"`
}
type socialResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}
type categoryResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	NameI18n map[string]string `json:"name_i18n,omitempty"`
}
type managerResponse struct {
	ID            string  `json:"id"`
	RestaurantID  string  `json:"restaurant_id"`
	UserID        string  `json:"user_id"`
	WhatsappOptIn bool    `json:"whatsapp_opt_in"`
	WhatsappPhone *string `json:"whatsapp_phone"`
}

// baseFromDomain builds the response shape shared by the list/detail routes.
// lang is the resolved caller locale ("" when the caller asked for nothing —
// see resolveLocale). When lang is non-empty, the scalar fields fall back to
// the i18n column's translation for lang, or to the base value when that
// translation is missing (domain.I18n.Resolve); when lang is empty, the
// scalar fields are the base value exactly as before, so a caller that never
// asks for a language sees byte-identical output to before this feature.
func baseFromDomain(r domain.Restaurant, lang string) restaurantResponse {
	var cat *string
	if r.CategoryID != nil {
		s := r.CategoryID.String()
		cat = &s
	}
	return restaurantResponse{
		ID: r.ID.String(), CategoryID: cat,
		Name:         r.NameI18n.Resolve(lang, r.Name),
		NameI18n:     r.NameI18n,
		Description:  r.DescriptionI18n.Resolve(lang, r.Description),
		CuisineType:  r.CuisineTypeI18n.Resolve(lang, r.CuisineType),
		Address:      r.AddressI18n.Resolve(lang, r.Address),
		OpeningHours: r.OpeningHoursI18n.Resolve(lang, r.OpeningHours),
		City:         string(r.City), PriceCategory: string(r.PriceCategory),
		Email: r.Email, Phone: r.Phone, Latitude: r.Latitude, Longitude: r.Longitude,
		IsActive: r.IsActive, IsNew: r.IsNew, IsPopular: r.IsPopular, IsPremium: r.IsPremium,
		DisplayOrder: r.DisplayOrder, CreatedAt: r.CreatedAt,
	}
}

func listItemToResponse(it domain.RestaurantListItem, lang string) restaurantResponse {
	resp := baseFromDomain(it.Restaurant, lang)
	resp.PrimaryImage = it.PrimaryImage
	return resp
}

// PublicListItem builds the same public JSON shape the catalog listing uses,
// for a domain.RestaurantListItem read from elsewhere (the favorites
// transport package reads a user's bookmarked restaurants through
// favorites.Facade.List and must serialize them identically to the main
// catalog listing). Returned as the concrete response value (not a pointer)
// so json.Marshal/gin's c.JSON serialize it exactly like every other route in
// this package.
func PublicListItem(it domain.RestaurantListItem, lang string) any {
	return listItemToResponse(it, lang)
}

func aggregateToResponse(a *domain.RestaurantAggregate, lang string) restaurantResponse {
	resp := baseFromDomain(a.Restaurant, lang)
	for _, i := range a.Images {
		resp.Images = append(resp.Images, imageResponse{ID: i.ID.String(), ImageURL: i.ImageURL, IsPrimary: i.IsPrimary})
		if i.IsPrimary && resp.PrimaryImage == nil {
			u := i.ImageURL
			resp.PrimaryImage = &u
		}
	}
	for _, f := range a.Features {
		resp.Features = append(resp.Features, featureResponse{ID: f.ID.String(), Name: f.Name, NameI18n: f.NameI18n})
	}
	for _, t := range a.Tags {
		resp.Tags = append(resp.Tags, tagResponse{ID: t.ID.String(), TagName: t.TagName, TagNameI18n: t.TagNameI18n})
	}
	for _, s := range a.SocialLinks {
		resp.SocialLinks = append(resp.SocialLinks, socialResponse{ID: s.ID.String(), Type: s.Type, URL: s.URL})
	}
	return resp
}

func categoryToResponse(c domain.RestaurantCategory) categoryResponse {
	return categoryResponse{ID: c.ID.String(), Name: c.Name, NameI18n: c.NameI18n}
}

func managerToResponse(m domain.RestaurantManager) managerResponse {
	return managerResponse{
		ID: m.ID.String(), RestaurantID: m.RestaurantID.String(), UserID: m.UserID.String(),
		WhatsappOptIn: m.WhatsappOptIn, WhatsappPhone: m.WhatsappPhone,
	}
}

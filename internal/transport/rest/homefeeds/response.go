package homefeeds

import (
	"time"

	"backend-core/internal/domain"
)

type cuisineResponse struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ImageURL *string `json:"image_url"`
	Sort     int     `json:"sort"`
}

func cuisineToResponse(c domain.Cuisine) cuisineResponse {
	return cuisineResponse{ID: c.ID.String(), Name: c.Name, ImageURL: c.ImageURL, Sort: c.Sort}
}

type promotionResponse struct {
	ID            string     `json:"id"`
	RestaurantID  *string    `json:"restaurant_id"`
	Title         string     `json:"title"`
	DiscountLabel *string    `json:"discount_label"`
	StartsAt      *time.Time `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at"`
	ImageURL      *string    `json:"image_url"`
	Sort          int        `json:"sort"`
}

func promotionToResponse(p domain.Promotion) promotionResponse {
	var restaurantID *string
	if p.RestaurantID != nil {
		s := p.RestaurantID.String()
		restaurantID = &s
	}
	return promotionResponse{
		ID:            p.ID.String(),
		RestaurantID:  restaurantID,
		Title:         p.Title,
		DiscountLabel: p.DiscountLabel,
		StartsAt:      p.StartsAt,
		EndsAt:        p.EndsAt,
		ImageURL:      p.ImageURL,
		Sort:          p.Sort,
	}
}

type articleResponse struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	AuthorLabel *string    `json:"author_label"`
	CoverURL    *string    `json:"cover_url"`
	URL         *string    `json:"url"`
	PublishedAt *time.Time `json:"published_at"`
	Sort        int        `json:"sort"`
}

func articleToResponse(a domain.Article) articleResponse {
	return articleResponse{
		ID:          a.ID.String(),
		Title:       a.Title,
		AuthorLabel: a.AuthorLabel,
		CoverURL:    a.CoverURL,
		URL:         a.URL,
		PublishedAt: a.PublishedAt,
		Sort:        a.Sort,
	}
}

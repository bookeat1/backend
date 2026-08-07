package stories

import (
	"backend-core/internal/domain"
)

// storyResponse is one story card. Caption is omitted from the JSON when the
// venue set none (nil), so the client can distinguish "no caption" from an empty
// string. image_url is the full public URL, same convention as menu_items.
type storyResponse struct {
	ID        string  `json:"id"`
	ImageURL  string  `json:"image_url"`
	Caption   *string `json:"caption,omitempty"`
	SortOrder int     `json:"sort_order"`
}

func storyToResponse(s *domain.Story) storyResponse {
	return storyResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   s.Caption,
		SortOrder: s.SortOrder,
	}
}

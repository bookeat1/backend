package stories

import (
	"time"

	"backend-core/internal/domain"
)

// storyResponse is one story card. Caption is omitted from the JSON when the
// venue set none (nil), so the client can distinguish "no caption" from an empty
// string. image_url is the full public URL, same convention as menu_items.
// action_url is the EXTERNAL link a tap on the card follows; it is omitted when
// the venue set none, so a client can branch on its presence rather than on an
// empty string. It is deliberately a separate field from image_url — that one
// is where the PICTURE lives, this one is where the GUEST goes.
type storyResponse struct {
	ID        string  `json:"id"`
	ImageURL  string  `json:"image_url"`
	Caption   *string `json:"caption,omitempty"`
	ActionURL *string `json:"action_url,omitempty"`
	SortOrder int     `json:"sort_order"`
}

func storyToResponse(s *domain.Story) storyResponse {
	return storyResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   s.Caption,
		ActionURL: s.ActionURL,
		SortOrder: s.SortOrder,
	}
}

// adminStoryResponse is a story as the venue cabinet sees it: the public shape
// plus is_active (the cabinet lists retired cards too) and created_at. Same
// caption-omitted-when-nil convention.
type adminStoryResponse struct {
	ID        string  `json:"id"`
	ImageURL  string  `json:"image_url"`
	Caption   *string `json:"caption,omitempty"`
	ActionURL *string `json:"action_url,omitempty"`
	SortOrder int     `json:"sort_order"`
	IsActive  bool    `json:"is_active"`
	CreatedAt string  `json:"created_at"`
}

func adminStoryToResponse(s *domain.Story) adminStoryResponse {
	return adminStoryResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   s.Caption,
		ActionURL: s.ActionURL,
		SortOrder: s.SortOrder,
		IsActive:  s.IsActive,
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
	}
}

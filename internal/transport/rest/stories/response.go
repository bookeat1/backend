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

// storyToResponse renders a card for a GUEST: the caption is resolved into
// lang (the Russian column when that language has no translation), and the raw
// translation map is not shipped — the client has nothing to resolve. lang ==
// "" (the caller asked for no language) leaves the caption exactly as stored,
// so an old build sees byte-identical output.
func storyToResponse(s *domain.Story, lang string) storyResponse {
	return storyResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   localizedCaption(s, lang),
		ActionURL: s.ActionURL,
		SortOrder: s.SortOrder,
	}
}

// localizedCaption resolves the caption, keeping "no caption at all" (nil)
// distinct from an empty one: a card without a caption has no translations
// either, and inventing an empty string here would make the client draw an
// empty label.
func localizedCaption(s *domain.Story, lang string) *string {
	if s.Caption == nil {
		return nil
	}
	v := s.CaptionI18n.Resolve(lang, *s.Caption)
	return &v
}

// adminStoryResponse is a story as the venue cabinet sees it: the public shape
// plus is_active (the cabinet lists retired cards too) and created_at. Same
// caption-omitted-when-nil convention.
type adminStoryResponse struct {
	ID       string  `json:"id"`
	ImageURL string  `json:"image_url"`
	Caption  *string `json:"caption,omitempty"`
	// CaptionI18n — сырая карта переводов. Кабинет правит подпись и обязан
	// видеть, что именно он правит; гостевой ответ карту не отдаёт вовсе.
	CaptionI18n map[string]string `json:"caption_i18n,omitempty"`
	ActionURL   *string           `json:"action_url,omitempty"`
	SortOrder   int               `json:"sort_order"`
	IsActive    bool              `json:"is_active"`
	CreatedAt   string            `json:"created_at"`
}

func adminStoryToResponse(s *domain.Story) adminStoryResponse {
	return adminStoryResponse{
		ID:          s.ID.String(),
		ImageURL:    s.ImageURL,
		Caption:     s.Caption,
		CaptionI18n: s.CaptionI18n,
		ActionURL:   s.ActionURL,
		SortOrder:   s.SortOrder,
		IsActive:    s.IsActive,
		CreatedAt:   s.CreatedAt.Format(time.RFC3339),
	}
}

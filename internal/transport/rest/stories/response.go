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
//
// expires_at is present only when the venue set a lifetime, and a story that
// reaches a guest is by definition not expired yet — the server already dropped
// the expired ones. It is exposed anyway so a client holding a CACHED rail can
// drop a card that ran out since the fetch, instead of showing week-old oysters
// until the next refresh. That is a courtesy, never the gate: the gate is the
// SQL predicate, so the app, the cabinet and any future client agree.
type storyResponse struct {
	ID        string  `json:"id"`
	ImageURL  string  `json:"image_url"`
	Caption   *string `json:"caption,omitempty"`
	ActionURL *string `json:"action_url,omitempty"`
	SortOrder int     `json:"sort_order"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

func storyToResponse(s *domain.Story) storyResponse {
	return storyResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   s.Caption,
		ActionURL: s.ActionURL,
		SortOrder: s.SortOrder,
		ExpiresAt: formatExpiresAt(s.ExpiresAt),
	}
}

// formatExpiresAt renders the optional lifetime as RFC3339, keeping nil as nil
// so "no expiry" stays an ABSENT field rather than an empty string or a zero
// timestamp a client might read as "expired in 1970".
func formatExpiresAt(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// adminStoryResponse is a story as the venue cabinet sees it: the public shape
// plus is_active (the cabinet lists retired cards too), expires_at and
// is_expired, and created_at. Same caption-omitted-when-nil convention.
//
// is_expired is computed on the SERVER against the same instant the guest read
// uses, and is why the cabinet can list an expired card without lying about it.
// Sending only expires_at and letting the browser compare would put the badge at
// the mercy of the operator's own clock — a laptop an hour off would show a live
// card as expired, or worse, the reverse. The panel still needs expires_at
// itself, to pre-fill the edit form.
type adminStoryResponse struct {
	ID        string  `json:"id"`
	ImageURL  string  `json:"image_url"`
	Caption   *string `json:"caption,omitempty"`
	ActionURL *string `json:"action_url,omitempty"`
	SortOrder int     `json:"sort_order"`
	IsActive  bool    `json:"is_active"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	IsExpired bool    `json:"is_expired"`
	CreatedAt string  `json:"created_at"`
}

func adminStoryToResponse(s *domain.Story, now time.Time) adminStoryResponse {
	return adminStoryResponse{
		ID:        s.ID.String(),
		ImageURL:  s.ImageURL,
		Caption:   s.Caption,
		ActionURL: s.ActionURL,
		SortOrder: s.SortOrder,
		IsActive:  s.IsActive,
		ExpiresAt: formatExpiresAt(s.ExpiresAt),
		IsExpired: s.IsExpired(now),
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
	}
}

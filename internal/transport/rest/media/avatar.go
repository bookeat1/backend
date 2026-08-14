package media

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
)

// AVATAR UPLOAD — the ONE image a guest may put into the bucket.
//
// The admin route next door is gated to staff and the superadmin, with a
// standing note that a plain guest must not reach it. That gate exists because
// the admin upload produces a FREE-FLOATING object: it is stored, its URL is
// returned, and attaching it to anything is a separate, restaurant-scoped
// write. Handing that to every guest would give anyone with an account an
// unattached write into our storage.
//
// This endpoint does not have that shape. It stores the image AND writes the
// URL onto the caller's own profile in the same request, so there is nothing a
// guest can produce here that is not immediately their own avatar — and no id
// in the path to get wrong, because the owner is the token.
//
// Two consequences worth stating:
//   - a guest can only ever overwrite their OWN avatar (userID comes from the
//     token, never from the request);
//   - the previous object is left in the bucket rather than deleted. Deleting
//     it would be a second failure point in the middle of a profile write, and
//     the URLs are immutable and randomly keyed, so an old one leaks nothing
//     beyond the picture the guest had already published to their own profile.
//     Cleanup of orphans is a storage job, not a request-time concern.

// avatarMaxBytes is smaller than the admin cover limit on purpose: an avatar is
// displayed at ~96pt, and 5 MiB already covers a modern phone photo without
// inviting an 8 MB upload for a picture that will be shown in a circle.
const avatarMaxBytes = 5 << 20 // 5 MiB

// AvatarSetter writes the new avatar URL onto a user's profile. Bound in
// bootstrap/deps.go to the users facade.
type AvatarSetter interface {
	SetAvatarURL(ctx context.Context, userID uuid.UUID, url string) error
}

// RegisterUserRoutes mounts the guest-facing avatar upload. Mount on a group
// already running middleware.Auth — the caller's identity IS the authorisation
// here, so there is no role gate: every signed-in user has exactly one avatar
// and it is their own.
func (h *Handler) RegisterUserRoutes(rg *gin.RouterGroup) {
	rg.POST("/users/me/avatar", h.uploadAvatar)
}

func (h *Handler) uploadAvatar(c *gin.Context) {
	if h.store == nil {
		response.ErrorWithCode(c.Writer, http.StatusServiceUnavailable, domain.CodeUnavailable, "media upload is not configured")
		return
	}
	if h.avatars == nil {
		response.ErrorWithCode(c.Writer, http.StatusServiceUnavailable, domain.CodeUnavailable, "avatar upload is not configured")
		return
	}
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}

	data, contentType, ext, ok := readImage(c, avatarMaxBytes, "image exceeds the 5 MB limit")
	if !ok {
		return // readImage has already answered
	}

	key, err := newObjectKey(nowUTC(), ext)
	if err != nil {
		response.HandleError(c.Writer, fmt.Errorf("media: generate key: %w", err))
		return
	}
	if err := h.store.PutOriginal(c.Request.Context(), key, data, contentType); err != nil {
		response.HandleError(c.Writer, fmt.Errorf("media: store avatar: %w", err))
		return
	}

	url := h.store.PublicURL(key)
	// The profile write is what makes this endpoint a guest's own avatar rather
	// than a free upload. If it fails, the request FAILS: answering 200 with a
	// URL that is not on the profile would show the guest a new picture that
	// disappears on the next screen open.
	if err := h.avatars.SetAvatarURL(c.Request.Context(), au.ID, url); err != nil {
		response.HandleError(c.Writer, err)
		return
	}

	response.OK(c.Writer, uploadResponse{URL: url})
}

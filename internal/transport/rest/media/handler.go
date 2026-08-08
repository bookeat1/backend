// Package media exposes the authenticated image-upload endpoint the admin
// panel uses to store a cover/story picture and get back its public URL,
// instead of pasting a third-party URL by hand.
//
// This is the FIRST multipart endpoint in the service. It deliberately holds a
// narrow Store port (PutOriginal + PublicURL), not the whole mediastore, so the
// handler can be tested against a fake without an S3 client, and so a
// mis-wired dependency is a compile error rather than a runtime surprise.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
)

// maxUploadBytes caps an accepted image at 8 MiB. The largest legacy cover in
// the bucket is 7.79 MB (see internal/media doc), so 8 MiB clears every real
// photo while still refusing an attempt to stream something huge into R2.
const maxUploadBytes = 8 << 20 // 8 MiB

// fileField is the multipart form field the client puts the image in.
const fileField = "file"

// extByType maps a SNIFFED (never client-declared) content type to the file
// extension used in the generated key. It is also the allow-list: a type not
// present here is rejected. WebP is included because http.DetectContentType
// recognises the RIFF/WEBP magic and returns "image/webp".
var extByType = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// Store is the slice of the object store this handler needs. The real
// implementation is *mediastore.Store; PutOriginal refuses any key outside the
// uploads/ prefix, and PublicURL turns a key into its public read URL.
type Store interface {
	PutOriginal(ctx context.Context, key string, body []byte, contentType string) error
	PublicURL(key string) string
}

// Handler serves the admin media endpoints. store is nil when R2 is not
// configured on this deployment; every route then answers 503 rather than
// panicking, so the server boots without R2 credentials.
type Handler struct{ store Store }

// NewHandler builds the media HTTP handler. Pass a nil store when R2 is not
// configured — the handler stays mountable and returns 503 on use.
func NewHandler(store Store) *Handler { return &Handler{store: store} }

// RegisterRoutes mounts the authed upload route. Mount on a group already
// running middleware.Auth. The endpoint is NOT restaurant-scoped (it just
// produces an immutable, randomly-keyed object and its URL; attaching that URL
// to a venue's promo/story is a separate, restaurant-scoped, RBAC-gated write),
// so the finest gate available without a restaurant id in the path is the
// global role: any staff user (RoleRestaurant) or the superadmin (RoleAdmin).
// A plain guest (RoleUser) is forbidden.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	grp := rg.Group("")
	// TODO(security): this is a COARSE, global-role gate — any staff user
	// (RoleRestaurant, which includes a hostess) or the superadmin may upload an
	// object that is not yet attached to any restaurant. That is a known
	// first-version tradeoff: PermRestaurantManage cannot be evaluated without a
	// restaurant id in the path, and the upload alone attaches nothing (the
	// restaurant-scoped, RBAC-gated write happens when the URL is saved onto a
	// promo/story). Proper fix = a media-scoped permission (or move the write
	// into a restaurant-scoped usecase). Deferred — do not widen or narrow here
	// without that design.
	grp.Use(middleware.RequireRole(domain.RoleAdmin, domain.RoleRestaurant))
	grp.POST("/admin/media/images", h.upload)
}

type uploadResponse struct {
	URL string `json:"url"`
}

func (h *Handler) upload(c *gin.Context) {
	if h.store == nil {
		response.ErrorWithCode(c.Writer, http.StatusServiceUnavailable, domain.CodeUnavailable, "media upload is not configured")
		return
	}

	// Bound the request body before parsing so a client cannot make us buffer an
	// unbounded multipart body. A little slack over the byte cap covers the
	// multipart envelope (boundaries + headers); the real per-image byte guard
	// is the LimitReader on the file contents below.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes+(1<<20))

	fh, err := c.FormFile(fileField)
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "a file field named \"file\" is required")
		return
	}
	if fh.Size > maxUploadBytes {
		response.Error(c.Writer, http.StatusRequestEntityTooLarge, "image exceeds the 8 MB limit")
		return
	}

	f, err := fh.Open()
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "could not read the uploaded file")
		return
	}
	defer f.Close()

	// Read at most one byte past the cap: if we get that extra byte the file lied
	// about (or omitted) its declared size and is over the limit.
	data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "could not read the uploaded file")
		return
	}
	if len(data) > maxUploadBytes {
		response.Error(c.Writer, http.StatusRequestEntityTooLarge, "image exceeds the 8 MB limit")
		return
	}
	if len(data) == 0 {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "the uploaded file is empty")
		return
	}

	// Sniff the real type from the bytes; never trust the client's Content-Type.
	sniffLen := 512
	if len(data) < sniffLen {
		sniffLen = len(data)
	}
	contentType := http.DetectContentType(data[:sniffLen])
	ext, ok := extByType[contentType]
	if !ok {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "unsupported image type: only JPEG, PNG and WebP are allowed")
		return
	}

	key, err := newObjectKey(time.Now().UTC(), ext)
	if err != nil {
		response.HandleError(c.Writer, fmt.Errorf("media: generate key: %w", err))
		return
	}

	if err := h.store.PutOriginal(c.Request.Context(), key, data, contentType); err != nil {
		response.HandleError(c.Writer, fmt.Errorf("media: store upload: %w", err))
		return
	}

	response.OK(c.Writer, uploadResponse{URL: h.store.PublicURL(key)})
}

// newObjectKey builds an immutable object key:
//
//	uploads/<YYYY>/<MM>/<32-hex>.<ext>
//
// The 16 random bytes make the key collision-free without a database round
// trip, and make the object immutable by construction (a re-upload of the same
// picture gets a fresh key, never new bytes under an old one) — which is what
// the year-long immutable Cache-Control on the object relies on. The YYYY/MM
// segments keep the prefix listing browsable and bounded per month.
func newObjectKey(now time.Time, ext string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("uploads/%04d/%02d/%s%s", now.Year(), int(now.Month()), hex.EncodeToString(b[:]), ext), nil
}

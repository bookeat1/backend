// Package notifications exposes the GUEST-facing in-app «Уведомления» feed.
// Routes must be registered on a group already protected by middleware.Auth —
// every operation acts on the caller's own user id, taken from the token and
// never from the body or the path, so there is no restaurant/RBAC gate here
// (same shape as /users/me, /favorites, /devices/push-tokens).
package notifications

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/middleware"
	"backend-core/internal/transport/rest/response"
	uc "backend-core/internal/usecase/notifications"
)

// Handler serves the guest's in-app notification feed.
type Handler struct{ feed *uc.NotificationFeedUseCase }

// NewHandler builds the notifications feed handler.
func NewHandler(feed *uc.NotificationFeedUseCase) *Handler { return &Handler{feed: feed} }

// RegisterRoutes mounts the endpoints on an authenticated group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/notifications")
	g.GET("", h.list)
	g.POST("/read-all", h.readAll)
	g.POST("/:id/read", h.read)
}

// notificationResponse is one feed entry as the mobile app consumes it. read is
// a boolean the client toggles a dot on; booking_id / restaurant_id are the
// deep-link targets and are null when the origin row was since deleted.
type notificationResponse struct {
	ID           uuid.UUID  `json:"id"`
	Type         string     `json:"type"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	BookingID    *uuid.UUID `json:"booking_id"`
	RestaurantID *uuid.UUID `json:"restaurant_id"`
	Read         bool       `json:"read"`
	CreatedAt    time.Time  `json:"created_at"`
}

// feedResponse is the list payload: a page of entries, the unread badge count
// and the cursor for the next page (omitted on the last page).
type feedResponse struct {
	Items       []notificationResponse `json:"items"`
	UnreadCount int                    `json:"unread_count"`
	NextCursor  string                 `json:"next_cursor,omitempty"`
}

func toResponse(n domain.Notification) notificationResponse {
	return notificationResponse{
		ID:           n.ID,
		Type:         string(n.Type),
		Title:        n.Title,
		Body:         n.Body,
		BookingID:    n.BookingID,
		RestaurantID: n.RestaurantID,
		Read:         n.Read(),
		CreatedAt:    n.CreatedAt,
	}
}

// list returns a page of the caller's feed newest-first plus the unread count.
// @Summary     List my notifications
// @Description Returns the authenticated guest's in-app notification feed,
// @Description newest first, keyset-paginated via the opaque `cursor` query
// @Description parameter, plus the unread badge count.
// @Tags        notifications
// @Produce     json
// @Security    BearerAuth
// @Param       cursor query string false "Opaque cursor from a previous page's next_cursor"
// @Param       limit  query int    false "Page size (default 20, max 100)"
// @Success     200 {object} response.Envelope{data=feedResponse}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     422 {object} response.Envelope "invalid cursor or limit"
// @Router      /api/v1/notifications [get]
func (h *Handler) list(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	cursor, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid cursor")
		return
	}
	limit := 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 {
			response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid limit")
			return
		}
	}
	page, err := h.feed.List(c.Request.Context(), au.ID, cursor, limit)
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	items := make([]notificationResponse, 0, len(page.Items))
	for _, n := range page.Items {
		items = append(items, toResponse(n))
	}
	out := feedResponse{Items: items, UnreadCount: page.UnreadCount}
	if page.Next != nil {
		out.NextCursor = encodeCursor(*page.Next)
	}
	response.OK(c.Writer, out)
}

// read marks one of the caller's notifications read.
// @Summary     Mark a notification read
// @Tags        notifications
// @Produce     json
// @Security    BearerAuth
// @Param       id path string true "Notification id"
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     404 {object} response.Envelope "not found"
// @Router      /api/v1/notifications/{id}/read [post]
func (h *Handler) read(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Error(c.Writer, http.StatusUnprocessableEntity, "invalid notification id")
		return
	}
	if err := h.feed.MarkRead(c.Request.Context(), id, au.ID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "read"})
}

// readAll marks every unread notification of the caller read.
// @Summary     Mark all my notifications read
// @Tags        notifications
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope
// @Failure     401 {object} response.Envelope "unauthorized"
// @Router      /api/v1/notifications/read-all [post]
func (h *Handler) readAll(c *gin.Context) {
	au, ok := middleware.GetAuthUser(c.Request.Context())
	if !ok {
		response.Error(c.Writer, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.feed.MarkAllRead(c.Request.Context(), au.ID); err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	response.OK(c.Writer, gin.H{"status": "read"})
}

// --- cursor codec ------------------------------------------------------------
//
// The cursor is an OPAQUE token to the client: a base64url of
// "<created_at unix nanos>|<uuid>". It is not signed — it leaks nothing (a
// timestamp and a row id the caller already owns) and forging it can only
// re-position the caller within their OWN feed, since every query is still
// scoped by user_id.

var errBadCursor = errors.New("malformed cursor")

func encodeCursor(c domain.NotificationCursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + "|" + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (*domain.NotificationCursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return nil, errBadCursor
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, err
	}
	return &domain.NotificationCursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}

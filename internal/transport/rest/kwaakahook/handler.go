// Package kwaakahook receives Kwaaka's status webhooks.
//
// The handler does the minimum: check the shared secret, cap the body, store
// it in the inbox and answer 200. Parsing and applying happen later in the
// worker (usecase/kwaakaorders), so a slow database or a bad body never turns
// into a Kwaaka retry storm, and the wire format is known to one file only
// (infrastructure/kwaaka/webhook.go).
//
// Authentication is a shared secret in X-Webhook-Secret (KWAAKA_WEBHOOK_SECRET).
// An EMPTY secret skips the check (decision 24.09: Kwaaka has not agreed on a
// secret yet) — the endpoint then trusts anything, which is acceptable only
// because a status can at worst move a pos_state forward or raise a venue
// alert, never touch bookings or money.
package kwaakahook

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
)

const (
	maxBody      = 256 << 10
	secretHeader = "X-Webhook-Secret"
)

// Handler serves POST /webhooks/kwaaka/{order-status,reserve-status}.
type Handler struct {
	inbox  domain.KwaakaWebhookRepository
	secret string
	log    *slog.Logger
}

// NewHandler builds the receiver; an empty secret disables the check.
func NewHandler(inbox domain.KwaakaWebhookRepository, secret string, log *slog.Logger) *Handler {
	return &Handler{inbox: inbox, secret: strings.TrimSpace(secret), log: log}
}

// RegisterRoutes mounts both routes OUTSIDE every auth group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/webhooks/kwaaka/order-status", h.receive(domain.KwaakaWebhookOrderStatus))
	rg.POST("/webhooks/kwaaka/reserve-status", h.receive(domain.KwaakaWebhookReserveStatus))
}

func (h *Handler) receive(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.secret != "" {
			got := c.GetHeader(secretHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
				h.log.Warn("kwaaka webhook rejected: bad secret", slog.String("kind", kind))
				c.JSON(http.StatusUnauthorized, gin.H{"ok": false})
				return
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"ok": false})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"ok": false})
			return
		}
		sum := sha256.Sum256(append([]byte(kind+"\x00"), body...))
		if _, err := h.inbox.Insert(c.Request.Context(), &domain.KwaakaWebhookEvent{
			Kind: kind, DedupKey: hex.EncodeToString(sum[:]), Body: body,
		}); err != nil {
			h.log.Error("kwaaka webhook store failed", slog.String("kind", kind), slog.String("error", err.Error()))
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

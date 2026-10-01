// Package kwaakahook receives Kwaaka's status webhooks.
//
// The handler does the minimum: check the shared secret, cap the body, store
// it in the inbox and answer 200. Parsing and applying happen later in the
// worker (usecase/kwaakaorders), so a slow database or a bad body never turns
// into a Kwaaka retry storm, and the wire format is known to one file only
// (infrastructure/kwaaka/webhook.go).
//
// Authentication is a shared secret in X-Webhook-Secret (KWAAKA_WEBHOOK_SECRET).
// The endpoint exists ONLY when the kitchen-order feature is switched on
// (KWAAKA_ORDERS_ENABLED) AND a secret is configured; otherwise both routes
// answer a bare 404 and read/store nothing (ADR-050, same as telegramhook). An
// empty secret never means "no auth": an open public POST that writes a bytea
// per request into the database is a disk-fill vector, and a forged status can
// flip a pos_state or raise a venue alert.
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
	inbox   domain.KwaakaWebhookRepository
	secret  string
	enabled bool
	log     *slog.Logger
}

// NewHandler builds the receiver. enabled is KWAAKA_ORDERS_ENABLED; with it off
// or with an empty secret every request is answered 404 before anything is read.
func NewHandler(inbox domain.KwaakaWebhookRepository, secret string, enabled bool, log *slog.Logger) *Handler {
	return &Handler{inbox: inbox, secret: strings.TrimSpace(secret), enabled: enabled, log: log}
}

// active reports whether the endpoint exists at all.
func (h *Handler) active() bool {
	return h.enabled && h.secret != "" && h.inbox != nil
}

// RegisterRoutes mounts both routes OUTSIDE every auth group.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/webhooks/kwaaka/order-status", h.receive(domain.KwaakaWebhookOrderStatus))
	rg.POST("/webhooks/kwaaka/reserve-status", h.receive(domain.KwaakaWebhookReserveStatus))
}

func (h *Handler) receive(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.active() {
			// Indistinguishable from a route that does not exist.
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		got := c.GetHeader(secretHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
			h.log.Warn("kwaaka webhook rejected: bad secret", slog.String("kind", kind))
			c.JSON(http.StatusUnauthorized, gin.H{"ok": false})
			return
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

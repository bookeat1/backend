package notifications

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"backend-core/internal/domain"
	"backend-core/internal/transport/rest/response"
)

// TelegramMigrationHandler serves the superadmin's view of the staged migration
// of venue booking alerts from the old notifications bot to
// @book_eat_restaurants_bot (spec telegram-miniapp-restaurant.md §7, step 5).
//
// It exists because the failure mode of this migration is SILENCE: a venue the
// new bot may not write to gets its alert from the old bot and nobody notices
// anything, which is correct behaviour and also exactly why "the migration is
// going fine" must be a number and not a feeling. The old bot may only be
// switched off when `pending` here is zero.
//
// Mount it on the RequireRole(RoleAdmin) group: the payload contains every
// venue's notification chat id.
type TelegramMigrationHandler struct {
	settings domain.RestaurantNotificationSettingsRepository
}

// NewTelegramMigrationHandler builds the report handler.
func NewTelegramMigrationHandler(settings domain.RestaurantNotificationSettingsRepository) *TelegramMigrationHandler {
	return &TelegramMigrationHandler{settings: settings}
}

// RegisterAdminGlobal mounts the report on a superadmin-only group.
func (h *TelegramMigrationHandler) RegisterAdminGlobal(rg *gin.RouterGroup) {
	rg.GET("/admin/notifications/telegram-migration", h.report)
}

// telegramMigrationVenue is one venue's line of the report.
type telegramMigrationVenue struct {
	RestaurantID   uuid.UUID  `json:"restaurant_id"`
	RestaurantName string     `json:"restaurant_name"`
	ChatID         string     `json:"chat_id"`
	Enabled        bool       `json:"telegram_enabled"`
	NewBotReady    bool       `json:"new_bot_ready"`
	NewBotReadyAt  *time.Time `json:"new_bot_ready_at"`
	NewBotFailedAt *time.Time `json:"new_bot_failed_at"`
	// NeedsManualWork marks a target that CANNOT migrate itself: an @username /
	// channel handle has no "Start" to press, so the new bot has to be added as
	// an administrator by hand. These are the venues that will otherwise sit in
	// `pending` forever.
	NeedsManualWork bool `json:"needs_manual_work"`
}

// telegramMigrationReport is the whole answer: three counters to watch and the
// named list of who is still behind.
type telegramMigrationReport struct {
	Total       int                      `json:"total"`
	Ready       int                      `json:"ready"`
	Pending     int                      `json:"pending"`
	ManualWork  int                      `json:"manual_work"`
	PendingList []telegramMigrationVenue `json:"pending_list"`
	ReadyList   []telegramMigrationVenue `json:"ready_list"`
}

// report returns the migration progress.
// @Summary     Telegram bot migration progress
// @Description Superadmin-only. Lists every venue with a connected Telegram
// @Description alert chat and whether the new restaurants bot can already
// @Description write to it. `pending_list` is who is still served by the old
// @Description bot; `manual_work` counts @username targets, which cannot press
// @Description Start and need the bot added as a channel administrator by hand.
// @Tags        admin
// @Produce     json
// @Security    BearerAuth
// @Success     200 {object} response.Envelope{data=telegramMigrationReport}
// @Failure     401 {object} response.Envelope "unauthorized"
// @Failure     403 {object} response.Envelope "forbidden"
// @Router      /api/v1/admin/notifications/telegram-migration [get]
func (h *TelegramMigrationHandler) report(c *gin.Context) {
	rows, err := h.settings.TelegramMigrationStatus(c.Request.Context())
	if err != nil {
		response.HandleError(c.Writer, err)
		return
	}
	out := telegramMigrationReport{
		Total:       len(rows),
		PendingList: make([]telegramMigrationVenue, 0),
		ReadyList:   make([]telegramMigrationVenue, 0),
	}
	for _, r := range rows {
		v := telegramMigrationVenue{
			RestaurantID:    r.RestaurantID,
			RestaurantName:  r.RestaurantName,
			ChatID:          r.ChatID,
			Enabled:         r.Enabled,
			NewBotReady:     r.NewBotReady(),
			NewBotReadyAt:   r.NewBotReadyAt,
			NewBotFailedAt:  r.NewBotFailedAt,
			NeedsManualWork: r.ChatIsUsername(),
		}
		if v.NeedsManualWork {
			out.ManualWork++
		}
		if v.NewBotReady {
			out.Ready++
			out.ReadyList = append(out.ReadyList, v)
			continue
		}
		out.Pending++
		out.PendingList = append(out.PendingList, v)
	}
	response.OK(c.Writer, out)
}

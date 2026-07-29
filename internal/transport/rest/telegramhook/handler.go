// Package telegramhook receives Telegram Bot API updates — today, the presses
// of the Confirm/Reject buttons under a venue's booking alert.
//
// # Why this endpoint is not authenticated like the rest of the API
//
// Everything else in this service authorises a person: a bearer token says who
// you are, and the handler checks what you may touch. A button press carries
// neither. Telegram posts an update to a URL we gave it, and the only proof of
// origin it offers is a secret header we chose ourselves (setWebhook's
// secret_token). So the trust chain here is three links:
//
//  1. the header proves the request came from Telegram and not from the open
//     internet;
//  2. the chat id inside proves WHICH venue pressed, because that venue
//     connected the chat itself in the panel;
//  3. the usecase proves the booking belongs to that venue.
//
// Drop any one and the endpoint becomes a way to confirm strangers' bookings,
// so none of them is optional.
package telegramhook

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
	uc "backend-core/internal/usecase/bookings"
)

// Answerer closes the loop in Telegram: acknowledges the press (so the button
// stops spinning) and rewrites the original message so the alert shows what was
// decided and the buttons are gone. Both are best-effort — see handle.
type Answerer interface {
	AnswerCallback(ctx context.Context, callbackID, text string) error
	EditMessageText(ctx context.Context, chatID string, messageID int64, text string) error
}

// Handler serves POST /telegram/webhook.
type Handler struct {
	status   uc.StatusUseCase
	settings domain.RestaurantNotificationSettingsRepository
	answer   Answerer
	secret   string
}

// NewHandler wires the webhook. An empty secret DISABLES the endpoint: without
// it there is nothing separating Telegram from anyone who guesses the URL, and
// a silently-open confirm endpoint is worse than a missing feature.
func NewHandler(
	status uc.StatusUseCase,
	settings domain.RestaurantNotificationSettingsRepository,
	answer Answerer,
	secret string,
) *Handler {
	return &Handler{status: status, settings: settings, answer: answer, secret: strings.TrimSpace(secret)}
}

// RegisterRoutes mounts the webhook. Mount it OUTSIDE every auth group: Telegram
// cannot carry a bearer token.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/telegram/webhook", h.handle)
}

// update is the sliver of the Telegram update we act on. Everything else in the
// payload is ignored rather than parsed: the less of an untrusted body we
// decode, the less there is to get wrong.
type update struct {
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			Text string `json:"text"`
		} `json:"message"`
	} `json:"callback_query"`
}

func (h *Handler) handle(c *gin.Context) {
	log := logging.FromContext(c.Request.Context())

	if h.secret == "" {
		// Configured off. 404 rather than 503: an endpoint that is not enabled
		// should not advertise that it exists.
		c.Status(http.StatusNotFound)
		return
	}
	got := c.GetHeader("X-Telegram-Bot-Api-Secret-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) != 1 {
		log.Warn("telegram webhook: rejected a request with a bad secret header")
		c.Status(http.StatusUnauthorized)
		return
	}

	var u update
	if err := c.ShouldBindJSON(&u); err != nil {
		// Malformed body: acknowledge anyway. A non-2xx makes Telegram retry the
		// same broken update forever.
		c.Status(http.StatusOK)
		return
	}
	cb := u.CallbackQuery
	if cb == nil || cb.Message == nil {
		// Some other update type (a message to the bot, a member change). Not
		// ours, not an error.
		c.Status(http.StatusOK)
		return
	}

	decision, bookingID, ok := parseCallbackData(cb.Data)
	if !ok {
		h.reply(c, cb.ID, "Не понимаю эту кнопку")
		return
	}

	ctx := c.Request.Context()
	chatID := strconvI64(cb.Message.Chat.ID)
	restaurantID, err := h.settings.RestaurantByTelegramChatID(ctx, chatID)
	if err != nil {
		// The chat is not a venue's notification chat, or the venue switched
		// Telegram off. Same answer either way: this chat may not decide.
		log.Warn("telegram webhook: press from a chat that owns no venue",
			slog.String("chat_id", chatID))
		h.reply(c, cb.ID, "Этот чат не привязан к заведению")
		return
	}

	res, err := h.status.DecideAsVenue(ctx, restaurantID, bookingID, decision)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		h.reply(c, cb.ID, "Бронь не найдена")
		return
	case err != nil:
		log.Error("telegram webhook: decision failed", slog.String("error", err.Error()))
		// 200 with an apology: retrying a failed write from Telegram's queue
		// would replay the same press for hours.
		h.reply(c, cb.ID, "Не получилось, попробуйте в панели")
		return
	}

	who := strings.TrimSpace(cb.From.FirstName)
	if who == "" {
		who = cb.From.Username
	}
	answer, edited := outcomeText(res, decision, who)

	h.reply(c, cb.ID, answer)
	// Rewrite the alert so the chat shows the outcome and the buttons are gone.
	// Best-effort: the decision is already committed, and failing the webhook
	// here would make Telegram replay a press that has already been applied.
	if edited != "" {
		if err := h.answer.EditMessageText(ctx, chatID, cb.Message.MessageID,
			cb.Message.Text+"\n\n"+edited); err != nil {
			log.Warn("telegram webhook: could not rewrite the alert", slog.String("error", err.Error()))
		}
	}
}

// reply acknowledges the press and always answers Telegram with 200. Telegram
// retries any non-2xx, and every failure mode here is one a retry cannot fix.
func (h *Handler) reply(c *gin.Context, callbackID, text string) {
	if err := h.answer.AnswerCallback(c.Request.Context(), callbackID, text); err != nil {
		logging.FromContext(c.Request.Context()).
			Warn("telegram webhook: could not answer the press", slog.String("error", err.Error()))
	}
	c.Status(http.StatusOK)
}

// outcomeText renders what the venue is told, in the two places it is told: the
// toast on the button, and the line appended to the alert.
func outcomeText(res uc.VenueDecisionResult, d uc.VenueDecision, who string) (toast, appended string) {
	switch {
	case res.Conflict:
		return "Уже нельзя: бронь отменена или завершена", "Ответ не применён: бронь уже отменена или завершена."
	case !res.Applied && d == uc.VenueDecisionConfirm:
		return "Уже подтверждена", ""
	case !res.Applied:
		return "Уже отклонена", ""
	case d == uc.VenueDecisionConfirm:
		return "Подтверждено", "Подтверждено" + by(who)
	default:
		return "Отклонено", "Отклонено" + by(who)
	}
}

func by(who string) string {
	if who == "" {
		return "."
	}
	return ": " + who + "."
}

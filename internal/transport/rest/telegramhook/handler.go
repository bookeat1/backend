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

	"github.com/google/uuid"

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

// Messenger sends a plain message into a chat. Only the NEW (restaurants) bot
// needs it: an inbound /start has no callback query to answer, so the only way
// to tell staff "you are connected" — or to hand them their chat id when they
// are not — is an ordinary message back. Same signature as
// notifications.TelegramSender, which *telegramnotify.Sender already satisfies.
type Messenger interface {
	Send(ctx context.Context, chatID, text string) (int, error)
}

// Handler serves one bot's inbound updates.
//
// TWO instances of it run side by side during the migration to
// @book_eat_restaurants_bot (spec §7), each on its own path, with its own
// secret and — critically — its own Answerer: a callback query can only be
// answered with the token of the bot that sent the message it belongs to.
// Sharing one Answerer between two bots would silently break every button under
// yesterday's alerts.
type Handler struct {
	status   uc.StatusUseCase
	settings domain.RestaurantNotificationSettingsRepository
	answer   Answerer
	secret   string
	path     string
	// onboarding is set only for the NEW bot: it is what turns an inbound
	// /start (or "bot added to the group") into telegram_new_bot_ready_at, so a
	// venue migrates itself without re-typing a chat id anywhere. Nil for the
	// old bot, whose updates must never move the migration flag.
	onboarding OnboardingRepository
	// replyBot is the new bot's way to talk back after /start. Optional: without it
	// onboarding still works, the venue just gets no confirmation.
	replyBot Messenger
}

// OnboardingRepository is the slice of the notification settings repository the
// new bot's webhook writes to. Narrow on purpose: an inbound update may move
// the migration flag and nothing else.
type OnboardingRepository interface {
	MarkTelegramNewBotReady(ctx context.Context, restaurantID uuid.UUID) error
	MarkTelegramNewBotFailed(ctx context.Context, restaurantID uuid.UUID) error
}

// NewHandler wires the OLD notifications bot's webhook. An empty secret DISABLES
// the endpoint: without it there is nothing separating Telegram from anyone who
// guesses the URL, and a silently-open confirm endpoint is worse than a missing
// feature.
//
// It stays mounted for the whole migration: the buttons under alerts already
// sitting in venues' chats were sent by the old bot and their presses come back
// here.
func NewHandler(
	status uc.StatusUseCase,
	settings domain.RestaurantNotificationSettingsRepository,
	answer Answerer,
	secret string,
) *Handler {
	return &Handler{
		status: status, settings: settings, answer: answer,
		secret: strings.TrimSpace(secret), path: "/telegram/webhook",
	}
}

// NewStaffHandler wires the NEW restaurants bot's webhook on its own path with
// its own secret. On top of button presses it handles the two updates that
// migrate a venue by themselves: /start in a private chat and my_chat_member in
// a group.
func NewStaffHandler(
	status uc.StatusUseCase,
	settings domain.RestaurantNotificationSettingsRepository,
	answer Answerer,
	secret string,
	onboarding OnboardingRepository,
	replyBot Messenger,
) *Handler {
	return &Handler{
		status: status, settings: settings, answer: answer,
		secret: strings.TrimSpace(secret), path: "/telegram/staff-webhook",
		onboarding: onboarding, replyBot: replyBot,
	}
}

// RegisterRoutes mounts the webhook. Mount it OUTSIDE every auth group: Telegram
// cannot carry a bearer token.
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST(h.path, h.handle)
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

	// Message is read ONLY for the /start of the new bot: the moment staff press
	// Start, this chat becomes reachable by that bot, which is exactly the fact
	// the migration flag records.
	Message *struct {
		Text string `json:"text"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`

	// MyChatMember is the group half of the same fact: the bot was added to (or
	// removed from) a group the venue already uses for alerts.
	MyChatMember *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		NewChatMember struct {
			Status string `json:"status"`
		} `json:"new_chat_member"`
	} `json:"my_chat_member"`
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
		// Not a button press. For the NEW bot two of these updates matter — they
		// are how a venue migrates itself. For the old bot they are ignored, as
		// before: an update to the old bot must never move the migration flag.
		if h.onboarding != nil {
			h.handleOnboarding(c, u)
		}
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

// --- self-service migration onto the new bot (spec §7, step 2) ---------------

// handleOnboarding turns "staff pressed Start" / "the bot was added to the
// group" into telegram_new_bot_ready_at, and "the bot was removed" back into
// not-ready. The chat id is the only credential here, exactly as it is for a
// button press: a chat that no venue connected can prove nothing, and is told
// its own id so staff can paste it into the panel once.
//
// Every branch answers Telegram with 200 (the caller does it): a non-2xx would
// make Telegram replay the same update for hours, and none of the failures here
// is fixable by a retry.
func (h *Handler) handleOnboarding(c *gin.Context, u update) {
	ctx := c.Request.Context()
	log := logging.FromContext(ctx)

	switch {
	case u.Message != nil && isStartCommand(u.Message.Text):
		chatID := strconvI64(u.Message.Chat.ID)
		restaurantID, err := h.settings.RestaurantByTelegramChatID(ctx, chatID)
		if err != nil {
			// Nobody connected this chat (or the venue switched Telegram off).
			// Handing back the chat id is the whole onboarding for a NEW venue:
			// it is the value the panel asks for.
			log.Info("telegram staff webhook: /start from a chat that owns no venue",
				slog.String("chat_id", chatID))
			h.say(ctx, chatID, "Этот чат пока не привязан к заведению.\n"+
				"Скопируйте этот ID и вставьте его в кабинете, раздел «Уведомления»: "+chatID)
			return
		}
		if err := h.onboarding.MarkTelegramNewBotReady(ctx, restaurantID); err != nil {
			log.Error("telegram staff webhook: could not mark the venue ready",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("error", err.Error()))
			h.say(ctx, chatID, "Не получилось включить уведомления, попробуйте ещё раз чуть позже.")
			return
		}
		log.Info("telegram.new_bot_ready",
			slog.String("restaurant_id", restaurantID.String()),
			slog.String("chat_id", chatID))
		h.say(ctx, chatID, "Готово: уведомления о бронях теперь приходят от этого бота.")

	case u.MyChatMember != nil:
		chatID := strconvI64(u.MyChatMember.Chat.ID)
		status := strings.TrimSpace(u.MyChatMember.NewChatMember.Status)
		restaurantID, err := h.settings.RestaurantByTelegramChatID(ctx, chatID)
		if err != nil {
			log.Info("telegram staff webhook: membership change in a chat that owns no venue",
				slog.String("chat_id", chatID), slog.String("status", status))
			return
		}
		switch status {
		case "member", "administrator", "creator":
			if err := h.onboarding.MarkTelegramNewBotReady(ctx, restaurantID); err != nil {
				log.Error("telegram staff webhook: could not mark the venue ready",
					slog.String("restaurant_id", restaurantID.String()),
					slog.String("error", err.Error()))
				return
			}
			log.Info("telegram.new_bot_ready",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("chat_id", chatID))
			h.say(ctx, chatID, "Готово: уведомления о бронях теперь приходят от этого бота.")
		case "left", "kicked":
			// Demote immediately rather than waiting for the next booking to
			// discover it with a 403: the venue keeps its alerts, from the old
			// bot, without a single event spent finding out.
			if err := h.onboarding.MarkTelegramNewBotFailed(ctx, restaurantID); err != nil {
				log.Error("telegram staff webhook: could not demote the venue",
					slog.String("restaurant_id", restaurantID.String()),
					slog.String("error", err.Error()))
				return
			}
			log.Warn("telegram.new_bot_rejected",
				slog.String("restaurant_id", restaurantID.String()),
				slog.String("chat_id", chatID),
				slog.String("reason", "removed_from_chat"))
		}
	}
}

// isStartCommand recognises /start, including the "/start@botname" and
// "/start <payload>" forms Telegram produces in groups and from deep links.
func isStartCommand(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/start") {
		return false
	}
	rest := t[len("/start"):]
	return rest == "" || strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "@")
}

// say is a best-effort reply into the chat. Failure is logged and swallowed:
// the migration flag is already written, and failing the webhook over a missed
// confirmation would make Telegram replay the update.
func (h *Handler) say(ctx context.Context, chatID, text string) {
	if h.replyBot == nil {
		return
	}
	if _, err := h.replyBot.Send(ctx, chatID, text); err != nil {
		logging.FromContext(ctx).Warn("telegram staff webhook: could not reply",
			slog.String("error", err.Error()))
	}
}

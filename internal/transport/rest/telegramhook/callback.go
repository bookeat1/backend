package telegramhook

import (
	"strconv"
	"strings"

	"github.com/google/uuid"

	uc "backend-core/internal/usecase/bookings"
)

// Callback data format: "bk:<decision>:<booking uuid>".
//
// Telegram caps callback_data at 64 bytes, and a UUID plus this prefix is 43,
// so the whole decision fits with room to spare. It carries no signature on
// purpose: the data is not the credential — the chat it arrives from is (see
// the package comment). A forged booking id from a venue's own chat still has
// to belong to that venue, which the usecase checks.
const (
	prefix        = "bk"
	actionConfirm = "confirm"
	actionReject  = "reject"
)

// CallbackConfirm and CallbackReject build the data for the two buttons. They
// live here, next to the parser, so the two can never drift apart.
func CallbackConfirm(bookingID uuid.UUID) string {
	return prefix + ":" + actionConfirm + ":" + bookingID.String()
}

func CallbackReject(bookingID uuid.UUID) string {
	return prefix + ":" + actionReject + ":" + bookingID.String()
}

// parseCallbackData reads the data back. It is deliberately strict: anything
// that is not exactly our own format is refused rather than guessed at, because
// the only ways to get here are our own buttons and someone probing the bot.
func parseCallbackData(data string) (uc.VenueDecision, uuid.UUID, bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != prefix {
		return "", uuid.Nil, false
	}
	var decision uc.VenueDecision
	switch parts[1] {
	case actionConfirm:
		decision = uc.VenueDecisionConfirm
	case actionReject:
		decision = uc.VenueDecisionReject
	default:
		return "", uuid.Nil, false
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return "", uuid.Nil, false
	}
	return decision, id, true
}

// strconvI64 renders a Telegram chat id the way it is stored: as text, because
// a chat id can be a negative group id and the column is a varchar.
func strconvI64(v int64) string { return strconv.FormatInt(v, 10) }

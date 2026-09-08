package domain

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// OTP delivery channel names. They are stored verbatim in otp_codes.channel
// (VARCHAR, validated here — no DB enums), and they are the only strings the rest of the system may use to name a
// channel: the infrastructure adapters answer with these, the usecase remembers
// these. Renaming one is a data migration, not a refactor.
const (
	// OTPChannelTelegram is the paid Telegram Gateway (a code delivered to a
	// PHONE NUMBER inside the Telegram app), not the notifications bot.
	OTPChannelTelegram = "telegram_gateway"
	// OTPChannelWhatsApp is the Meta WhatsApp Cloud API authentication template.
	OTPChannelWhatsApp = "whatsapp"
	// OTPChannelSMS is the last-resort SMS, whichever provider is configured —
	// the guest does not care which one, and neither does the waterfall memory.
	OTPChannelSMS = "sms"
	// OTPChannelStub is the placeholder "delivery" used while no real channel is
	// configured. It is deliberately NOT a channel worth remembering.
	OTPChannelStub = "stub"
	// OTPChannelUndelivered marks a code no channel accepted. Such a row exists
	// only so the rate-limit counter sees the attempt (see usecase/auth
	// RequestOTP); it is born already used and can never complete a login.
	OTPChannelUndelivered = "undelivered"
)

// OTPRememberableChannel reports whether a channel is worth remembering as a
// guest's preferred route. The stub never delivered anything, and "undelivered"
// is the opposite of a working channel — remembering either would put a dead
// route at the head of the next waterfall.
func OTPRememberableChannel(channel string) bool {
	switch channel {
	case OTPChannelTelegram, OTPChannelWhatsApp, OTPChannelSMS:
		return true
	default:
		return false
	}
}

// OTPRememberableChannels lists the same channels as a slice, for repositories
// that filter in SQL. It returns a fresh slice on every call so a caller can
// never mutate the answer for everyone else.
func OTPRememberableChannels() []string {
	return []string{OTPChannelTelegram, OTPChannelWhatsApp, OTPChannelSMS}
}

// OTPCode is a single issued phone one-time code. CodeHash is sha256(code).
type OTPCode struct {
	ID        uuid.UUID
	Phone     string
	CodeHash  string
	Channel   string
	Attempts  int
	UsedAt    *time.Time
	ExpiresAt time.Time
	CreatedAt time.Time
}

// OTPRepository persists OTP codes.
type OTPRepository interface {
	Create(ctx context.Context, c *OTPCode) error
	// LatestActiveByPhone returns the newest unused, unexpired code for phone,
	// or ErrNotFound.
	LatestActiveByPhone(ctx context.Context, phone string) (*OTPCode, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
	IncrementAttempts(ctx context.Context, id uuid.UUID) error
	// CountSince counts codes created for phone at or after ts (for rate limits).
	CountSince(ctx context.Context, phone string, ts time.Time) (int, error)
	// InvalidateActiveByPhone marks every still-active (unused, unexpired) code
	// for phone as used, so none of them can complete a login. Used on account
	// deletion, before the phone number is anonymized off the user row and
	// potentially reused by a new signup. A no-op success when none are active.
	InvalidateActiveByPhone(ctx context.Context, phone string) error
	// LastUsedChannelByPhone returns the channel of the newest USED code for
	// phone, or "" when there is none. This is the delivery memory: a code
	// marked used is a code the guest actually typed back, which is the only
	// proof of delivery that exists (no provider API can give it — Meta and
	// Twilio both "accept" messages they then drop).
	//
	// It reads the EXISTING otp_codes rows rather than a preferences table on
	// purpose: no schema change, and the fact is already there. Two known
	// imprecisions, both acceptable for an advisory hint:
	//   - a code invalidated by account deletion is also "used" here;
	//   - otp_codes has no retention job yet, so the memory never ages out.
	// Both cost at most one wasted attempt on a stale channel, never a login.
	LastUsedChannelByPhone(ctx context.Context, phone string) (string, error)
}

// OTPSendHint orders the delivery waterfall for ONE send. It lives in domain
// because both sides need the word: the usecase builds it from what it knows
// about the phone, the infrastructure sender obeys it. Every field is advisory —
// a sender that cannot honour a hint must still deliver, never fail.
type OTPSendHint struct {
	// Prefer is the channel to try first (the one that last completed a login
	// for this phone). Empty, unknown or disabled → ignored.
	Prefer string
	// Deprioritize lists channels to move to the BACK of the order. Populated
	// when a channel already accepted a code for this phone that nobody has
	// verified: the accept may have been a silent drop, so a retry that picks
	// the same channel again would strand the guest in the same silence.
	Deprioritize []string
}

package otpsender

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"backend-core/internal/domain"
	"backend-core/internal/logging"
)

// Channel is one OTP delivery route (Telegram Gateway, WhatsApp, SMS). It is
// deliberately tiny: the waterfall needs to know a channel's name and whether
// it took the code. Everything else — pre-checks, templates, retries inside one
// provider — is the channel's own business.
type Channel interface {
	// Name is one of the domain.OTPChannel* constants. It is what gets stored
	// on the otp_codes row and remembered as the guest's preference.
	Name() string
	// Send delivers code to phone (E.164). A nil error means the provider took
	// the code; any error means "I did not deliver" and the waterfall moves on.
	//
	// The first return value is the PROVIDER's own message id (Telegram's
	// request_id, Meta's wamid, Twilio's SID). It is logged so a delivery
	// complaint can be traced in the provider's dashboard without asking the
	// guest to repeat anything; it may be empty when a provider returns none.
	//
	// A Send MUST NOT put the code into the error it returns — the waterfall
	// scrubs defensively, but the guarantee belongs to the channel.
	Send(ctx context.Context, phone, code string) (string, error)
}

var (
	// ErrChannelUnavailable is a channel's definite "this number cannot receive
	// anything from me" — Telegram Gateway's pre-check saying the number is not
	// on Telegram, WhatsApp's 131026. It is separated from a transient failure
	// only for the operator's benefit (a log line that says "wrong channel"
	// rather than "provider broken"); both fall through identically.
	ErrChannelUnavailable = errors.New("otp channel cannot reach this number")

	// ErrDeliveryFailed is what the waterfall returns when no channel took the
	// code. It is deliberately one single error for every cause: number not on
	// any channel, every provider down, credentials expired. The caller cannot
	// tell those apart, and neither can an attacker probing which numbers are
	// reachable where (see the note on the enumeration oracle below).
	ErrDeliveryFailed = errors.New("otp delivery failed on every configured channel")
)

// defaultChannelTimeout caps ONE channel's attempt. Without it a provider that
// accepts the TCP connection and then goes quiet would hold the login request
// until the HTTP server's own timeout, and the remaining channels — the ones
// that would have worked — would never be tried.
//
// Note that Telegram Gateway spends this budget on TWO calls (pre-check, then
// send), which is why it is not shaved down further.
const defaultChannelTimeout = 5 * time.Second

// defaultDeliveryBudget caps the WHOLE waterfall. Per-channel timeouts alone do
// not bound the request: three channels at 5 s each would be 15 s, which is
// exactly the HTTP server's WriteTimeout (bootstrap.app) — the guest's
// connection would die while we were still spending money on their behalf. The
// budget is deliberately under that, so the login endpoint always gets to answer.
const defaultDeliveryBudget = 12 * time.Second

// WaterfallConfig holds the two deadlines that keep OTP delivery inside one
// synchronous login request.
type WaterfallConfig struct {
	// ChannelTimeout caps one channel's attempt.
	ChannelTimeout time.Duration
	// TotalBudget caps the whole waterfall, including every fallback hop.
	TotalBudget time.Duration
}

// Waterfall delivers a code through the first channel that takes it.
//
// # Ordering
//
// The configured order is the fallback order (cheapest/most likely first). A
// per-phone hint reorders it: the channel that last completed a login for this
// number goes first, and any channel that already accepted an unverified code
// for this number goes last. The hint can only REORDER — it can never remove a
// channel, so a stale memory pointing at a dead channel costs one failed
// attempt, never a failed login.
//
// # Why a total failure says so little
//
// "delivery_failed" carries no channel list. Telling a caller "telegram: not on
// telegram, whatsapp: not on whatsapp, sms: sent" would turn the login endpoint
// into a free lookup service: anyone could enumerate which numbers have
// Telegram or WhatsApp accounts, which is exactly the data a targeted phishing
// campaign wants and which the guest never agreed to publish. The per-channel
// outcomes exist only in our own logs, with the phone masked.
//
// # Known and accepted: the walk is observable in wall-clock time
//
// The response BODY cannot leak which channel was used — every outcome renders
// the same envelope, and a total failure renders one generic error (above). The
// DURATION can. A number that is on Telegram costs two Gateway round-trips
// (checkSendAbility, then sendVerificationMessage) and answers; a number that is
// not fails the pre-check on the first round-trip and then pays for a WhatsApp
// or SMS attempt. Those paths take measurably different amounts of time, so an
// observer who can time /auth/otp/request can infer, with some noise, whether a
// number has Telegram — the very fact the opaque error above refuses to state.
//
// This is DOCUMENTED, NOT PAPERED OVER. Padding every response to a fixed
// duration would mean holding a connection open on purpose and making every
// honest login as slow as the worst path; whether that trade is worth making is
// the owner's call, not this file's. Today the mitigation is the rate limit on
// the endpoint (AUTH_OTP_RATE_PER_MIN / _PER_HOUR count otp_codes rows, and a
// failed delivery consumes the budget too — see usecase/auth.RequestOTP), which
// bounds how many numbers an attacker can time per phone number and per hour.
// Enumerating a list of numbers this way stays possible, just slow and noisy.
type Waterfall struct {
	channels []Channel
	log      *slog.Logger
	timeout  time.Duration
	budget   time.Duration
}

// NewWaterfall builds the sender over the channels in fallback order. Callers
// pass only ENABLED channels — a channel whose credentials are absent is never
// constructed (see bootstrap), so there is no "disabled" state to check here.
// With no channels at all NewWaterfall returns nil: the caller must fall back to
// the Stub, which is the documented pre-token behaviour.
func NewWaterfall(log *slog.Logger, cfg WaterfallConfig, channels ...Channel) *Waterfall {
	if len(channels) == 0 {
		return nil
	}
	timeout := cfg.ChannelTimeout
	if timeout <= 0 {
		timeout = defaultChannelTimeout
	}
	budget := cfg.TotalBudget
	if budget <= 0 {
		budget = defaultDeliveryBudget
	}
	if budget < timeout {
		// A budget smaller than one attempt would mean no channel ever gets a
		// full try; the per-channel cap is the floor.
		budget = timeout
	}
	return &Waterfall{channels: channels, log: log, timeout: timeout, budget: budget}
}

// Channels reports the configured channel names in fallback order (diagnostics
// and startup logging — never a response body).
func (w *Waterfall) Channels() []string {
	names := make([]string, 0, len(w.channels))
	for _, c := range w.channels {
		names = append(names, c.Name())
	}
	return names
}

// Send walks the ordered channels and returns the name of the first one that
// took the code. When none does it returns ErrDeliveryFailed.
func (w *Waterfall) Send(ctx context.Context, phone, code string, hint domain.OTPSendHint) (string, error) {
	log := w.logger(ctx)
	masked := logging.MaskPhone(phone)

	// One budget for the whole walk: the guest is holding an open HTTP request,
	// and the fallbacks must fit inside it rather than outlive it.
	ctx, cancelBudget := context.WithTimeout(ctx, w.budget)
	defer cancelBudget()

	for _, ch := range w.order(hint) {
		// The caller's context is still the boss: if the guest hung up or the
		// handler's deadline passed, stop spending money on their behalf.
		if err := ctx.Err(); err != nil {
			log.Warn("otp.delivery_aborted",
				slog.String("phone_masked", masked),
				slog.String("reason", err.Error()),
			)
			return "", ErrDeliveryFailed
		}

		attemptCtx, cancel := context.WithTimeout(ctx, w.timeout)
		messageID, err := ch.Send(attemptCtx, phone, code)
		cancel()

		if err == nil {
			// The provider's own id is the only handle we and the provider
			// share: it is what a support ticket ("the guest never got it") is
			// looked up by. The code is never here, by construction.
			log.Info("otp.sent",
				slog.String("phone_masked", masked),
				slog.String("channel", ch.Name()),
				slog.String("provider_message_id", messageID),
			)
			return ch.Name(), nil
		}

		// One channel failing is the normal path, not an incident: that is what
		// the waterfall is for. It is logged at WARN (not ERROR) with the reason
		// scrubbed of the code, so an operator can see WHY a guest ended up on
		// the expensive channel without the log becoming a code leak.
		level := slog.LevelWarn
		if errors.Is(err, ErrChannelUnavailable) {
			// A definite "not reachable here" is expected traffic, not a fault.
			level = slog.LevelInfo
		}
		log.Log(ctx, level, "otp.channel_failed",
			slog.String("phone_masked", masked),
			slog.String("channel", ch.Name()),
			slog.String("error", sanitize(err, code, phone).Error()),
		)
	}

	// Every channel refused. ERROR, because either the guest is unreachable
	// everywhere (rare, worth seeing) or our providers are down (very worth
	// seeing) — and from here the two are indistinguishable on purpose.
	log.Error("otp.delivery_failed",
		slog.String("phone_masked", masked),
		slog.Int("channels_tried", len(w.channels)),
	)
	return "", ErrDeliveryFailed
}

// order applies the hint to the configured fallback order. It returns a new
// slice and never drops a channel: preference is an optimization, and an
// optimization that can make a login impossible is a bug.
func (w *Waterfall) order(hint domain.OTPSendHint) []Channel {
	if hint.Prefer == "" && len(hint.Deprioritize) == 0 {
		return w.channels
	}

	demoted := make(map[string]bool, len(hint.Deprioritize))
	for _, name := range hint.Deprioritize {
		demoted[name] = true
	}

	ordered := make([]Channel, 0, len(w.channels))
	taken := make(map[string]bool, len(w.channels))
	// A remembered channel that ALSO just accepted an unverified code is not
	// promoted: the fresher fact (a code sitting unused right now) beats the
	// older one (a login that worked days ago). Otherwise the guest whose
	// WhatsApp broke would be pinned to WhatsApp by their own history.
	if hint.Prefer != "" && !demoted[hint.Prefer] {
		for _, c := range w.channels {
			if c.Name() == hint.Prefer {
				ordered = append(ordered, c)
				taken[c.Name()] = true
				break
			}
		}
	}
	for _, c := range w.channels {
		if taken[c.Name()] || demoted[c.Name()] {
			continue
		}
		ordered = append(ordered, c)
		taken[c.Name()] = true
	}
	// Demoted channels are appended, not removed: WhatsApp having swallowed the
	// previous code is a reason to try it last, not a reason to leave a guest
	// with no route at all when it is the only channel configured.
	for _, c := range w.channels {
		if demoted[c.Name()] {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

func (w *Waterfall) logger(ctx context.Context) *slog.Logger {
	// Same rule as the Stub: prefer the request-scoped logger (it carries the
	// request id), fall back to the one the sender was built with.
	log := w.log
	if log == nil {
		log = slog.Default()
	}
	if ctxLog := logging.FromContext(ctx); ctxLog != slog.Default() {
		log = ctxLog
	}
	return log
}

// sanitize is the last line of defence before an error reaches a log line. The
// channels promise never to embed the code or the raw phone in an error, but an
// error can be wrapped by anything (a *url.Error carrying a URL we built, a
// provider echoing its input back at us); one accidental echo would publish a
// live credential, or an unmasked phone number, into the log store.
//
// The code becomes "***" and the phone becomes its masked form, so the line
// stays useful without becoming a leak.
func sanitize(err error, code, phone string) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	out := msg
	if code != "" {
		out = strings.ReplaceAll(out, code, "***")
	}
	if phone != "" {
		out = strings.ReplaceAll(out, phone, logging.MaskPhone(phone))
	}
	if out == msg {
		return err
	}
	return errors.New(out)
}

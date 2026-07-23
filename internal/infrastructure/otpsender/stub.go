// Package otpsender delivers OTP codes. The stub stands in for the real
// provider waterfall (Telegram / Gateway / WhatsApp / SMS), which is a later
// phase.
//
// # Why the code is not logged outside development
//
// An OTP is a bearer credential: whoever reads it owns the account for the next
// few minutes. Logs are the least private place in a system — they are shipped
// to a log store, read by support, kept for weeks and, in this deployment,
// forwarded to a hosted service. Printing the code is a development
// convenience and an account takeover anywhere else.
//
// So the code reaches the log only when the application explicitly declares
// itself a development environment. Everywhere else the stub still answers (it
// is a stub, it must not break the flow) but logs only that a code was issued,
// with the phone masked. A stub answering outside development is a
// misconfiguration, so that line is WARN, loudly, on purpose.
package otpsender

import (
	"context"
	"log/slog"
	"strings"

	"backend-core/internal/logging"
)

// devEnvironment is the only APP_ENV value that lets the code into the log.
const devEnvironment = "development"

// Stub reports the code instead of delivering it. Construct it with the
// application environment so it can decide whether printing the code is safe.
type Stub struct {
	log *slog.Logger
	dev bool
}

// NewStub builds the stub for the given environment. Anything other than
// "development" (case-insensitive) is treated as a real environment, where the
// code is never written to the log.
func NewStub(log *slog.Logger, environment string) *Stub {
	return &Stub{
		log: log,
		dev: strings.EqualFold(strings.TrimSpace(environment), devEnvironment),
	}
}

// Send reports the "stub" channel and never errors.
func (s *Stub) Send(ctx context.Context, phone, code string) (string, error) {
	// Prefer the request-scoped logger so the line carries the request id.
	// FromContext falls back to slog.Default() when a context never went
	// through the middleware; in that case use the logger this stub was built
	// with, which is what tests and one-off commands pass in.
	log := s.log
	if ctxLog := logging.FromContext(ctx); ctxLog != slog.Default() {
		log = ctxLog
	}

	if s.dev {
		log.Info("otp.stub_send",
			slog.String("phone", phone),
			slog.String("code", code),
			slog.String("channel", "stub"),
		)
		return "stub", nil
	}

	// Not development: the code stays out of the log, and a stub sender
	// answering at all is a configuration problem worth shouting about.
	log.Warn("otp.stub_send_outside_development",
		slog.String("phone_masked", logging.MaskPhone(phone)),
		slog.String("channel", "stub"),
		slog.String("detail", "OTP code withheld from logs; configure a real OTP provider"),
	)
	return "stub", nil
}

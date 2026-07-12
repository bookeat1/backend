// Package otpsender delivers OTP codes. The stub logs the code instead of
// sending it — the real provider waterfall (Telegram / Gateway / WhatsApp / SMS)
// is a later phase.
package otpsender

import (
	"context"
	"log/slog"
)

type Stub struct{ log *slog.Logger }

func NewStub(log *slog.Logger) *Stub { return &Stub{log: log} }

// Send logs the code and reports the "stub" channel. Never errors.
func (s *Stub) Send(ctx context.Context, phone, code string) (string, error) {
	s.log.Info("otp stub send", slog.String("phone", phone), slog.String("code", code))
	return "stub", nil
}

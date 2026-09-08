package auth

import (
	"context"
	"crypto/subtle"
	"log/slog"

	"github.com/google/uuid"

	"backend-core/internal/auth/phone"
	"backend-core/internal/logging"
)

// testAccount is the ONE phone number that logs in with a fixed code and no
// message ever leaving the server.
//
// # Why it exists
//
// The app authenticates with a code delivered over Telegram / WhatsApp / SMS.
// An App Store reviewer cannot receive any of those, so without a documented
// demo account the review simply cannot get past the login screen — one of the
// most common rejection reasons (App Review Guideline 2.1: a full-access demo
// account must be supplied in App Review Information). The alternative the
// owner floated — a hidden long-press bypass inside the app — is worse on two
// counts: Guideline 2.3.1 forbids hidden or undocumented features outright, and
// a bypass that ships to every user is a permanent hole in the product. The
// server-side allowlist below is the standard answer: one number, one code,
// switched on by configuration, invisible to the app, deletable by unsetting an
// env var.
//
// # The rules it obeys
//
//   - It is OFF unless BOTH the phone and the code are configured. Zero values
//     mean the whole file is dead weight and every number, including this one,
//     takes the ordinary path (bootstrap.NewConfig refuses a half-configured
//     pair outright, so "off" can never be an accident of one missing var).
//   - Requesting a code for it never calls the sender. The number does not
//     exist on any network; a delivery attempt would fail, the waterfall would
//     burn its whole budget, and the reviewer would see an error instead of the
//     code input.
//   - Verifying accepts exactly the configured code and nothing else.
//   - It NEVER participates in the phone-change flow (see otp.go): letting a
//     signed-in stranger move onto this number would hand them the reviewer's
//     account, and letting the reviewer move off it would break the next review.
//   - Every request and every verification of it is logged at WARN. This number
//     is expected to see a handful of logins per app submission; anything more
//     is somebody probing, and a WARN line is what makes that visible without
//     anybody watching.
type testAccount struct {
	// phone is stored normalized, so the env var may be written in any human
	// format ("+7 777 000 00 00") and still match what the app sends.
	phone string
	code  string
}

// newTestAccount reads the pair off the usecase config. An empty phone or an
// empty code disables the whole feature — that is the safe default and the
// state every environment except the review contour is expected to be in.
func newTestAccount(cfg Config) testAccount {
	p := phone.Normalize(cfg.TestAccountPhone)
	if p == "" || cfg.TestAccountCode == "" {
		return testAccount{}
	}
	return testAccount{phone: p, code: cfg.TestAccountCode}
}

// enabled reports whether a test account is configured at all.
func (t testAccount) enabled() bool { return t.phone != "" && t.code != "" }

// matches reports whether an ALREADY NORMALIZED phone is the test account.
func (t testAccount) matches(normalizedPhone string) bool {
	return t.enabled() && normalizedPhone == t.phone
}

// codeAccepted compares a submitted code with the configured one in constant
// time. The comparison cannot leak a prefix through timing — cheap here, and
// the one thing that would otherwise make guessing this fixed code easier than
// guessing a random one.
func (t testAccount) codeAccepted(code string) bool {
	if !t.enabled() {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(code), []byte(t.code)) == 1
}

// logRequest records that somebody asked for a code for the test number. No
// message was sent and no otp_codes row was written, so this log line is the
// ONLY trace such a request leaves anywhere.
func (t testAccount) logRequest(ctx context.Context) {
	logging.FromContext(ctx).Warn(logging.EventTestAccountOTPRequested,
		slog.String("phone_masked", logging.MaskPhone(t.phone)),
	)
}

// logVerify records one login attempt on the test number, accepted or not.
// WARN on purpose, including the successful case: a successful login on this
// number is itself an event worth seeing, and a stream of accepted=false lines
// is what a brute-force attempt looks like.
func (t testAccount) logVerify(ctx context.Context, accepted bool) {
	logging.FromContext(ctx).Warn(logging.EventTestAccountLoginAttempt,
		slog.String("phone_masked", logging.MaskPhone(t.phone)),
		slog.Bool("accepted", accepted),
	)
}

// logPhoneChangeRefused records a signed-in account trying to move ONTO the
// test number. Nothing legitimate does this, so the line carries the caller's
// user id: it is the one field that turns "somebody tried" into "this account
// tried".
func (t testAccount) logPhoneChangeRefused(ctx context.Context, userID uuid.UUID) {
	logging.FromContext(ctx).Warn(logging.EventTestAccountPhoneChangeRefused,
		slog.String("phone_masked", logging.MaskPhone(t.phone)),
		slog.String("user_id", userID.String()),
	)
}

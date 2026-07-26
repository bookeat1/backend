package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
)

const maxOTPAttempts = 5

// OTPUseCase is the phone-OTP authentication usecase: request a one-time code
// and verify it (find-or-create the user, then issue a token pair). It is a
// distinct usecase from the core credential Facade (facade.go).
type OTPUseCase interface {
	RequestOTP(ctx context.Context, rawPhone string) (string, error)
	VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error)
}

type otpUseCase struct {
	users   domain.UserRepository
	otp     domain.OTPRepository
	refresh domain.RefreshTokenRepository
	tx      domain.TxManager
	tokens  TokenIssuer
	sender  OTPSender
	cfg     Config
}

// NewOTPUseCase constructs the phone-OTP authentication usecase.
func NewOTPUseCase(
	users domain.UserRepository,
	otp domain.OTPRepository,
	refresh domain.RefreshTokenRepository,
	tx domain.TxManager,
	tokens TokenIssuer,
	sender OTPSender,
	cfg Config,
) OTPUseCase {
	return &otpUseCase{
		users:   users,
		otp:     otp,
		refresh: refresh,
		tx:      tx,
		tokens:  tokens,
		sender:  sender,
		cfg:     cfg,
	}
}

// RequestOTP normalizes the phone, enforces rate limits, stores a hashed code,
// and asks the sender to deliver it. Returns the code only when OTPDevExpose.
//
// # Why delivery is synchronous and not handed to the outbox worker
//
// Booking notifications go through the transactional outbox that the
// notifications Dispatcher drains once per NOTIFY_DISPATCH_TICK_INTERVAL (15 s
// by default). That is right for a push nobody is waiting for, and wrong here:
//
//   - A guest is holding an open login screen. Up to a tick of latency before
//     the first channel is even attempted, plus another tick per fallback hop,
//     would make a three-channel waterfall feel broken.
//   - The answer to "we could not deliver" has to reach the caller. The outbox
//     is fire-and-forget by design; the endpoint would have to answer 200 for a
//     code that will never arrive.
//   - The waterfall IS the retry policy: three channels tried in order inside
//     one bounded budget (otpsender.WaterfallConfig). The worker's redelivery
//     would be a second, uncoordinated retry — and every retry costs money.
//
// The cost of that choice is bounded by the delivery budget, which is set below
// the HTTP server's WriteTimeout; see otpsender.defaultDeliveryBudget.
func (o *otpUseCase) RequestOTP(ctx context.Context, rawPhone string) (string, error) {
	p := phone.Normalize(rawPhone)
	if p == "" {
		return "", fmt.Errorf("%w: phone required", domain.ErrValidation)
	}

	perMin, err := o.otp.CountSince(ctx, p, time.Now().Add(-time.Minute))
	if err != nil {
		return "", err
	}
	if perMin >= o.cfg.OTPPerMin {
		return "", fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation)
	}
	perHour, err := o.otp.CountSince(ctx, p, time.Now().Add(-time.Hour))
	if err != nil {
		return "", err
	}
	if perHour >= o.cfg.OTPPerHour {
		return "", fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation)
	}

	code, err := otpcode.Generate()
	if err != nil {
		return "", err
	}
	channel, err := o.sender.Send(ctx, p, code, o.sendHint(ctx, p))
	if err != nil {
		// Delivery failed on every channel. Record the attempt anyway, as an
		// already-used row: the rate-limit counter above counts otp_codes rows,
		// so without this an attacker could hammer this endpoint forever — every
		// call free of a limit and every call spending real money on WhatsApp /
		// SMS sends. The row is born used (and with no reachable channel) so it
		// can never complete a login. Best effort: a failure to record must not
		// replace the delivery error the caller needs to see.
		o.recordUndeliveredAttempt(ctx, p, code)
		return "", fmt.Errorf("send otp: %w", err)
	}
	now := time.Now()
	rec := &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   channel,
		ExpiresAt: now.Add(o.cfg.OTPTTL),
		CreatedAt: now,
	}
	if err := o.otp.Create(ctx, rec); err != nil {
		return "", err
	}
	if o.cfg.OTPDevExpose {
		return code, nil
	}
	return "", nil
}

// sendHint builds the per-phone ordering advice for one delivery. Everything it
// does is best effort: a hint is an optimization, and a repository hiccup must
// never be the reason a guest cannot log in, so every error here is swallowed
// and the waterfall falls back to its configured order.
func (o *otpUseCase) sendHint(ctx context.Context, p string) domain.OTPSendHint {
	var hint domain.OTPSendHint

	// 1. The channel that last carried a code this number actually TYPED BACK
	// goes first. The fact comes from the existing otp_codes rows (a used code
	// is a delivered code), so the memory needs no table of its own — see
	// domain.OTPRepository.LastUsedChannelByPhone for its two imprecisions.
	if last, err := o.otp.LastUsedChannelByPhone(ctx, p); err == nil &&
		domain.OTPRememberableChannel(last) {
		hint.Prefer = last
	}

	// 2. A channel that already holds an UNVERIFIED code for this number goes
	// last. This is the answer to WhatsApp's (and SMS's) optimistic accept: the
	// provider said yes, the guest never typed the code, so the message may well
	// have been swallowed. Repeating the same channel would repeat the same
	// silence; moving it to the back is what turns a stuck login into a working
	// one on the second try, with no background timer and no extra API surface.
	if prev, err := o.otp.LatestActiveByPhone(ctx, p); err == nil && prev != nil &&
		domain.OTPRememberableChannel(prev.Channel) {
		hint.Deprioritize = append(hint.Deprioritize, prev.Channel)
	}

	return hint
}

// recordUndeliveredAttempt writes a burned otp_codes row so a failed delivery
// still consumes the phone's rate-limit budget. See the call site for why.
func (o *otpUseCase) recordUndeliveredAttempt(ctx context.Context, p, code string) {
	now := time.Now()
	used := now
	_ = o.otp.Create(ctx, &domain.OTPCode{
		ID:        uuid.New(),
		Phone:     p,
		CodeHash:  otpcode.Hash(code),
		Channel:   domain.OTPChannelUndelivered,
		UsedAt:    &used,
		ExpiresAt: now.Add(o.cfg.OTPTTL),
		CreatedAt: now,
	})
}

// VerifyOTP checks the latest active code for the phone; on success it marks the
// code used, finds-or-creates the user, and returns a token pair.
func (o *otpUseCase) VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error) {
	p := phone.Normalize(rawPhone)
	if p == "" || code == "" {
		return nil, fmt.Errorf("%w: phone and code required", domain.ErrValidation)
	}

	// Read + attempt accounting happen OUTSIDE the transaction: a failed guess
	// must durably increment attempts (if it were inside the tx that returns the
	// auth error, the rollback would discard it and the lockout would never fire).
	rec, err := o.otp.LatestActiveByPhone(ctx, p)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if rec.Attempts >= maxOTPAttempts {
		return nil, domain.ErrUnauthorized
	}
	if otpcode.Hash(code) != rec.CodeHash {
		_ = o.otp.IncrementAttempts(ctx, rec.ID) // committed immediately (no active tx)
		return nil, domain.ErrUnauthorized
	}

	// Correct code: mark used + find-or-create the user + issue tokens atomically.
	var pair *TokenPair
	err = o.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := o.otp.MarkUsed(ctx, rec.ID); err != nil {
			return err
		}

		u, err := o.users.GetByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			now := time.Now()
			u = &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru", PhoneVerifiedAt: &now}
			if err := o.users.Create(ctx, u); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if u.PhoneVerifiedAt == nil {
			now := time.Now()
			u.PhoneVerifiedAt = &now
			if err := o.users.Update(ctx, u); err != nil {
				return err
			}
		}

		pair, err = issuePair(ctx, o.tokens, o.refresh, o.cfg.RefreshTTL, u)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Nothing to write for the delivery memory: MarkUsed above already recorded
	// it. The row that just became used carries the channel, and "used" is
	// exactly the proof (the guest typed the code back) that no provider API can
	// give us — WhatsApp and SMS both "accept" messages they then drop.
	return pair, nil
}

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

// The two rate-limit windows. They are constants rather than two literals at
// the call site because each is used TWICE: once to count the codes inside it,
// and once as the Retry-After the caller is told to wait. A drift between those
// two would send the guest back exactly one moment too early, every time.
const (
	otpPerMinWindow  = time.Minute
	otpPerHourWindow = time.Hour
)

// OTPUseCase is the phone-OTP authentication usecase: request a one-time code
// and verify it (find-or-create the user, then issue a token pair). It is a
// distinct usecase from the core credential Facade (facade.go).
type OTPUseCase interface {
	RequestOTP(ctx context.Context, rawPhone string) (string, error)
	VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error)

	// RequestPhoneChangeOTP sends an OTP to a NEW number a signed-in user wants
	// to move to. It runs the same generation/send/rate-limit path as
	// RequestOTP (so the new number shares its own per-phone budget) after two
	// guards: the new number must differ from the caller's current one, and it
	// must not already belong to another live account.
	RequestPhoneChangeOTP(ctx context.Context, userID uuid.UUID, rawNewPhone string) (string, error)

	// VerifyPhoneChange verifies the OTP delivered to newPhone and, on success,
	// moves the authenticated user to that number (setting phone_verified_at).
	// It reuses the code-check of VerifyOTP (match / expiry / used / attempt
	// budget) but issues NO token pair and does NOT find-or-create a user.
	// Returns the updated user.
	VerifyPhoneChange(ctx context.Context, userID uuid.UUID, rawNewPhone, code string) (*domain.User, error)
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
		return "", domain.WithCode(domain.CodeOTPInvalidPhone,
			fmt.Errorf("%w: phone required", domain.ErrValidation))
	}

	// Both limits keep their 422 and their message; only the code and the
	// Retry-After are new. The wait is the FULL window, not the exact time
	// remaining: computing the latter needs the timestamp of somebody else's
	// request, and answering with it would tell an outsider the second at which
	// this number last asked for a code — the one piece of timing an OTP
	// phishing call actually wants. An upper bound costs a legitimate guest a
	// few seconds and gives a prober nothing.
	perMin, err := o.otp.CountSince(ctx, p, time.Now().Add(-otpPerMinWindow))
	if err != nil {
		return "", err
	}
	if perMin >= o.cfg.OTPPerMin {
		return "", domain.WithCode(domain.CodeOTPRateLimitedMinute,
			domain.WithRetryAfter(otpPerMinWindow,
				fmt.Errorf("%w: too many requests, wait a minute", domain.ErrValidation)))
	}
	perHour, err := o.otp.CountSince(ctx, p, time.Now().Add(-otpPerHourWindow))
	if err != nil {
		return "", err
	}
	if perHour >= o.cfg.OTPPerHour {
		return "", domain.WithCode(domain.CodeOTPRateLimitedHour,
			domain.WithRetryAfter(otpPerHourWindow,
				fmt.Errorf("%w: hourly OTP limit reached", domain.ErrValidation)))
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

// errOTPInvalid is the ONE answer to "the code you sent is not accepted".
//
// It is deliberately shared by three different situations — the code is wrong,
// the code expired, and there is no active code for this number at all — and
// they must stay indistinguishable on the wire. See domain.CodeOTPInvalid: the
// difference between them is exactly "does a live code exist for this number
// right now", and publishing that bit turns this endpoint into a presence
// oracle for anyone who knows a phone number.
//
// Built fresh on every call rather than kept as a package-level var so nobody
// can wrap or annotate the shared value by accident.
func errOTPInvalid() error {
	return domain.WithCode(domain.CodeOTPInvalid, domain.ErrUnauthorized)
}

// errOTPTooManyAttempts marks a code that is dead from wrong guesses. Separate
// from errOTPInvalid because retyping cannot help any more — the guest has to
// request a new code, and the app can only say that if it can tell the two
// apart.
func errOTPTooManyAttempts() error {
	return domain.WithCode(domain.CodeOTPTooManyAttempts, domain.ErrUnauthorized)
}

// errPhoneInUse — the new number belongs to another live account. Wraps
// ErrAlreadyExists so the transport layer maps it to 409, with a code the app
// can tell apart from the generic "already exists".
func errPhoneInUse() error {
	return domain.WithCode(domain.CodePhoneInUse,
		fmt.Errorf("%w: phone already in use", domain.ErrAlreadyExists))
}

// errPhoneUnchanged — the new number normalizes to the caller's current one.
// Wraps ErrValidation → 422.
func errPhoneUnchanged() error {
	return domain.WithCode(domain.CodePhoneUnchanged,
		fmt.Errorf("%w: new phone is the same as the current one", domain.ErrValidation))
}

// VerifyOTP checks the latest active code for the phone; on success it marks the
// code used, finds-or-creates the user, and returns a token pair.
func (o *otpUseCase) VerifyOTP(ctx context.Context, rawPhone, code string) (*TokenPair, error) {
	p := phone.Normalize(rawPhone)
	if p == "" {
		return nil, domain.WithCode(domain.CodeOTPInvalidPhone,
			fmt.Errorf("%w: phone and code required", domain.ErrValidation))
	}
	if code == "" {
		return nil, domain.WithCode(domain.CodeOTPCodeRequired,
			fmt.Errorf("%w: phone and code required", domain.ErrValidation))
	}

	// Read + attempt accounting happen OUTSIDE the transaction: a failed guess
	// must durably increment attempts (if it were inside the tx that returns the
	// auth error, the rollback would discard it and the lockout would never fire).
	rec, err := o.otp.LatestActiveByPhone(ctx, p)
	if errors.Is(err, domain.ErrNotFound) {
		// No unused, unexpired code for this number — never requested, already
		// used, or expired. Same answer as a wrong code, on purpose.
		return nil, errOTPInvalid()
	}
	if err != nil {
		return nil, err
	}
	if rec.Attempts >= maxOTPAttempts {
		return nil, errOTPTooManyAttempts()
	}
	if otpcode.Hash(code) != rec.CodeHash {
		// Committed immediately (no active tx). When this guess is the one that
		// exhausts the budget, say so straight away instead of inviting the
		// guest to retype into a code that is already dead — but only if the
		// attempt was actually recorded, or the lockout would be a claim about
		// state we failed to write.
		// Counted BEFORE the write: rec is this caller's snapshot, and reading
		// it back after the repository has touched the row makes the answer
		// depend on whether that repository happens to hand out live rows.
		attempts := rec.Attempts + 1
		if err := o.otp.IncrementAttempts(ctx, rec.ID); err == nil &&
			attempts >= maxOTPAttempts {
			return nil, errOTPTooManyAttempts()
		}
		return nil, errOTPInvalid()
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

// RequestPhoneChangeOTP guards a signed-in user's request to move to a new
// number, then delegates delivery to RequestOTP so the number gets exactly the
// same generation, waterfall send and rate-limit budget an unauthenticated
// login request would. The uniqueness check here is only a courtesy 409 before
// spending an SMS — the users.phone UNIQUE constraint is the real guarantee,
// re-checked at write time in VerifyPhoneChange.
func (o *otpUseCase) RequestPhoneChangeOTP(ctx context.Context, userID uuid.UUID, rawNewPhone string) (string, error) {
	p := phone.Normalize(rawNewPhone)
	if p == "" {
		return "", domain.WithCode(domain.CodeOTPInvalidPhone,
			fmt.Errorf("%w: phone required", domain.ErrValidation))
	}

	caller, err := o.users.GetByID(ctx, userID)
	if err != nil {
		return "", err
	}
	if caller.Phone != nil && *caller.Phone == p {
		return "", errPhoneUnchanged()
	}
	if err := o.assertPhoneFree(ctx, p, userID); err != nil {
		return "", err
	}

	// Same path as an unauthenticated request: generate, send through the
	// waterfall, record the row that spends the NEW number's rate-limit budget.
	return o.RequestOTP(ctx, p)
}

// assertPhoneFree returns errPhoneInUse when p already belongs to a different,
// non-soft-deleted account. A soft-deleted row NULLs its phone (see
// UserRepository.Delete), so it never collides here. ErrNotFound — nobody has
// the number — is the success case.
func (o *otpUseCase) assertPhoneFree(ctx context.Context, p string, userID uuid.UUID) error {
	existing, err := o.users.GetByPhone(ctx, p)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.ID != userID && existing.DeletedAt == nil {
		return errPhoneInUse()
	}
	return nil
}

// VerifyPhoneChange checks the OTP for newPhone with the SAME logic as
// VerifyOTP (read + attempt accounting outside the transaction so a wrong guess
// durably counts), but on success it neither creates a user nor issues tokens:
// it moves the authenticated caller to newPhone and marks it verified, inside
// one transaction that re-checks uniqueness at write time.
func (o *otpUseCase) VerifyPhoneChange(ctx context.Context, userID uuid.UUID, rawNewPhone, code string) (*domain.User, error) {
	p := phone.Normalize(rawNewPhone)
	if p == "" {
		return nil, domain.WithCode(domain.CodeOTPInvalidPhone,
			fmt.Errorf("%w: phone and code required", domain.ErrValidation))
	}
	if code == "" {
		return nil, domain.WithCode(domain.CodeOTPCodeRequired,
			fmt.Errorf("%w: phone and code required", domain.ErrValidation))
	}

	caller, err := o.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if caller.Phone != nil && *caller.Phone == p {
		return nil, errPhoneUnchanged()
	}

	// Attempt accounting stays OUTSIDE the transaction, exactly as VerifyOTP
	// does it: a failed guess must durably count even though the request
	// returns an error, or the lockout would never fire.
	rec, err := o.otp.LatestActiveByPhone(ctx, p)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, errOTPInvalid()
	}
	if err != nil {
		return nil, err
	}
	// TODO(security): the read + IncrementAttempts here is not atomic (the same
	// pre-existing gap as login VerifyOTP: two concurrent guesses can both read
	// the same Attempts snapshot and each spend one of the same budget slot).
	// Deferred as a separate hardening task that must fix BOTH paths together —
	// do not diverge this one from VerifyOTP in the meantime.
	if rec.Attempts >= maxOTPAttempts {
		return nil, errOTPTooManyAttempts()
	}
	if otpcode.Hash(code) != rec.CodeHash {
		attempts := rec.Attempts + 1
		if err := o.otp.IncrementAttempts(ctx, rec.ID); err == nil &&
			attempts >= maxOTPAttempts {
			return nil, errOTPTooManyAttempts()
		}
		return nil, errOTPInvalid()
	}

	// Correct code. Mark it used and move the caller to the new number in ONE
	// transaction, re-checking uniqueness at write time: the pre-check in the
	// request step is advisory, the UNIQUE constraint (via Update →
	// ErrAlreadyExists) is what actually forbids a collision that raced in.
	var out *domain.User
	var oldPhone *string
	err = o.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := o.otp.MarkUsed(ctx, rec.ID); err != nil {
			return err
		}
		if err := o.assertPhoneFree(ctx, p, userID); err != nil {
			return err
		}
		u, err := o.users.GetByID(ctx, userID)
		if err != nil {
			return err
		}
		oldPhone = u.Phone
		now := time.Now()
		u.Phone = &p
		u.PhoneVerifiedAt = &now
		if err := o.users.Update(ctx, u); err != nil {
			// The advisory assertPhoneFree above cannot close the race between
			// its read and this write; the users.phone UNIQUE constraint does,
			// surfacing as ErrAlreadyExists. Tag it with the same CodePhoneInUse
			// the pre-check returns so this rare path answers the identical 409
			// the handler documents, not a generic already_exists.
			if errors.Is(err, domain.ErrAlreadyExists) {
				return domain.WithCode(domain.CodePhoneInUse, err)
			}
			return err
		}
		out = u
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Best-effort hygiene AFTER the change is committed: any other live code for
	// the old or the new number can no longer complete anything useful, so kill
	// them. Deliberately outside the transaction and error-swallowed — a cleanup
	// hiccup must never undo a phone change the caller already succeeded at.
	if oldPhone != nil && *oldPhone != p {
		_ = o.otp.InvalidateActiveByPhone(ctx, *oldPhone)
	}
	_ = o.otp.InvalidateActiveByPhone(ctx, p)

	return out, nil
}

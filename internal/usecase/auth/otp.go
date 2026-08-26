package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/otpcode"
	"backend-core/internal/auth/phone"
	"backend-core/internal/domain"
	"backend-core/internal/logging"
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
	users    domain.UserRepository
	otp      domain.OTPRepository
	refresh  domain.RefreshTokenRepository
	bookings guestBookingLinker
	tx       domain.TxManager
	tokens   TokenIssuer
	sender   OTPSender
	cfg      Config
	// testAcc is the App Store review account, resolved once at construction.
	// Zero value = disabled; see test_account.go.
	testAcc testAccount
}

// NewOTPUseCase constructs the phone-OTP authentication usecase.
func NewOTPUseCase(
	users domain.UserRepository,
	otp domain.OTPRepository,
	refresh domain.RefreshTokenRepository,
	bookings guestBookingLinker,
	tx domain.TxManager,
	tokens TokenIssuer,
	sender OTPSender,
	cfg Config,
) OTPUseCase {
	return &otpUseCase{
		users:    users,
		otp:      otp,
		refresh:  refresh,
		bookings: bookings,
		tx:       tx,
		tokens:   tokens,
		sender:   sender,
		cfg:      cfg,
		testAcc:  newTestAccount(cfg),
	}
}

// otpPersistTimeout bounds the write that stores an already-sent code. It is
// deliberately short: the row is small and the only reason to wait is a
// struggling database, not a slow caller.
const otpPersistTimeout = 5 * time.Second

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

	// The App Store review account answers success without sending anything and
	// without writing an otp_codes row. Placed BEFORE the rate-limit counters on
	// purpose — see the block comment in test_account.go and the note below on
	// why the per-phone budget does not apply to this one number.
	//
	// The per-phone limits (1/min, 5/hour) exist to protect two things: the SMS
	// bill and a real person's phone from being used as a doorbell. Neither can
	// happen here — nothing is sent, nothing is billed, and the number belongs
	// to nobody. What they WOULD do is break the review: a reviewer who taps
	// "resend" twice, or a second reviewer on the same submission, would be
	// locked out for a minute with an error the app shows as a failure. The
	// endpoint is still not open: the per-IP strict limiter in front of it
	// (RATE_LIMIT_STRICT_LIMIT, 5/min per IP per route) is untouched and applies
	// to this number exactly as it does to every other.
	if o.testAcc.matches(p) {
		o.testAcc.logRequest(ctx)
		if o.cfg.OTPDevExpose {
			return o.testAcc.code, nil
		}
		return "", nil
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
	// СОХРАНЯЕМ ВНЕ КОНТЕКСТА ЗАПРОСА. Код уже ушёл человеку на телефон, и с
	// этой секунды он существует в мире независимо от того, ждёт ли ещё
	// приложение ответа. Если гость свернул экран или у клиента истёк свой
	// таймаут, отмена запроса не должна отменять запись: иначе код придёт, а
	// войти по нему будет нельзя — ровно это и случилось 24.08.2026, когда
	// доставка через Telegram Gateway заняла около восьми секунд, приложение
	// оборвало запрос, и `create otp` упал с context canceled.
	//
	// Отдельный короткий таймаут: писать бесконечно тоже нельзя, иначе
	// зависшая база задержит горутину после ухода клиента.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), otpPersistTimeout)
	defer cancel()
	if err := o.otp.Create(saveCtx, rec); err != nil {
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
	// Тоже вне контекста запроса: строка нужна счётчику ограничений, а он
	// защищает от перебора и от трат на отправку. Клиент, отвалившийся в этот
	// момент, не должен обнулять эту защиту.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), otpPersistTimeout)
	defer cancel()
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

// logAttachedBookings reports how many account-less bookings a proof of phone
// ownership just handed to a guest. Called AFTER the transaction commits, so
// the line can never claim an attach that got rolled back, and silent on zero:
// the vast majority of logins have nothing to attach, and a line per login
// would bury the ones that matter.
//
// The phone is masked (logging.MaskPhone) — a log line is not a place to keep
// a full contact number.
func logAttachedBookings(ctx context.Context, userID uuid.UUID, p string, attached int64) {
	if attached <= 0 {
		return
	}
	logging.FromContext(ctx).Info(logging.EventGuestBookingsLinked,
		slog.String("user_id", userID.String()),
		slog.String("phone_masked", logging.MaskPhone(p)),
		slog.Int64("bookings_attached", attached),
	)
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

	// App Store review account: the fixed code, and only it, opens this number.
	// There is no otp_codes row to read, no attempt counter to bump and no
	// lockout — every wrong guess answers with the same errOTPInvalid() any
	// other number gets, and every attempt (right or wrong) leaves a WARN line.
	if o.testAcc.matches(p) {
		if !o.testAcc.codeAccepted(code) {
			o.testAcc.logVerify(ctx, false)
			return nil, errOTPInvalid()
		}
		o.testAcc.logVerify(ctx, true)
		// From here on it is an ordinary login: the account is created on first
		// use and behaves like any other guest afterwards.
		return o.completeLogin(ctx, p, nil)
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

	// Correct code: mark used + find-or-create the user + attach the bookings
	// made for this number before the account existed + issue tokens, atomically.
	return o.completeLogin(ctx, p, rec)
}

// completeLogin turns a proven ownership of phone p into a session, in ONE
// transaction: burn the code, find-or-create the user, attach the bookings made
// for that number before the account existed, issue the token pair.
//
// rec is the code that was just accepted, and may be nil — that is the App
// Store review account, which has no otp_codes row to mark used. Everything
// after that point is deliberately identical for both callers: the review
// account must exercise the same login the real one does, otherwise the review
// proves nothing about the app the guests use.
func (o *otpUseCase) completeLogin(ctx context.Context, p string, rec *domain.OTPCode) (*TokenPair, error) {
	var pair *TokenPair
	var attached int64
	var userID uuid.UUID
	var created bool
	err := o.tx.WithinTx(ctx, func(ctx context.Context) error {
		if rec != nil {
			if err := o.otp.MarkUsed(ctx, rec.ID); err != nil {
				return err
			}
		}

		u, err := o.users.GetByPhone(ctx, p)
		if errors.Is(err, domain.ErrNotFound) {
			now := time.Now()
			u = &domain.User{ID: uuid.New(), Phone: &p, Role: domain.RoleUser, IsActive: true, PreferredLanguage: "ru", PhoneVerifiedAt: &now}
			if err := o.users.Create(ctx, u); err != nil {
				return err
			}
			created = true
		} else if err != nil {
			return err
		} else if u.PhoneVerifiedAt == nil {
			now := time.Now()
			u.PhoneVerifiedAt = &now
			if err := o.users.Update(ctx, u); err != nil {
				return err
			}
		}

		// Same transaction as the user row on purpose: a guest must never end up
		// created-but-without-their-history, and a failure here must roll the
		// whole login back so the next attempt does the job. The write itself
		// cannot take anything from anybody (user_id IS NULL only), so the worst
		// case of a retry is attaching zero rows.
		userID = u.ID
		attached, err = o.bookings.AttachOrphanedByPhone(ctx, u.ID, p)
		if err != nil {
			return fmt.Errorf("attach account-less bookings: %w", err)
		}

		pair, err = issuePair(ctx, o.tokens, o.refresh, o.cfg.RefreshTTL, u)
		if err != nil {
			return err
		}
		// Carried out of the transaction so the app can tell a first-ever login
		// apart from a returning guest. Assigned here, inside the closure, so a
		// rolled-back attempt can never leave the flag set on a pair we return.
		pair.IsNewUser = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	logAttachedBookings(ctx, userID, p, attached)

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

	// The App Store review number is reserved: it is not a destination a
	// signed-in account may move onto. Allowing it would let anybody with a
	// session take over the reviewer's account (or strand the next review on an
	// account they can no longer reach). Answered with the same 409 an occupied
	// number gets — which is exactly what this is.
	if o.testAcc.matches(p) {
		o.testAcc.logPhoneChangeRefused(ctx, userID)
		return "", errPhoneInUse()
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

	// The App Store review number is reserved: it is not a destination a
	// signed-in account may move onto. Allowing it would let anybody with a
	// session take over the reviewer's account (or strand the next review on an
	// account they can no longer reach). Answered with the same 409 an occupied
	// number gets — which is exactly what this is.
	if o.testAcc.matches(p) {
		o.testAcc.logPhoneChangeRefused(ctx, userID)
		return nil, errPhoneInUse()
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
	var attached int64
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
		// Same rule as a login, for the same reason: the caller has just proved
		// ownership of this number, and bookings made for it while it had no
		// account belong to whoever owns the number. Without this the guest
		// would have to wait for their next full OTP login to see them — the
		// hole the "no backfill job, ever" requirement exists to close. Bookings
		// that came with the OLD number are NOT touched: they already have an
		// owner (this same user), and the write only ever moves a booking from
		// nobody to somebody.
		attached, err = o.bookings.AttachOrphanedByPhone(ctx, u.ID, p)
		if err != nil {
			return fmt.Errorf("attach account-less bookings: %w", err)
		}

		out = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	logAttachedBookings(ctx, userID, p, attached)

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

package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/initdata"
	"backend-core/internal/auth/password"
	"backend-core/internal/domain"
)

// StaffVenue is one venue the signed-in employee works at, as the mini app needs
// it: the id it will scope every later request to, the display name, and the
// role that decides which controls are drawn.
//
// It carries the raw i18n map alongside the resolved name so the transport layer
// can localize per request, exactly like GET /admin/my-restaurants does.
type StaffVenue struct {
	RestaurantID uuid.UUID
	Name         string
	NameI18n     domain.I18n
	Role         string
}

// VenueLister answers "which venues does this account work at". It is a port and
// not a direct import of usecase/restaurants so this package keeps depending on
// nothing but domain; bootstrap passes the adapter over MyRestaurantsUseCase,
// which means the mini app and the admin panel's venue picker can never drift
// into two different answers about who works where.
type VenueLister interface {
	ListForStaff(ctx context.Context, userID uuid.UUID, role domain.Role) ([]StaffVenue, error)
}

// MiniAppSession is what all three endpoints hand back on success: a token pair
// the mini app uses for every subsequent call, who it belongs to, and the venues
// it may act in.
type MiniAppSession struct {
	Pair   *TokenPair
	User   *domain.User
	Venues []StaffVenue
}

// MiniAppConfig is the tuning of the venue mini app's sign-in.
type MiniAppConfig struct {
	// BotToken is @book_eat_restaurants_bot's token — the key every initData
	// signature is checked against. EMPTY DISABLES all three endpoints (they
	// answer 404), the same "not configured means it does not exist" rule the
	// inbound webhook already follows. The alternative — accepting initData
	// without a key to check it against — is a sign-in as anybody.
	BotToken string
	// InitDataTTL is how long a signed blob stays usable. It bounds the damage
	// of one leaking: past it, the blob is a string in a log rather than a key.
	InitDataTTL time.Duration
}

// MiniAppUseCase is sign-in for the Telegram venue mini app (spec §5.2 A–D).
//
// # The shape of the flow, and why it is not simpler
//
// Telegram's initData proves that a request came from our bot's mini app and
// names the Telegram account that opened it. It does NOT prove that the person
// holding the phone is staff — anyone can open the bot. So the first sign-in
// costs an email and a password (Link), and only after that does initData work
// on its own (SignIn), as the identifier of a device a password was once pinned
// to.
//
// Every path re-reads the venue memberships live. A link is a memory of a
// password check, never a grant: an employee removed from their last restaurant
// is refused on their next open and their link is revoked on the spot, without
// anybody having to remember to clean it up.
type MiniAppUseCase struct {
	links   domain.TelegramStaffLinkRepository
	users   domain.UserRepository
	creds   domain.UserCredentialRepository
	refresh domain.RefreshTokenRepository
	venues  VenueLister
	tx      domain.TxManager
	tokens  TokenIssuer
	cfg     Config
	mini    MiniAppConfig
	log     *slog.Logger
	now     func() time.Time
}

// NewMiniAppUseCase wires the mini app's sign-in. log may be nil (the default
// logger is used); now may be nil (time.Now), and is injected so the freshness
// window is testable without sleeping.
func NewMiniAppUseCase(
	links domain.TelegramStaffLinkRepository,
	users domain.UserRepository,
	creds domain.UserCredentialRepository,
	refresh domain.RefreshTokenRepository,
	venues VenueLister,
	tx domain.TxManager,
	tokens TokenIssuer,
	cfg Config,
	mini MiniAppConfig,
	log *slog.Logger,
) *MiniAppUseCase {
	if log == nil {
		log = slog.Default()
	}
	return &MiniAppUseCase{
		links: links, users: users, creds: creds, refresh: refresh,
		venues: venues, tx: tx, tokens: tokens, cfg: cfg, mini: mini,
		log: log, now: time.Now,
	}
}

// Configured reports whether the bot token is set. The transport layer turns a
// false here into a 404 on all three routes.
func (u *MiniAppUseCase) Configured() bool { return u.mini.BotToken != "" }

// verify checks an initData blob and maps its failures onto the codes the mini
// app branches on. It is the first thing every endpoint does — before a password
// is read, before the database is touched — so an unsigned request can never
// spend a bcrypt comparison or a row lookup.
func (u *MiniAppUseCase) verify(raw string) (*initdata.Data, error) {
	d, err := initdata.Verify(raw, u.mini.BotToken, u.mini.InitDataTTL, u.now())
	switch {
	case err == nil:
		return d, nil
	case errors.Is(err, initdata.ErrExpired):
		u.log.Warn("miniapp.init_data_expired")
		return nil, domain.WithCode(domain.CodeInitDataExpired, domain.ErrUnauthorized)
	default:
		// Warn, not Info: a forged or foreign-bot signature is somebody probing
		// the endpoint. The blob itself is NOT logged — it carries a real name
		// and a Telegram id.
		u.log.Warn("miniapp.init_data_invalid")
		return nil, domain.WithCode(domain.CodeInitDataInvalid, domain.ErrUnauthorized)
	}
}

// SignIn is the ordinary open of the mini app: no password, just the remembered
// link (spec §5.2 A).
//
// It refuses in three distinguishable ways, because the app draws a different
// screen for each: init_data_* (reopen from the bot), link_required (show the
// password form), staff_not_found (say access was withdrawn).
func (u *MiniAppUseCase) SignIn(ctx context.Context, rawInitData string) (*MiniAppSession, error) {
	d, err := u.verify(rawInitData)
	if err != nil {
		return nil, err
	}

	link, err := u.links.GetByTelegramUserID(ctx, d.User.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.WithCode(domain.CodeLinkRequired, domain.ErrForbidden)
	}
	if err != nil {
		return nil, err
	}
	if !link.Active() {
		// A revoked link is the same answer as no link at all: show the password
		// form. Anything else would strand somebody whose access was restored.
		return nil, domain.WithCode(domain.CodeLinkRequired, domain.ErrForbidden)
	}

	user, err := u.users.GetByID(ctx, link.UserID)
	if errors.Is(err, domain.ErrNotFound) {
		// The account is gone but the row survived (it should have cascaded).
		// Treat it as a revoked link rather than a 500.
		_ = u.links.Revoke(ctx, d.User.ID)
		return nil, domain.WithCode(domain.CodeLinkRequired, domain.ErrForbidden)
	}
	if err != nil {
		return nil, err
	}
	if !user.IsActive {
		u.revokeAccess(ctx, d.User.ID, user.ID, "miniapp.user_inactive")
		return nil, domain.WithCode(domain.CodeStaffNotFound, domain.ErrForbidden)
	}

	venues, err := u.venues.ListForStaff(ctx, user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	if len(venues) == 0 {
		// Criterion 10: fired from the last venue. The link dies here, on the
		// read path, so no separate cleanup job has to be right for access to
		// end when employment does.
		u.revokeAccess(ctx, d.User.ID, user.ID, "miniapp.membership_lost")
		return nil, domain.WithCode(domain.CodeStaffNotFound, domain.ErrForbidden)
	}

	var pair *TokenPair
	if err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		pair, err = issuePair(ctx, u.tokens, u.refresh, u.cfg.RefreshTTL, user)
		return err
	}); err != nil {
		return nil, err
	}
	// Telemetry only, and outside the transaction: a failure to record the visit
	// must not cost somebody their sign-in mid-shift.
	if err := u.links.TouchLastSeen(ctx, d.User.ID); err != nil {
		u.log.Warn("miniapp.touch_last_seen_failed", slog.String("error", err.Error()))
	}
	return &MiniAppSession{Pair: pair, User: user, Venues: venues}, nil
}

// Link is the FIRST sign-in: email + password, then the link is written (spec
// §5.2 B).
//
// The order of checks is fixed — initData, then the password, then membership —
// and each stage is a precondition of the next. Checking membership before the
// password would answer "is this address staff here" to anyone who asks;
// checking the password before initData would let an unsigned request burn
// bcrypt time and turn the endpoint into a password oracle with a rate limit
// more generous than /auth/login's. That is also why the route is registered as
// TierStrict in middleware.routeTiers: it reaches the same password check as
// /auth/login and must not be an easier door to it.
func (u *MiniAppUseCase) Link(ctx context.Context, rawInitData, email, pw string) (*MiniAppSession, error) {
	d, err := u.verify(rawInitData)
	if err != nil {
		return nil, err
	}
	if email == "" || pw == "" {
		return nil, domain.WithCode(domain.CodeInvalidCredentials, domain.ErrUnauthorized)
	}

	user, err := u.authenticate(ctx, email, pw)
	if err != nil {
		return nil, err
	}

	venues, err := u.venues.ListForStaff(ctx, user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	if len(venues) == 0 {
		// Criterion 9: a guest's real password is still a correct password. It
		// buys nothing here, and NO link is written — a row would let the same
		// account walk in without a password the moment somebody made it staff.
		u.log.Warn("miniapp.link_refused_not_staff", slog.String("user_id", user.ID.String()))
		return nil, domain.WithCode(domain.CodeStaffNotFound, domain.ErrForbidden)
	}

	// Criterion 12: this Telegram account may already point at somebody else —
	// a colleague handing the phone over. Read the previous owner BEFORE the
	// upsert overwrites it, so the old sessions can be cut and the swap logged.
	var previous *uuid.UUID
	if prev, err := u.links.GetByTelegramUserID(ctx, d.User.ID); err == nil && prev.UserID != user.ID {
		id := prev.UserID
		previous = &id
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	var pair *TokenPair
	// The link and the tokens are written in ONE transaction: a link without a
	// session leaves the app looping on a form it already passed, and a session
	// without a link makes the next open ask for the password again.
	if err := u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if previous != nil {
			if err := u.refresh.RevokeAllByUser(ctx, *previous); err != nil {
				return err
			}
		}
		l := &domain.TelegramStaffLink{
			TelegramUserID: d.User.ID,
			UserID:         user.ID,
			// The private chat with the bot has the same id as the user, which
			// is what makes an alert addressable to the person and not only to
			// the venue's shared chat.
			ChatID: d.User.ID,
		}
		if err := u.links.Upsert(ctx, l); err != nil {
			return err
		}
		var err error
		pair, err = issuePair(ctx, u.tokens, u.refresh, u.cfg.RefreshTTL, user)
		return err
	}); err != nil {
		return nil, err
	}
	if previous != nil {
		u.log.Info("miniapp.link_replaced",
			slog.String("previous_user_id", previous.String()),
			slog.String("user_id", user.ID.String()))
	} else {
		u.log.Info("miniapp.linked", slog.String("user_id", user.ID.String()))
	}
	return &MiniAppSession{Pair: pair, User: user, Venues: venues}, nil
}

// Unlink is «Выйти» (spec §5.2 C): the link is revoked and every refresh token
// of the account is cut, so closing the mini app is not the same as staying
// signed in on a phone that changed hands.
//
// It accepts either a signed initData blob or, when the caller is already
// authenticated, the bearer user id — the mini app has both and either is proof
// enough to end one's OWN session. userID is optional; pass uuid.Nil to identify
// the link by initData alone.
func (u *MiniAppUseCase) Unlink(ctx context.Context, rawInitData string, userID uuid.UUID) error {
	if rawInitData != "" {
		d, err := u.verify(rawInitData)
		if err != nil {
			return err
		}
		link, err := u.links.GetByTelegramUserID(ctx, d.User.ID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			// Nothing to revoke is the outcome the caller asked for. Answer
			// success rather than 404: signing out must never fail.
			return nil
		case err != nil:
			return err
		}
		// A bearer that names a different account than the link may not end it:
		// otherwise anyone with a captured blob could sign a colleague out.
		if userID != uuid.Nil && link.UserID != userID {
			return domain.WithCode(domain.CodeForbidden, domain.ErrForbidden)
		}
		return u.revokeSession(ctx, d.User.ID, link.UserID)
	}

	if userID == uuid.Nil {
		return domain.WithCode(domain.CodeValidation, domain.ErrValidation)
	}
	// Bearer-only: sign every device of this account out of the mini app.
	if _, err := u.links.RevokeByUser(ctx, userID); err != nil {
		return err
	}
	if err := u.refresh.RevokeAllByUser(ctx, userID); err != nil {
		return err
	}
	u.log.Info("miniapp.unlinked", slog.String("user_id", userID.String()))
	return nil
}

// authenticate is the same email + password check as Facade.Login, including the
// dummy-hash comparison on the "no such account" branch that keeps the response
// time from revealing whether an address is registered. It returns ONE code for
// every failure for the same reason.
func (u *MiniAppUseCase) authenticate(ctx context.Context, email, pw string) (*domain.User, error) {
	deny := domain.WithCode(domain.CodeInvalidCredentials, domain.ErrUnauthorized)
	user, err := u.users.GetByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		password.Verify(dummyPasswordHash, pw)
		return nil, deny
	}
	if err != nil {
		return nil, err
	}
	cred, err := u.creds.GetByUserID(ctx, user.ID)
	if errors.Is(err, domain.ErrNotFound) {
		password.Verify(dummyPasswordHash, pw)
		return nil, deny
	}
	if err != nil {
		return nil, err
	}
	if !password.Verify(cred.PasswordHash, pw) {
		return nil, deny
	}
	if !user.IsActive {
		// A disabled account is not a wrong password, but the mini app must not
		// be the place that tells them apart.
		return nil, deny
	}
	return user, nil
}

// revokeSession revokes the link and cuts the account's refresh tokens.
func (u *MiniAppUseCase) revokeSession(ctx context.Context, telegramUserID int64, userID uuid.UUID) error {
	return u.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := u.links.Revoke(ctx, telegramUserID); err != nil {
			return err
		}
		return u.refresh.RevokeAllByUser(ctx, userID)
	})
}

// revokeAccess ends access on a REFUSAL path, where the caller is about to be
// answered 403 anyway. Failures are logged and swallowed on purpose: the refusal
// is what protects the account, and turning a bookkeeping error into a 500 would
// replace a clear "access withdrawn" screen with an unexplained crash.
func (u *MiniAppUseCase) revokeAccess(ctx context.Context, telegramUserID int64, userID uuid.UUID, reason string) {
	if err := u.revokeSession(ctx, telegramUserID, userID); err != nil {
		u.log.Warn("miniapp.revoke_failed", slog.String("reason", reason), slog.String("error", err.Error()))
		return
	}
	u.log.Info(reason, slog.String("user_id", userID.String()))
}

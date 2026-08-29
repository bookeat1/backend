package auth

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"backend-core/internal/auth/initdata"
	"backend-core/internal/auth/password"
	"backend-core/internal/domain"
)

const (
	miniAppBot       = "7654321:AAH-our-restaurants-bot"
	foreignBot       = "1234567:AAF-somebody-elses-bot"
	tgAnya     int64 = 4242
	tgBoss     int64 = 5353
)

// --- fakes ------------------------------------------------------------------

type fakeLinks struct {
	rows map[int64]*domain.TelegramStaffLink
	// touched counts TouchLastSeen calls; touchErr makes it fail, so a test can
	// prove a telemetry write never costs somebody their sign-in.
	touched  int
	touchErr error
}

func newFakeLinks() *fakeLinks {
	return &fakeLinks{rows: map[int64]*domain.TelegramStaffLink{}}
}

func (f *fakeLinks) GetByTelegramUserID(_ context.Context, id int64) (*domain.TelegramStaffLink, error) {
	if l, ok := f.rows[id]; ok {
		cp := *l
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeLinks) Upsert(_ context.Context, l *domain.TelegramStaffLink) error {
	cp := *l
	cp.LinkedAt = time.Now()
	cp.RevokedAt = nil
	f.rows[l.TelegramUserID] = &cp
	l.RevokedAt = nil
	return nil
}

func (f *fakeLinks) Revoke(_ context.Context, id int64) error {
	if l, ok := f.rows[id]; ok && l.RevokedAt == nil {
		now := time.Now()
		l.RevokedAt = &now
	}
	return nil
}

func (f *fakeLinks) RevokeByUser(_ context.Context, userID uuid.UUID) (int, error) {
	n := 0
	now := time.Now()
	for _, l := range f.rows {
		if l.UserID == userID && l.RevokedAt == nil {
			l.RevokedAt = &now
			n++
		}
	}
	return n, nil
}

func (f *fakeLinks) TouchLastSeen(_ context.Context, id int64) error {
	f.touched++
	if f.touchErr != nil {
		return f.touchErr
	}
	if l, ok := f.rows[id]; ok {
		now := time.Now()
		l.LastSeenAt = &now
	}
	return nil
}

// fakeVenues is the membership reader. venues[userID] is what that account works
// at right now — emptying it is how a test fires somebody mid-scenario.
type fakeVenues struct {
	venues map[uuid.UUID][]StaffVenue
	err    error
}

func (f *fakeVenues) ListForStaff(_ context.Context, userID uuid.UUID, _ domain.Role) ([]StaffVenue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.venues[userID], nil
}

// --- harness ----------------------------------------------------------------

type miniAppFixture struct {
	uc     *MiniAppUseCase
	links  *fakeLinks
	users  *fakeUsers
	creds  *fakeCreds
	refr   *fakeRefresh
	venues *fakeVenues
	now    time.Time
}

func newMiniApp(t *testing.T) *miniAppFixture {
	t.Helper()
	f := &miniAppFixture{
		links:  newFakeLinks(),
		users:  newFakeUsers(),
		creds:  newFakeCreds(),
		refr:   newFakeRefresh(),
		venues: &fakeVenues{venues: map[uuid.UUID][]StaffVenue{}},
		now:    time.Now(),
	}
	f.uc = NewMiniAppUseCase(
		f.links, f.users, f.creds, f.refr, f.venues, noTx{}, testIssuer(t),
		Config{RefreshTTL: time.Hour},
		MiniAppConfig{BotToken: miniAppBot, InitDataTTL: time.Hour},
		nil,
	)
	f.uc.now = func() time.Time { return f.now }
	return f
}

// staff creates an account with a password and one venue.
func (f *miniAppFixture) staff(t *testing.T, email, pw string) *domain.User {
	t.Helper()
	u := f.account(t, email, pw)
	f.venues.venues[u.ID] = []StaffVenue{{
		RestaurantID: uuid.New(), Name: "Del Papa", Role: "hostess",
	}}
	return u
}

// account creates an account with a password and NO venue — a guest.
func (f *miniAppFixture) account(t *testing.T, email, pw string) *domain.User {
	t.Helper()
	id := uuid.New()
	mail := email
	u := &domain.User{ID: id, Email: &mail, FullName: "Аня", Role: domain.RoleUser, IsActive: true}
	if err := f.users.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	hash, err := password.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.creds.Upsert(context.Background(), &domain.UserCredential{UserID: id, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	return u
}

// initData builds a genuinely signed blob for a Telegram account.
func (f *miniAppFixture) initData(tgID int64, botToken string, authDate time.Time) string {
	v := url.Values{}
	v.Set("user", `{"id":`+strconv.FormatInt(tgID, 10)+`,"first_name":"Аня"}`)
	v.Set("auth_date", strconv.FormatInt(authDate.Unix(), 10))
	v.Set("hash", initdata.Sign(v, botToken))
	return v.Encode()
}

// good is the ordinary case: our bot, stamped now.
func (f *miniAppFixture) good(tgID int64) string {
	return f.initData(tgID, miniAppBot, f.now)
}

// assertCode fails unless err carries the expected machine-readable code — the
// thing the mini app branches on, which a status alone does not pin down.
func assertCode(t *testing.T, err error, want domain.ErrorCode, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got success", want)
	}
	got, ok := domain.CodeOf(err)
	if !ok || got != want {
		t.Fatalf("code = %q (ok=%v), want %q — err: %v", got, ok, want, err)
	}
	if sentinel != nil && !errors.Is(err, sentinel) {
		t.Fatalf("err does not wrap %v: %v", sentinel, err)
	}
}

// --- initData authenticity (criteria 1-3, 5) --------------------------------

func TestMiniAppRejectsATamperedBlobOnEveryEndpoint(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")

	v, _ := url.ParseQuery(f.good(tgAnya))
	v.Set("user", `{"id":9999,"first_name":"Чужой"}`) // hash untouched
	forged := v.Encode()

	if _, err := f.uc.SignIn(context.Background(), forged); true {
		assertCode(t, err, domain.CodeInitDataInvalid, domain.ErrUnauthorized)
	}
	if _, err := f.uc.Link(context.Background(), forged, "hostess@delpapa.kz", "s3cret123"); true {
		assertCode(t, err, domain.CodeInitDataInvalid, domain.ErrUnauthorized)
	}
	if err := f.uc.Unlink(context.Background(), forged, uuid.Nil); true {
		assertCode(t, err, domain.CodeInitDataInvalid, domain.ErrUnauthorized)
	}
	if len(f.links.rows) != 0 {
		t.Fatal("a forged blob created a link")
	}
}

// Criterion 2: correct credentials + a blob signed by SOMEBODY ELSE'S bot must
// not link anything. This is the case that separates "we check a signature" from
// "we check a signature against our own key".
func TestMiniAppRejectsAForeignBotsSignature(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")
	raw := f.initData(tgAnya, foreignBot, f.now)

	_, err := f.uc.Link(context.Background(), raw, "hostess@delpapa.kz", "s3cret123")
	assertCode(t, err, domain.CodeInitDataInvalid, domain.ErrUnauthorized)
	if len(f.links.rows) != 0 {
		t.Fatal("a foreign bot's blob created a link even though the password was right")
	}
}

// Criterion 3: an expired blob is a DIFFERENT code from an invalid one, because
// the mini app can fix one by reopening itself and can do nothing about the other.
func TestMiniAppRejectsAStaleBlobWithItsOwnCode(t *testing.T) {
	f := newMiniApp(t)
	raw := f.initData(tgAnya, miniAppBot, f.now.Add(-3*time.Hour))
	_, err := f.uc.SignIn(context.Background(), raw)
	assertCode(t, err, domain.CodeInitDataExpired, domain.ErrUnauthorized)
}

// Criterion 5: no bot token configured → the endpoints report themselves absent
// rather than accepting an unverifiable blob.
func TestMiniAppIsDisabledWithoutABotToken(t *testing.T) {
	f := newMiniApp(t)
	f.uc.mini.BotToken = ""
	if f.uc.Configured() {
		t.Fatal("Configured() is true without a bot token — the transport layer would mount the routes")
	}
	if _, err := f.uc.SignIn(context.Background(), f.good(tgAnya)); err == nil {
		t.Fatal("signed in with no bot token to verify against")
	}
}

// The password check must never run before the signature does: otherwise the
// endpoint is a password oracle for anyone who can POST to it.
func TestMiniAppChecksTheSignatureBeforeThePassword(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")
	// Wrong bot AND wrong password: the answer must name the signature, which
	// is only possible if that is what was checked first.
	_, err := f.uc.Link(context.Background(), f.initData(tgAnya, foreignBot, f.now), "hostess@delpapa.kz", "wrong")
	assertCode(t, err, domain.CodeInitDataInvalid, domain.ErrUnauthorized)
}

// --- first sign-in (criteria 6, 7, 9) ---------------------------------------

// Criterion 6: a Telegram account nobody linked gets link_required — the ONE
// code that means "draw the password form".
func TestSignInWithoutALinkAsksForTheForm(t *testing.T) {
	f := newMiniApp(t)
	_, err := f.uc.SignIn(context.Background(), f.good(tgAnya))
	assertCode(t, err, domain.CodeLinkRequired, domain.ErrForbidden)
}

// Criterion 7: correct credentials link exactly one row and return a session.
func TestLinkWithCorrectCredentials(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")

	s, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123")
	if err != nil {
		t.Fatalf("link failed: %v", err)
	}
	if s.Pair == nil || s.Pair.AccessToken == "" || s.Pair.RefreshToken == "" {
		t.Fatal("no token pair issued")
	}
	if s.User.ID != u.ID {
		t.Fatalf("session names user %s, want %s", s.User.ID, u.ID)
	}
	if len(s.Venues) != 1 || s.Venues[0].Role != "hostess" {
		t.Fatalf("venues = %+v, want one hostess membership", s.Venues)
	}
	if len(f.links.rows) != 1 {
		t.Fatalf("%d link rows, want exactly 1", len(f.links.rows))
	}
	if l := f.links.rows[tgAnya]; l == nil || l.UserID != u.ID || !l.Active() {
		t.Fatalf("link = %+v, want an active link to %s", l, u.ID)
	}
}

func TestLinkRejectsAWrongPasswordAndAnUnknownEmailIdentically(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")

	_, wrongPw := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "not-it")
	assertCode(t, wrongPw, domain.CodeInvalidCredentials, domain.ErrUnauthorized)

	_, noSuchUser := f.uc.Link(context.Background(), f.good(tgAnya), "nobody@delpapa.kz", "s3cret123")
	assertCode(t, noSuchUser, domain.CodeInvalidCredentials, domain.ErrUnauthorized)

	if len(f.links.rows) != 0 {
		t.Fatal("a failed sign-in created a link")
	}
}

// Criterion 9: a guest's password is CORRECT and still buys nothing. No link is
// written — one would let the same account walk in without a password later.
func TestLinkRefusesAGuestAccountAndWritesNoLink(t *testing.T) {
	f := newMiniApp(t)
	f.account(t, "guest@example.com", "s3cret123") // no venues

	_, err := f.uc.Link(context.Background(), f.good(tgAnya), "guest@example.com", "s3cret123")
	assertCode(t, err, domain.CodeStaffNotFound, domain.ErrForbidden)
	if len(f.links.rows) != 0 {
		t.Fatalf("a guest sign-in left %d link rows behind", len(f.links.rows))
	}
}

// --- later sign-ins (criteria 8, 10, 13) ------------------------------------

// Criterion 8: the second open needs no password at all.
func TestSignInAfterLinkingNeedsNoPassword(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}

	s, err := f.uc.SignIn(context.Background(), f.good(tgAnya))
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	if s.User.ID != u.ID || len(s.Venues) != 1 {
		t.Fatalf("session = %+v", s)
	}
	if f.links.touched == 0 {
		t.Fatal("last_seen_at was not recorded")
	}
}

// Criterion 10: fired from the last venue → refused AND the link dies on the
// spot, so access ends with employment without a cleanup job having to be right.
func TestSignInRevokesTheLinkWhenTheLastMembershipIsGone(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}
	f.venues.venues[u.ID] = nil // fired

	_, err := f.uc.SignIn(context.Background(), f.good(tgAnya))
	assertCode(t, err, domain.CodeStaffNotFound, domain.ErrForbidden)
	if l := f.links.rows[tgAnya]; l == nil || l.Active() {
		t.Fatalf("link = %+v, want revoked", l)
	}
	// And every session the person still held is cut, not just the next one.
	for _, tok := range f.refr.byHash {
		if tok.UserID == u.ID && tok.RevokedAt == nil {
			t.Fatal("a live refresh token survived the revocation")
		}
	}
}

// A revoked link reads as "show the form again", not as a dead end: somebody
// re-hired must be able to sign back in.
func TestSignInWithARevokedLinkAsksForTheFormAgain(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}
	if err := f.uc.Unlink(context.Background(), f.good(tgAnya), uuid.Nil); err != nil {
		t.Fatal(err)
	}

	_, err := f.uc.SignIn(context.Background(), f.good(tgAnya))
	assertCode(t, err, domain.CodeLinkRequired, domain.ErrForbidden)

	// ...and the password gets them back in, reusing the same row.
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatalf("re-linking after a sign-out failed: %v", err)
	}
	if l := f.links.rows[tgAnya]; l == nil || !l.Active() || l.UserID != u.ID {
		t.Fatalf("link = %+v, want active again", l)
	}
	if len(f.links.rows) != 1 {
		t.Fatalf("%d rows, want the row reused rather than duplicated", len(f.links.rows))
	}
}

// Criterion 13: one BookEat account, two Telegram accounts (a phone and a
// tablet). Both links live. This is why the table has no UNIQUE (user_id).
func TestOneAccountMayBeLinkedFromTwoTelegramAccounts(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")

	for _, tg := range []int64{tgAnya, tgBoss} {
		if _, err := f.uc.Link(context.Background(), f.good(tg), "hostess@delpapa.kz", "s3cret123"); err != nil {
			t.Fatalf("link for %d: %v", tg, err)
		}
	}
	for _, tg := range []int64{tgAnya, tgBoss} {
		if _, err := f.uc.SignIn(context.Background(), f.good(tg)); err != nil {
			t.Fatalf("sign-in for %d: %v", tg, err)
		}
	}
}

// Criterion 12: signing in as somebody else on the SAME phone repoints the link
// and cuts the previous owner's sessions.
func TestLinkReplacementRepointsAndCutsTheOldSessions(t *testing.T) {
	f := newMiniApp(t)
	a := f.staff(t, "anya@delpapa.kz", "s3cret123")
	b := f.staff(t, "boss@delpapa.kz", "another1")

	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "anya@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "boss@delpapa.kz", "another1"); err != nil {
		t.Fatal(err)
	}

	l := f.links.rows[tgAnya]
	if l == nil || l.UserID != b.ID || !l.Active() {
		t.Fatalf("link = %+v, want an active link to B (%s)", l, b.ID)
	}
	if len(f.links.rows) != 1 {
		t.Fatalf("%d rows, want one (a Telegram account points at one person)", len(f.links.rows))
	}
	for _, tok := range f.refr.byHash {
		if tok.UserID == a.ID && tok.RevokedAt == nil {
			t.Fatal("A's refresh token survived B taking over the phone")
		}
	}
	// B's own session, issued by the same call, must obviously still work.
	live := false
	for _, tok := range f.refr.byHash {
		if tok.UserID == b.ID && tok.RevokedAt == nil {
			live = true
		}
	}
	if !live {
		t.Fatal("B was signed out by their own sign-in")
	}
}

// --- sign-out (criterion 11) ------------------------------------------------

func TestUnlinkRevokesTheLinkAndTheTokens(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}

	if err := f.uc.Unlink(context.Background(), f.good(tgAnya), uuid.Nil); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if l := f.links.rows[tgAnya]; l.Active() {
		t.Fatal("the link is still active after signing out")
	}
	for _, tok := range f.refr.byHash {
		if tok.UserID == u.ID && tok.RevokedAt == nil {
			t.Fatal("a refresh token survived the sign-out")
		}
	}
}

// Signing out must never fail: an unknown link is the outcome the caller wanted.
func TestUnlinkOfAnUnknownLinkSucceeds(t *testing.T) {
	f := newMiniApp(t)
	if err := f.uc.Unlink(context.Background(), f.good(tgAnya), uuid.Nil); err != nil {
		t.Fatalf("unlink of an unknown link failed: %v", err)
	}
}

// A captured blob must not let one employee sign a colleague out.
func TestUnlinkRefusesWhenTheBearerIsNotTheLinkOwner(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "anya@delpapa.kz", "s3cret123")
	other := f.staff(t, "boss@delpapa.kz", "another1")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "anya@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}

	err := f.uc.Unlink(context.Background(), f.good(tgAnya), other.ID)
	assertCode(t, err, domain.CodeForbidden, domain.ErrForbidden)
	if l := f.links.rows[tgAnya]; !l.Active() {
		t.Fatal("a stranger's bearer revoked the link anyway")
	}
}

// Bearer-only sign-out (no initData): every device of the account goes.
func TestUnlinkByBearerRevokesEveryDevice(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	for _, tg := range []int64{tgAnya, tgBoss} {
		if _, err := f.uc.Link(context.Background(), f.good(tg), "hostess@delpapa.kz", "s3cret123"); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.uc.Unlink(context.Background(), "", u.ID); err != nil {
		t.Fatalf("bearer-only unlink: %v", err)
	}
	for tg, l := range f.links.rows {
		if l.Active() {
			t.Fatalf("device %d is still linked", tg)
		}
	}
}

func TestUnlinkWithNeitherProofIsAValidationError(t *testing.T) {
	f := newMiniApp(t)
	err := f.uc.Unlink(context.Background(), "", uuid.Nil)
	assertCode(t, err, domain.CodeValidation, domain.ErrValidation)
}

// --- robustness -------------------------------------------------------------

// A failing telemetry write must not cost a hostess her shift screen.
func TestSignInSurvivesAFailingLastSeenWrite(t *testing.T) {
	f := newMiniApp(t)
	f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}
	f.links.touchErr = errors.New("db hiccup")

	if _, err := f.uc.SignIn(context.Background(), f.good(tgAnya)); err != nil {
		t.Fatalf("a failed last_seen write broke the sign-in: %v", err)
	}
}

// A deactivated account is refused, and refused the same way a wrong password
// is — the mini app must not be the place that tells them apart.
func TestLinkRefusesADeactivatedAccount(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	u.IsActive = false
	if err := f.users.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	_, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123")
	assertCode(t, err, domain.CodeInvalidCredentials, domain.ErrUnauthorized)
}

func TestSignInRefusesADeactivatedAccountAndRevokes(t *testing.T) {
	f := newMiniApp(t)
	u := f.staff(t, "hostess@delpapa.kz", "s3cret123")
	if _, err := f.uc.Link(context.Background(), f.good(tgAnya), "hostess@delpapa.kz", "s3cret123"); err != nil {
		t.Fatal(err)
	}
	u.IsActive = false
	if err := f.users.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	_, err := f.uc.SignIn(context.Background(), f.good(tgAnya))
	assertCode(t, err, domain.CodeStaffNotFound, domain.ErrForbidden)
	if f.links.rows[tgAnya].Active() {
		t.Fatal("the link survived the account being deactivated")
	}
}

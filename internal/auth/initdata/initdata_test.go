package initdata

import (
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const (
	ourBot     = "7654321:AAH-our-restaurants-bot-token"
	foreignBot = "1234567:AAF-somebody-elses-bot-token"
)

// blob builds a genuinely signed initData string for the given bot, so the
// negative tests differ from the positive one by exactly the thing under test
// and not by being malformed.
func blob(t *testing.T, botToken string, authDate time.Time, extra map[string]string) string {
	t.Helper()
	v := url.Values{}
	v.Set("user", `{"id":4242,"first_name":"Аня","last_name":"Х","username":"anya"}`)
	v.Set("auth_date", formatUnix(authDate))
	v.Set("query_id", "AAHdF6IQAAAAAN0XohDhrOrc")
	for k, val := range extra {
		if val == "" {
			v.Del(k)
			continue
		}
		v.Set(k, val)
	}
	v.Set("hash", Sign(v, botToken))
	return v.Encode()
}

func formatUnix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestVerifyAcceptsAGenuineBlob(t *testing.T) {
	now := time.Now()
	d, err := Verify(blob(t, ourBot, now, nil), ourBot, time.Hour, now)
	if err != nil {
		t.Fatalf("genuine blob rejected: %v", err)
	}
	if d.User.ID != 4242 {
		t.Fatalf("user id = %d, want 4242", d.User.ID)
	}
	if d.User.FirstName != "Аня" {
		t.Fatalf("first name = %q — the percent-encoded JSON did not survive decoding", d.User.FirstName)
	}
	if d.QueryID != "AAHdF6IQAAAAAN0XohDhrOrc" {
		t.Fatalf("query id = %q", d.QueryID)
	}
}

// The single most important test in the package: a blob signed by ANOTHER bot
// must not open our mini app. Both signatures are real — only the key differs.
func TestVerifyRejectsAnotherBotsSignature(t *testing.T) {
	now := time.Now()
	_, err := Verify(blob(t, foreignBot, now, nil), ourBot, time.Hour, now)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a foreign bot's signature was accepted (err = %v)", err)
	}
}

// Tampering with the payload after signing — the "log in as somebody else"
// attack the whole verification exists to stop.
func TestVerifyRejectsATamperedUserID(t *testing.T) {
	now := time.Now()
	raw := blob(t, ourBot, now, nil)
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	v.Set("user", `{"id":9999,"first_name":"Не Аня"}`) // hash left as it was
	if _, err := Verify(v.Encode(), ourBot, time.Hour, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an edited user id was accepted (err = %v)", err)
	}
}

func TestVerifyRejectsATamperedHash(t *testing.T) {
	now := time.Now()
	v, _ := url.ParseQuery(blob(t, ourBot, now, nil))
	h := []byte(v.Get("hash"))
	// Flip one hex digit: the closest a guess can get and still be wrong.
	if h[0] == 'a' {
		h[0] = 'b'
	} else {
		h[0] = 'a'
	}
	v.Set("hash", string(h))
	if _, err := Verify(v.Encode(), ourBot, time.Hour, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a one-digit-off hash was accepted (err = %v)", err)
	}
}

func TestVerifyRejectsAStaleBlob(t *testing.T) {
	now := time.Now()
	raw := blob(t, ourBot, now.Add(-2*time.Hour), nil)
	_, err := Verify(raw, ourBot, time.Hour, now)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("a two-hour-old blob was accepted with a one-hour TTL (err = %v)", err)
	}
	// Same blob, wider window: proves the rejection was the age and not the
	// signature.
	if _, err := Verify(raw, ourBot, 3*time.Hour, now); err != nil {
		t.Fatalf("the same blob failed inside a wider window: %v", err)
	}
}

func TestVerifyRejectsABlobFromTheFuture(t *testing.T) {
	now := time.Now()
	if _, err := Verify(blob(t, ourBot, now.Add(10*time.Minute), nil), ourBot, time.Hour, now); !errors.Is(err, ErrExpired) {
		t.Fatalf("an auth_date ten minutes ahead was accepted (err = %v)", err)
	}
	// A few seconds ahead is ordinary clock drift between us and Telegram, and
	// must NOT be treated as an attack.
	if _, err := Verify(blob(t, ourBot, now.Add(20*time.Second), nil), ourBot, time.Hour, now); err != nil {
		t.Fatalf("twenty seconds of clock skew was rejected: %v", err)
	}
}

func TestVerifyRejectsMissingPieces(t *testing.T) {
	now := time.Now()
	cases := map[string]string{
		"empty":                     "",
		"no hash":                   "user=%7B%22id%22%3A1%7D&auth_date=1756000000",
		"not a query string":        "%zz",
		"hash but nothing to check": "hash=deadbeef",
	}
	for name, raw := range cases {
		if _, err := Verify(raw, ourBot, time.Hour, now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: accepted (err = %v)", name, err)
		}
	}
	// A blob with no `user` is a signed bot context, not a person signing in.
	if _, err := Verify(blob(t, ourBot, now, map[string]string{"user": ""}), ourBot, time.Hour, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a blob naming no user was accepted (err = %v)", err)
	}
}

// Without a bot token there is nothing to check a signature against, and
// "accept it anyway" would be a sign-in as anybody.
func TestVerifyRefusesWithoutABotToken(t *testing.T) {
	now := time.Now()
	if _, err := Verify(blob(t, ourBot, now, nil), "", time.Hour, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("verified against an empty bot token (err = %v)", err)
	}
}

// Telegram's newer third-party field is excluded from the HMAC check string;
// leaving it in would fail every blob a current client sends.
func TestVerifyIgnoresTheSignatureField(t *testing.T) {
	now := time.Now()
	v, _ := url.ParseQuery(blob(t, ourBot, now, nil))
	v.Set("signature", "some-ed25519-thing")
	if _, err := Verify(v.Encode(), ourBot, time.Hour, now); err != nil {
		t.Fatalf("a blob carrying `signature` was rejected: %v", err)
	}
}

func TestVerifyCarriesTheStartParam(t *testing.T) {
	now := time.Now()
	raw := blob(t, ourBot, now, map[string]string{"start_param": "b_7f3c"})
	d, err := Verify(raw, ourBot, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.StartParam != "b_7f3c" {
		t.Fatalf("start_param = %q, want b_7f3c — the alert's «Открыть» button lands nowhere without it", d.StartParam)
	}
}

// Package initdata verifies the signed blob Telegram hands a Mini App on open
// (`Telegram.WebApp.initData`) and extracts the user it names.
//
// # What this proves, and what it does not
//
// A verified blob proves exactly one thing: this request came from a Mini App
// opened inside OUR bot, and Telegram states that the person who opened it is
// user N. It proves NOTHING about that person being staff of a venue — an
// unverified initData is just a number in a request body, and treating it as a
// credential would turn "sign in without a password" into "sign in as anyone".
// That is why the mini app's first sign-in still costs an email and a password
// (spec §5.2 D); initData is the device identifier the password is pinned to,
// never the secret itself.
//
// The algorithm is Telegram's, unchanged:
//
//	data_check_string = the fields except `hash` and `signature`, sorted by key,
//	                    joined as "key=value" with \n
//	secret_key        = HMAC_SHA256(key="WebAppData", data=<bot token>)
//	expected          = hex(HMAC_SHA256(key=secret_key, data=data_check_string))
//
// The bot token never leaves this package and is never logged.
package initdata

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Sentinel failures. They are separate values rather than one error because the
// mini app reacts differently to each: an expired blob is fixed by reopening the
// app from the bot, an invalid one never is.
var (
	// ErrInvalid — the blob is malformed, has no hash, names no user, or its
	// signature does not match the bot token. All one error on purpose: the
	// caller learns nothing useful from the difference and an attacker would.
	ErrInvalid = errors.New("init data invalid")
	// ErrExpired — the signature verified, auth_date did not.
	ErrExpired = errors.New("init data expired")
)

// User is the part of initData's `user` object the mini app needs. Telegram
// sends more; the rest is ignored rather than parsed, because every field
// decoded from an untrusted body is a field that can be got wrong.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Data is a verified initData blob.
type Data struct {
	User     User
	AuthDate time.Time
	// ChatInstance / QueryID are carried through for the caller's logs; nothing
	// is authorized on them.
	QueryID string
	// StartParam is the `startapp` payload — how an alert's "Открыть" button
	// asks the mini app to land straight on one booking (spec §5.2 F).
	StartParam string
}

// futureSkew is how far ahead of our clock an auth_date may sit before it is
// rejected. Telegram stamps auth_date on its own servers, so a small forward
// difference is ordinary clock drift and not an attack; anything larger is a
// clock nobody should be trusting.
const futureSkew = time.Minute

// Verify checks the signature of raw against botToken and the freshness of
// auth_date against ttl, and returns the verified contents.
//
// Order matters and is not negotiable: the signature is checked BEFORE
// auth_date, so an unsigned blob can never spend our time on anything else, and
// before the `user` object is decoded, so untrusted JSON is only parsed after we
// know Telegram wrote it. A ttl of zero or less disables the freshness check
// (test and local use only — in production an unbounded window means a blob
// captured from a log stays a valid key forever).
//
// The comparison of the two hashes is constant-time: a byte-at-a-time compare
// leaks, through timing, how much of a guessed hash is correct, and a hash that
// can be guessed is a sign-in that can be forged.
func Verify(raw, botToken string, ttl time.Duration, now time.Time) (*Data, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(botToken) == "" {
		return nil, ErrInvalid
	}
	// ParseQuery, not Parse: initData is a query STRING, and its `user` value is
	// percent-encoded JSON that must come back out decoded.
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	got := values.Get("hash")
	if got == "" {
		return nil, ErrInvalid
	}

	pairs := make([]string, 0, len(values))
	for k, vs := range values {
		// `hash` is the signature itself. `signature` is Telegram's newer
		// third-party Ed25519 field, excluded from the HMAC check string by the
		// same spec — leaving it in would make every blob from a current client
		// fail to verify.
		if k == "hash" || k == "signature" {
			continue
		}
		// A duplicated key has no defined check string, so it is not a blob we
		// can verify. Refuse rather than pick one and hope.
		if len(vs) != 1 {
			return nil, ErrInvalid
		}
		pairs = append(pairs, k+"="+vs[0])
	}
	if len(pairs) == 0 {
		return nil, ErrInvalid
	}
	sort.Strings(pairs)
	checkString := strings.Join(pairs, "\n")

	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	want := hex.EncodeToString(hmacSHA256(secret, []byte(checkString)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return nil, ErrInvalid
	}

	// Signature good from here on: the fields are Telegram's words, not the
	// caller's.
	authDateRaw := values.Get("auth_date")
	if authDateRaw == "" {
		return nil, ErrInvalid
	}
	var authUnix int64
	if _, err := fmt.Sscanf(authDateRaw, "%d", &authUnix); err != nil {
		return nil, ErrInvalid
	}
	authDate := time.Unix(authUnix, 0).UTC()
	if authDate.After(now.Add(futureSkew)) {
		return nil, ErrExpired
	}
	if ttl > 0 && now.Sub(authDate) > ttl {
		return nil, ErrExpired
	}

	var u User
	if err := json.Unmarshal([]byte(values.Get("user")), &u); err != nil || u.ID == 0 {
		// A signed blob with no user is a bot-to-bot context (an inline query,
		// an attachment menu opened from a channel), not a person signing in.
		return nil, ErrInvalid
	}

	return &Data{
		User:       u,
		AuthDate:   authDate,
		QueryID:    values.Get("query_id"),
		StartParam: values.Get("start_param"),
	}, nil
}

// Sign produces the `hash` for a set of initData fields. It exists so tests can
// build genuine blobs — including one signed by a DIFFERENT bot token, which is
// the only honest way to test that a foreign signature is rejected.
func Sign(values url.Values, botToken string) string {
	pairs := make([]string, 0, len(values))
	for k, vs := range values {
		if k == "hash" || k == "signature" {
			continue
		}
		for _, v := range vs {
			pairs = append(pairs, k+"="+v)
		}
	}
	sort.Strings(pairs)
	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	return hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(pairs, "\n"))))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

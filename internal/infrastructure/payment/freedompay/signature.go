package freedompay

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// Signature rules, verbatim from https://docs.freedompay.kz (Gateway API → Sync
// API → Overview, "Signature Formation", fetched 2026-07-21):
//
//	Every message in both directions is signed. Concatenate, separated by ';':
//	  1. the name of the called script (from the last '/' to the end, or to '?');
//	  2. all message fields in ALPHABETICAL ORDER by field name, including the
//	     random pg_salt;
//	  3. the secret_key known only to the merchant and FreedomPay.
//	MD5 the result and put the 32-character lowercase hex digest in pg_sig.
//
// pg_sig itself never participates. Fields with the same name are taken in the
// order they appear. The rule applies recursively to nested XML tags.
//
// TODO(verify): nested / repeated parameters. Our requests are flat, and the
// callbacks we act on (result_url) are flat too, but FreedomPay can add array
// parameters such as pg_receipt_positions[0][name] for fiscalised merchants.
// This implementation sorts by the FULL flattened key ("a[b]"), which is what
// the PHP reference implementation ends up doing for POST arrays — confirm on
// the sandbox with a receipt-carrying callback before enabling fiscalisation.
const sigParam = "pg_sig"
const saltParam = "pg_salt"

// scriptName extracts the signature's first component from a path or URL:
// everything after the last '/' and before any '?'.
//
// This is the detail that breaks integrations: for a CALLBACK the script name
// is the last segment of OUR result_url, not of a FreedomPay endpoint. See
// Config.ResultScriptName.
func scriptName(pathOrURL string) string {
	s := pathOrURL
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// sign computes pg_sig for a set of parameters. params must NOT contain pg_sig;
// it is ignored if present.
func sign(script string, params url.Values, secret string) string {
	parts := make([]string, 0, len(params)+2)
	parts = append(parts, script)
	for _, v := range sortedValues(params) {
		parts = append(parts, v)
	}
	parts = append(parts, secret)
	sum := md5.Sum([]byte(strings.Join(parts, ";")))
	return hex.EncodeToString(sum[:])
}

// sortedValues flattens params into signature order: keys alphabetically,
// values of a repeated key in their original order.
func sortedValues(params url.Values) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == sigParam {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, params[k]...)
	}
	return out
}

// verify checks a signed message in constant time.
//
// Constant-time comparison of an MD5 digest is not about the digest's strength
// — it is about not leaking, byte by byte, how close a forged signature is.
// A missing signature is a failure, never a pass.
func verify(script string, params url.Values, secret string) bool {
	got := strings.TrimSpace(params.Get(sigParam))
	if got == "" {
		return false
	}
	want := sign(script, params, secret)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) == 1
}

// newSalt returns the random pg_salt required in every signed message.
func newSalt() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever does,
		// a predictable salt is still better than an unsigned request being
		// sent — and the signature's security does not rest on the salt.
		return "0000000000000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

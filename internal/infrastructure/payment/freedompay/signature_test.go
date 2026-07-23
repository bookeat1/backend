package freedompay

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func TestScriptName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/init_payment", "init_payment"},
		{"/g2g/status_v2", "status_v2"},
		{"/g2g/clearing", "clearing"},
		{"https://api.freedompay.kz/g2g/refund", "refund"},
		{"https://bookeat.kz/webhooks/payments/freedompay", "freedompay"},
		{"https://bookeat.kz/webhooks/payments/freedompay?x=1", "freedompay"},
		{"/webhooks/payments/freedompay/", "freedompay"},
		{"freedompay", "freedompay"},
	}
	for _, tc := range tests {
		if got := scriptName(tc.in); got != tc.want {
			t.Errorf("scriptName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The signature is "script;<values of fields sorted by name>;secret", MD5 hex.
// This test pins the exact concatenation, because getting the order or the
// separators wrong is the single most common way this integration breaks.
func TestSignMatchesTheDocumentedConcatenation(t *testing.T) {
	params := url.Values{}
	params.Set("pg_order_id", "42")
	params.Set("pg_merchant_id", "123234")
	params.Set("pg_amount", "5000.00")
	params.Set("pg_salt", "saltsalt")

	// alphabetical by key: pg_amount, pg_merchant_id, pg_order_id, pg_salt
	want := md5.Sum([]byte(strings.Join([]string{
		"init_payment", "5000.00", "123234", "42", "saltsalt", "secret-key",
	}, ";")))

	got := sign("init_payment", params, "secret-key")
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("sign = %q, want %q", got, hex.EncodeToString(want[:]))
	}
	if len(got) != 32 || strings.ToLower(got) != got {
		t.Errorf("pg_sig must be a 32-character lowercase hex digest, got %q", got)
	}
}

// pg_sig must not sign itself, otherwise verification could never reproduce it.
func TestSignIgnoresPgSig(t *testing.T) {
	params := url.Values{}
	params.Set("pg_order_id", "42")
	params.Set("pg_salt", "saltsalt")

	without := sign("init_payment", params, "k")
	params.Set(sigParam, "deadbeef")
	with := sign("init_payment", params, "k")

	if without != with {
		t.Error("pg_sig must not participate in its own computation")
	}
}

func TestVerify(t *testing.T) {
	secret := "secret-key"
	params := url.Values{}
	params.Set("pg_order_id", "42")
	params.Set("pg_payment_id", "7777777777")
	params.Set("pg_salt", "saltsalt")

	t.Run("valid", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("freedompay", p, secret))
		if !verify("freedompay", p, secret) {
			t.Error("a correctly signed message must verify")
		}
	})

	t.Run("uppercase digest still verifies", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, strings.ToUpper(sign("freedompay", p, secret)))
		if !verify("freedompay", p, secret) {
			t.Error("an uppercase hex digest must verify")
		}
	})

	t.Run("missing pg_sig", func(t *testing.T) {
		if verify("freedompay", cloneValues(params), secret) {
			t.Error("an unsigned message must never verify")
		}
	})

	t.Run("empty pg_sig", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, "   ")
		if verify("freedompay", p, secret) {
			t.Error("an empty signature must never verify")
		}
	})

	t.Run("forged pg_sig", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, "00000000000000000000000000000000")
		if verify("freedompay", p, secret) {
			t.Error("a forged signature must not verify")
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("freedompay", p, "not-the-secret"))
		if verify("freedompay", p, secret) {
			t.Error("a signature made with another key must not verify")
		}
	})

	t.Run("wrong script name", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("some_other_script", p, secret))
		if verify("freedompay", p, secret) {
			t.Error("the script name is part of the signature")
		}
	})

	t.Run("field added after signing", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("freedompay", p, secret))
		p.Set("pg_amount", "999999.00")
		if verify("freedompay", p, secret) {
			t.Error("an extra field must invalidate the signature")
		}
	})

	t.Run("field removed after signing", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("freedompay", p, secret))
		p.Del("pg_payment_id")
		if verify("freedompay", p, secret) {
			t.Error("a removed field must invalidate the signature")
		}
	})

	t.Run("value changed after signing", func(t *testing.T) {
		p := cloneValues(params)
		p.Set(sigParam, sign("freedompay", p, secret))
		p.Set("pg_order_id", "43")
		if verify("freedompay", p, secret) {
			t.Error("a tampered value must invalidate the signature")
		}
	})
}

// Repeated keys keep their order within the key, and keys are ordered
// alphabetically between each other.
func TestSortedValuesOrdering(t *testing.T) {
	p := url.Values{}
	p["b"] = []string{"b1", "b2"}
	p["a"] = []string{"a1"}
	p["c"] = []string{"c1"}
	p[sigParam] = []string{"ignored"}

	got := sortedValues(p)
	want := []string{"a1", "b1", "b2", "c1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNewSaltIsRandomAndHex(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s := newSalt()
		if len(s) != 32 {
			t.Fatalf("salt %q has length %d, want 32", s, len(s))
		}
		if _, err := hex.DecodeString(s); err != nil {
			t.Fatalf("salt %q is not hex", s)
		}
		if seen[s] {
			t.Fatalf("salt %q repeated", s)
		}
		seen[s] = true
	}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		for _, s := range vs {
			out.Add(k, s)
		}
	}
	return out
}

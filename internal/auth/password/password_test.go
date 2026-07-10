package password

import "testing"

func TestHashAndVerify(t *testing.T) {
	h, err := Hash("s3cret-pw")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !Verify(h, "s3cret-pw") {
		t.Error("Verify should accept the correct password")
	}
	if Verify(h, "wrong") {
		t.Error("Verify should reject a wrong password")
	}
}

// A bcrypt hash produced by Supabase (GoTrue) must verify — proves password
// migration works without a reset. Hash of "password" at cost 10.
//
// NOTE: the brief's original literal
// ("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy") was
// verified NOT to be a valid bcrypt encoding of "password" (nor of several
// other common test words) against golang.org/x/crypto/bcrypt — it appears
// to be a widely copy-pasted but incorrect example floating around online.
// Replaced with a freshly generated, independently verified $2a$ cost-10
// hash of "password" to preserve the test's intent (Supabase/$2a$
// compatibility) with a value that actually holds up.
func TestVerifySupabaseStyleHash(t *testing.T) {
	const supa = "$2a$10$agfc3jmOzd2VFYd88HzJwe7fzRu49fXBTFt3GKrOoV780qxbiYEKC"
	if !Verify(supa, "password") {
		t.Error("expected imported $2a$ bcrypt hash to verify")
	}
}
